package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

func TestBucketDashboardTextSaysWhyASectionIsEmpty(t *testing.T) {
	info := model.BucketConfigInfo{
		Region:            "eu-west-1",
		Versioning:        model.ConfigItem{State: model.ConfigSet, Value: "Enabled"},
		Encryption:        model.ConfigItem{State: model.ConfigDenied},
		ObjectLock:        model.ConfigItem{State: model.ConfigUnset, Value: "off"},
		PublicAccessBlock: model.ConfigItem{State: model.ConfigUnsupported},
		Tags:              model.ConfigItem{State: model.ConfigSet, Value: "1 tag(s)", Detail: []string{"env=[prod]"}},
		Lifecycle:         model.ConfigItem{State: model.ConfigError, Err: errors.New("timeout")},
	}
	got := rendered(bucketDashboardText("logs", info))
	for _, want := range []string{
		"Region               eu-west-1",
		"Versioning           Enabled",
		"Default encryption   access denied",
		"Object lock          off",
		"Public access block  not supported by this endpoint",
		"    env=[prod]", // a tag value that looks like markup survives
		"Lifecycle            could not read: timeout",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dashboard lacks %q:\n%s", want, got)
		}
	}
	// Policy is last: it is the one section that can run long.
	if strings.Index(got, "Policy") < strings.Index(got, "Access logging") {
		t.Error("the policy should come after every one-line section")
	}
}

func TestValidateBucketTags(t *testing.T) {
	if err := validateBucketTags(map[string]string{"env": "prod"}); err != nil {
		t.Errorf("a plain tag was refused: %v", err)
	}
	if err := validateBucketTags(map[string]string{"aws:cloudformation:stack-name": "x"}); err == nil {
		t.Error("an aws: key must be refused before S3 refuses it")
	}
	many := map[string]string{}
	for i := 0; i <= maxBucketTags; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	if err := validateBucketTags(many); err == nil {
		t.Error("more than 50 tags must be refused")
	}
	// Buckets allow more tags than objects do.
	eleven := map[string]string{}
	for i := 0; i < 11; i++ {
		eleven[strings.Repeat("t", i+1)] = "v"
	}
	if err := validateBucketTags(eleven); err != nil {
		t.Errorf("11 bucket tags are legal: %v", err)
	}
}

func TestSystemTagsAreFound(t *testing.T) {
	got := systemTags(map[string]string{"env": "p", "aws:cloudformation:stack-id": "s", "AWS:x": "y"})
	if len(got) != 2 {
		t.Errorf("systemTags = %v, want both aws: keys whatever their case", got)
	}
}

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
