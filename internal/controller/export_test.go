package controller

import (
	"strings"
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
)

func minioProfile() *cfg.Config {
	region := "us-east-1"
	return &cfg.Config{
		Name: "home minio", BaseUrl: "https://minio.lan:9000/", Region: &region,
		AccessKey: "AKIDEXAMPLE", SecretKey: "s3cr3t-value", SessionToken: "tok-value", IgnoreSsl: true,
	}
}

func TestExportNeverContainsSecrets(t *testing.T) {
	p := minioProfile()
	for _, f := range exportFormats {
		out := exportProfile(p, f)
		if strings.Contains(out, p.SecretKey) || strings.Contains(out, p.SessionToken) {
			t.Errorf("%s export leaks a secret:\n%s", f, out)
		}
		if !strings.Contains(out, "AKIDEXAMPLE") {
			t.Errorf("%s export should name the access key ID, so the right secret can be filled in:\n%s", f, out)
		}
	}
}

func TestExportAWSCustomEndpoint(t *testing.T) {
	out := exportProfile(minioProfile(), exportAWS)
	for _, want := range []string{
		"[profile home-minio]",
		"region = us-east-1",
		"endpoint_url = https://minio.lan:9000\n",
		"addressing_style = path",
		"[home-minio]\naws_access_key_id = AKIDEXAMPLE",
		"aws_session_token = " + sessionPlaceholder,
		"--no-verify-ssl",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("AWS export lacks %q:\n%s", want, out)
		}
	}
}

func TestExportAWSOnAWSLeavesTheEndpointOut(t *testing.T) {
	region := "eu-central-1"
	p := &cfg.Config{Name: "prod", BaseUrl: "https://s3.eu-central-1.amazonaws.com", Region: &region, AccessKey: "AKID"}
	out := exportProfile(p, exportAWS)
	if strings.Contains(out, "endpoint_url") || strings.Contains(out, "addressing_style") {
		t.Errorf("an AWS endpoint should be left to the CLI:\n%s", out)
	}
	if strings.Contains(out, "aws_session_token") {
		t.Error("no session token, no placeholder for one")
	}
}

func TestExportDelegatingProfile(t *testing.T) {
	p := &cfg.Config{Name: "dev", BaseUrl: "https://minio.lan", AWSProfile: "sso-dev"}
	aws := exportProfile(p, exportAWS)
	if !strings.Contains(aws, "[profile sso-dev]") || strings.Contains(aws, "aws_access_key_id") {
		t.Errorf("a delegating profile adds the endpoint to its own ~/.aws profile:\n%s", aws)
	}
	rc := exportProfile(p, exportRclone)
	if !strings.Contains(rc, "env_auth = true\nprofile = sso-dev") || strings.Contains(rc, "access_key_id") {
		t.Errorf("rclone resolves the profile itself:\n%s", rc)
	}
	mc := exportProfile(p, exportMC)
	if !strings.Contains(mc, "cannot read") || !strings.Contains(mc, "'<access key id>'") {
		t.Errorf("mc cannot delegate, and must say so:\n%s", mc)
	}
}

func TestExportMCQuotesAndFlags(t *testing.T) {
	out := exportProfile(minioProfile(), exportMC)
	if !strings.Contains(out, "mc alias set home-minio https://minio.lan:9000 AKIDEXAMPLE '"+secretPlaceholder+"' --api S3v4 --path on") {
		t.Errorf("mc command:\n%s", out)
	}
	if !strings.Contains(out, "--insecure") || !strings.Contains(out, "MC_HOST_") {
		t.Errorf("mc notes on TLS and session tokens are missing:\n%s", out)
	}
}

func TestExportRcloneCustomEndpoint(t *testing.T) {
	out := exportProfile(minioProfile(), exportRclone)
	for _, want := range []string{"[home-minio]", "type = s3", "provider = Other", "endpoint = https://minio.lan:9000\n", "force_path_style = true", "--no-check-certificate"} {
		if !strings.Contains(out, want) {
			t.Errorf("rclone export lacks %q:\n%s", want, out)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":         "plain",
		"https://h:9/x": "https://h:9/x",
		"has space":     "'has space'",
		"it's":          `'it'\''s'`,
		"":              "''",
		"$(rm -rf ~)":   "'$(rm -rf ~)'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportNameAndFileName(t *testing.T) {
	if got := exportName(` a [b]/c "d" `); got != "a--b--c--d-" {
		t.Errorf("exportName = %q", got)
	}
	if got := exportFileName(&cfg.Config{Name: "x y"}, exportRclone); got != "s3duck-x-y.rclone.txt" {
		t.Errorf("exportFileName = %q", got)
	}
}
