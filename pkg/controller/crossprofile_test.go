package controller

import (
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
)

func TestCrossDstKey(t *testing.T) {
	cases := []struct {
		srcPrefix, dstPrefix, key, want string
	}{
		// A file at the source root of the copy.
		{"backups/", "restore/", "backups/a.txt", "restore/a.txt"},
		// A folder's objects keep the folder name under the destination.
		{"backups/", "restore/", "backups/photos/2024/img.jpg", "restore/photos/2024/img.jpg"},
		// Copying from the bucket root.
		{"", "mirror/", "a/b.txt", "mirror/a/b.txt"},
		// Into the destination bucket root.
		{"deep/path/", "", "deep/path/x.bin", "x.bin"},
		// A key outside the source prefix passes through un-stripped — the
		// planner never produces this, but the function must not corrupt it.
		{"other/", "dst/", "elsewhere/f", "dst/elsewhere/f"},
	}
	for _, c := range cases {
		if got := crossDstKey(c.srcPrefix, c.dstPrefix, c.key); got != c.want {
			t.Errorf("crossDstKey(%q, %q, %q) = %q, want %q", c.srcPrefix, c.dstPrefix, c.key, got, c.want)
		}
	}
}

// Every client the app builds goes through modelConfigFor, so a profile that
// delegates to an AWS named profile is checked, opened and copied from with
// the same credentials. A hand-rolled config somewhere in that set silently
// falls back to the default credential chain.
func TestModelConfigForCarriesTheProfileOptions(t *testing.T) {
	region := "eu-west-1"
	p := &cfg.Config{
		Name:         "prod",
		BaseUrl:      "https://s3.example",
		Region:       &region,
		AWSProfile:   "sso-prod",
		IgnoreSsl:    true,
		NoMimeDetect: true,
		SSE:          "aws:kms",
		SSEKMSKeyID:  "key-1",
		ChecksumAlgo: "CRC32C",
	}
	mc := modelConfigFor(p)

	if mc.AWSProfile != "sso-prod" {
		t.Errorf("AWSProfile = %q, want the delegated profile", mc.AWSProfile)
	}
	if mc.SSl {
		t.Error("IgnoreSsl was not carried across")
	}
	if mc.Write.DetectMime {
		t.Error("NoMimeDetect was not carried across")
	}
	if mc.Write.SSE != "aws:kms" || mc.Write.SSEKMSKeyID != "key-1" || mc.Write.ChecksumAlgorithm != "CRC32C" {
		t.Errorf("write options = %+v, want the profile's", mc.Write)
	}
	// The details pane and the profile check both read this; it is the one
	// visible sign that the delegated credentials are in force.
	if got := mc.CredentialSource(); got != "aws profile sso-prod (SDK-resolved, auto-refreshing)" {
		t.Errorf("CredentialSource = %q", got)
	}
}
