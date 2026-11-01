package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

func TestVersioningChange(t *testing.T) {
	tests := []struct {
		name       string
		item       model.ConfigItem
		wantEnable bool
		wantPrompt string
		wantErr    string
	}{
		{"never enabled", model.ConfigItem{State: model.ConfigUnset, Value: "off"}, true, "cannot be turned off", ""},
		{"enabled", model.ConfigItem{State: model.ConfigSet, Value: "Enabled"}, false, "Suspend", ""},
		{"suspended", model.ConfigItem{State: model.ConfigSet, Value: "Suspended"}, true, "Re-enable", ""},
		{"mfa delete", model.ConfigItem{State: model.ConfigSet, Value: "Enabled, MFA delete on"}, false, "", "MFA"},
		// Suspending a bucket whose state we merely failed to read is the
		// dangerous guess; it is refused, as is every unreadable state.
		{"denied", model.ConfigItem{State: model.ConfigDenied}, false, "", "access denied"},
		{"unsupported", model.ConfigItem{State: model.ConfigUnsupported}, false, "", "not supported"},
		{"error", model.ConfigItem{State: model.ConfigError, Err: errors.New("boom")}, false, "", "could not be read"},
		{"odd value", model.ConfigItem{State: model.ConfigSet, Value: "Weird"}, false, "", "unrecognised"},
	}
	for _, tt := range tests {
		enable, prompt, err := versioningChange("b", tt.item)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: err = %v, want one mentioning %q", tt.name, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tt.name, err)
			continue
		}
		if enable != tt.wantEnable || !strings.Contains(prompt, tt.wantPrompt) {
			t.Errorf("%s: = (%v, %q), want (%v, …%q…)", tt.name, enable, prompt, tt.wantEnable, tt.wantPrompt)
		}
	}
}

func TestAnonymousOptionsRoundTrip(t *testing.T) {
	p := &cfg.Config{Name: "pub"}
	o := optionsFromConfig(p)
	o.Anonymous = true
	o.PublicBuckets = " open-data, noaa ,,open-data;\nlandsat "
	if err := validateProfileOptions(o); err != nil {
		t.Fatalf("validate: %v", err)
	}
	applyOptionsToConfig(p, o)
	if !p.Anonymous {
		t.Error("Anonymous not stored")
	}
	if got := strings.Join(p.PublicBuckets, ","); got != "open-data,noaa,landsat" {
		t.Errorf("PublicBuckets = %v", p.PublicBuckets)
	}
	if back := optionsFromConfig(p); back.PublicBuckets != "open-data, noaa, landsat" || !back.Anonymous {
		t.Errorf("form shows %+v", back)
	}
	if !strings.Contains(optionsSummary(p), "anonymous") {
		t.Errorf("summary = %q", optionsSummary(p))
	}
	if !strings.Contains(identityLabel(p), "[ANONYMOUS]") {
		t.Errorf("identity = %q", identityLabel(p))
	}
}

func TestAnonymousOptionsValidation(t *testing.T) {
	both := widgets.ProfileOptions{Anonymous: true, AWSProfile: "sso"}
	if err := validateProfileOptions(both); err == nil {
		t.Error("anonymous + AWS profile accepted")
	}
	spaced := widgets.ProfileOptions{Anonymous: true, PublicBuckets: "my bucket"}
	if err := validateProfileOptions(spaced); err == nil || !strings.Contains(err.Error(), "commas") {
		t.Errorf("space-separated names: err = %v", err)
	}
	slashed := widgets.ProfileOptions{Anonymous: true, PublicBuckets: "b/prefix"}
	if err := validateProfileOptions(slashed); err == nil {
		t.Error("a bucket with a path accepted")
	}
}

type fakeProber struct {
	fail       map[string]error
	asked      []string
	refreshed  []string
	refreshErr error
}

func (f *fakeProber) RefreshClient(b *string) error {
	f.refreshed = append(f.refreshed, *b)
	return f.refreshErr
}

func (f *fakeProber) HasPrefix(_ context.Context, b *model.Object, _ string) (bool, error) {
	f.asked = append(f.asked, *b.Key)
	return true, f.fail[*b.Key]
}

func TestCheckPublicBuckets(t *testing.T) {
	ok := &fakeProber{}
	n, err := checkPublicBuckets(context.Background(), ok, []string{"a", " b ", "a", ""})
	if err != nil || n != 2 || strings.Join(ok.asked, ",") != "a,b" {
		t.Errorf("all readable: n=%d err=%v asked=%v; want 2 distinct, trimmed", n, err, ok.asked)
	}
	// Each bucket is checked in its own region, as browsing reaches it.
	if strings.Join(ok.refreshed, ",") != "a,b" {
		t.Errorf("regions pinned for %v, want a,b", ok.refreshed)
	}
	bad := &fakeProber{fail: map[string]error{"b": errors.New("AccessDenied")}}
	if _, err := checkPublicBuckets(context.Background(), bad, []string{"a", "b", "c"}); err == nil || !strings.Contains(err.Error(), "public bucket b") {
		t.Errorf("err = %v, want it to name bucket b", err)
	}
	// A region lookup that fails is not the verdict: the listing is.
	noRegion := &fakeProber{refreshErr: errors.New("Forbidden")}
	if n, err := checkPublicBuckets(context.Background(), noRegion, []string{"a"}); err != nil || n != 1 {
		t.Errorf("a failed region lookup failed the check: n=%d err=%v", n, err)
	}
	if _, err := checkPublicBuckets(context.Background(), ok, []string{" ", ""}); !errors.Is(err, model.ErrNoPublicBuckets) {
		t.Errorf("none named: err = %v", err)
	}
}

func TestExportAnonymousProfile(t *testing.T) {
	p := &cfg.Config{Name: "open data", BaseUrl: "https://s3.amazonaws.com", Anonymous: true,
		AccessKey: "AKIASHOULDNOTAPPEAR", PublicBuckets: []string{"noaa-ghcn-pds"}}
	aws := exportProfile(p, exportAWS)
	if !strings.Contains(aws, "--no-sign-request") || !strings.Contains(aws, "s3://noaa-ghcn-pds/") {
		t.Errorf("aws export:\n%s", aws)
	}
	if strings.Contains(aws, "aws_access_key_id") || strings.Contains(aws, "AKIASHOULDNOTAPPEAR") {
		t.Errorf("anonymous aws export carries credentials:\n%s", aws)
	}
	mc := exportProfile(p, exportMC)
	if !strings.Contains(mc, "'' ''") || strings.Contains(mc, "AKIASHOULDNOTAPPEAR") {
		t.Errorf("mc export:\n%s", mc)
	}
	rc := exportProfile(p, exportRclone)
	if strings.Contains(rc, "access_key_id") || !strings.Contains(rc, "env_auth = false") {
		t.Errorf("rclone export:\n%s", rc)
	}
	if got := anonymousExampleBucket(&cfg.Config{}); got != "BUCKET" {
		t.Errorf("example bucket with none named = %q", got)
	}
}
