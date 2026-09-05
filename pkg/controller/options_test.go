package controller

import (
	"strings"
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

func TestOptionsRoundTrip(t *testing.T) {
	p := &cfg.Config{Name: "p"}
	want := view.ProfileOptions{
		AWSProfile:  "sso-dev",
		ReadOnly:    true,
		Trash:       true,
		TrashPrefix: "trash/",
		DetectMime:  false,
		SSE:         "aws:kms",
		KMSKey:      "key-1",
		Checksum:    "CRC32C",
		Verify:      true,
	}
	applyOptionsToConfig(p, want)
	if got := optionsFromConfig(p); got != want {
		t.Errorf("round trip changed the options:\n got %+v\nwant %+v", got, want)
	}
	// Mime detection is stored inverted so that an old config file — which
	// has no such key — comes back with detection ON, the behaviour being
	// fixed rather than the one being preserved.
	if !p.NoMimeDetect {
		t.Error("DetectMime=false should store NoMimeDetect=true")
	}
	if got := optionsFromConfig(&cfg.Config{}); !got.DetectMime {
		t.Error("a profile predating this field must default to detecting")
	}
}

func TestValidateProfileOptions(t *testing.T) {
	base := view.ProfileOptions{DetectMime: true}

	if err := validateProfileOptions(base); err != nil {
		t.Errorf("the default options should validate: %v", err)
	}
	// A KMS key with no KMS encryption would be silently dropped on every
	// write, so it is refused where the message can still name the field.
	bad := base
	bad.KMSKey = "key-1"
	if err := validateProfileOptions(bad); err == nil {
		t.Error("a KMS key without SSE=aws:kms should be refused")
	}
	ok := base
	ok.SSE, ok.KMSKey = "aws:kms", "key-1"
	if err := validateProfileOptions(ok); err != nil {
		t.Errorf("aws:kms with a key should validate: %v", err)
	}

	bad = base
	bad.Checksum = "MD5" // S3 does not accept MD5 as an additional checksum
	if err := validateProfileOptions(bad); err == nil {
		t.Error("an unsupported checksum algorithm should be refused")
	}

	bad = base
	bad.Trash, bad.TrashPrefix = true, "/absolute"
	if err := validateProfileOptions(bad); err == nil {
		t.Error("an absolute trash prefix should be refused")
	}

	// Verify without a write-side checksum is legal: the ETag still covers
	// single-part objects.
	ok = base
	ok.Verify = true
	if err := validateProfileOptions(ok); err != nil {
		t.Errorf("verify without a checksum algorithm should be allowed: %v", err)
	}
}

func TestOptionsSummary(t *testing.T) {
	p := &cfg.Config{Name: "p", AWSProfile: "dev", ReadOnly: true, Trash: true,
		SSE: "AES256", ChecksumAlgo: "CRC32C", VerifyDownloads: true}
	got := optionsSummary(p)
	for _, want := range []string{"aws profile dev", "read-only", "trash .s3duck-trash/", "sse:AES256", "checksum:CRC32C", "verify downloads"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q is missing %q", got, want)
		}
	}
	// A plain profile should not claim anything it has not been given.
	got = optionsSummary(&cfg.Config{Name: "q"})
	if strings.Contains(got, "read-only") || strings.Contains(got, "sse") {
		t.Errorf("summary of a default profile = %q", got)
	}
	if !strings.Contains(got, "static keys") {
		t.Errorf("summary should name the credential source: %q", got)
	}
}

func TestTrashedPrefix(t *testing.T) {
	if got := (&cfg.Config{}).Trashed(); got != "" {
		t.Errorf("safe delete off should report no prefix, got %q", got)
	}
	if got := (&cfg.Config{Trash: true}).Trashed(); got != cfg.DefaultTrashPrefix {
		t.Errorf("default trash prefix = %q", got)
	}
	// A prefix is a folder, so it is always slash-terminated.
	if got := (&cfg.Config{Trash: true, TrashPrefix: "bin"}).Trashed(); got != "bin/" {
		t.Errorf("trash prefix = %q, want bin/", got)
	}
	var nilConf *cfg.Config
	if got := nilConf.Trashed(); got != "" {
		t.Errorf("a nil profile must not panic: %q", got)
	}
}

func TestIdentityLabel(t *testing.T) {
	region := "eu-west-1"
	p := &cfg.Config{Name: "prod", BaseUrl: "https://s3.example.com/path", Region: &region}
	got := identityLabel(p)
	// Which account is this? — the question the browser screen could not
	// answer before.
	for _, want := range []string{"prod", "s3.example.com", "eu-west-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("identity %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "/path") {
		t.Errorf("identity should reduce the endpoint to its host: %q", got)
	}
	if !strings.Contains(identityLabel(&cfg.Config{Name: "p", ReadOnly: true}), "READ-ONLY") {
		t.Error("a read-only profile must say so in the header")
	}
	if identityLabel(nil) != "" {
		t.Error("no profile means no identity")
	}
}

func TestEndpointHost(t *testing.T) {
	cases := map[string]string{
		"https://s3.example.com":           "s3.example.com",
		"http://localhost:9000/bucket":     "localhost:9000",
		"s3.example.com":                   "s3.example.com",
		"":                                 "aws",
		"https://s3.eu.amazonaws.com/?x=1": "s3.eu.amazonaws.com",
	}
	for in, want := range cases {
		if got := endpointHost(in); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrashKeyMapping(t *testing.T) {
	const trash = ".s3duck-trash/"
	key := trashKeyFor(trash, "20260902-181500", "docs/report.pdf")
	if key != ".s3duck-trash/20260902-181500/docs/report.pdf" {
		t.Fatalf("trashKeyFor = %q", key)
	}
	// The original key must be recoverable, or a restore would have to guess.
	if got := restoreKeyFrom(trash, key); got != "docs/report.pdf" {
		t.Errorf("restoreKeyFrom = %q, want docs/report.pdf", got)
	}
	// Anything not in the trash has no origin to restore to.
	if got := restoreKeyFrom(trash, "docs/report.pdf"); got != "" {
		t.Errorf("restoreKeyFrom outside the trash = %q, want empty", got)
	}
	// A stamp folder with no path below it is not restorable either.
	if got := restoreKeyFrom(trash, ".s3duck-trash/20260902-181500/"); got != "" {
		t.Errorf("restoreKeyFrom(stamp only) = %q, want empty", got)
	}
	if !isTrashed(trash, key) || isTrashed(trash, "docs/x") || isTrashed("", key) {
		t.Error("isTrashed misclassified a key")
	}
}
