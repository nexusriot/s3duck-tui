package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sampleCredentials = `
# a comment
[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secret-default

[prod]
aws_access_key_id=AKIAPROD
aws_secret_access_key=secret-prod
aws_session_token = tok-prod

; semicolon comment
[broken]
aws_access_key_id = only-a-key
`

const sampleConfig = `
[default]
region = us-east-1

[profile prod]
region = eu-central-1

[profile ssoish]
sso_session = corp
sso_account_id = 1234

[profile roley]
role_arn = arn:aws:iam::1:role/r
source_profile = default

[sso-session corp]
sso_start_url = https://example.awsapps.com/start
`

func names(ps []AWSProfile) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func find(t *testing.T, ps []AWSProfile, name string) AWSProfile {
	t.Helper()
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("profile %q not found in %v", name, names(ps))
	return AWSProfile{}
}

func TestParseAWSProfiles(t *testing.T) {
	ps := ParseAWSProfiles(sampleCredentials, sampleConfig)

	t.Run("default sorts first, the rest alphabetically", func(t *testing.T) {
		want := []string{"default", "broken", "prod", "roley", "ssoish"}
		if got := names(ps); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("sso-session sections are not profiles", func(t *testing.T) {
		for _, n := range names(ps) {
			if n == "corp" || n == "sso-session corp" {
				t.Errorf("non-profile section leaked in: %v", names(ps))
			}
		}
	})

	t.Run("credentials and config are merged per profile", func(t *testing.T) {
		prod := find(t, ps, "prod")
		if prod.AccessKey != "AKIAPROD" || prod.SecretKey != "secret-prod" {
			t.Errorf("keys not read: %+v", prod)
		}
		if prod.SessionToken != "tok-prod" {
			t.Errorf("session token = %q", prod.SessionToken)
		}
		if prod.Region != "eu-central-1" {
			t.Errorf("region from the config file = %q", prod.Region)
		}
		if !prod.Usable() {
			t.Errorf("prod should be usable: %+v", prod)
		}
	})

	t.Run("values are trimmed with or without spaces around =", func(t *testing.T) {
		if got := find(t, ps, "default").AccessKey; got != "AKIADEFAULT" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("SSO and role profiles are listed with a reason, not silently dropped", func(t *testing.T) {
		sso := find(t, ps, "ssoish")
		if sso.Usable() {
			t.Errorf("SSO profile must not be usable")
		}
		if sso.Err == "" || !strings.Contains(sso.Err, "SSO") {
			t.Errorf("err = %q, want an SSO explanation", sso.Err)
		}

		role := find(t, ps, "roley")
		if role.Usable() || !strings.Contains(role.Err, "role") {
			t.Errorf("role profile: usable=%v err=%q", role.Usable(), role.Err)
		}
	})

	t.Run("a half-filled profile reports the missing keys", func(t *testing.T) {
		b := find(t, ps, "broken")
		if b.Usable() || !strings.Contains(b.Err, "aws_secret_access_key") {
			t.Errorf("usable=%v err=%q", b.Usable(), b.Err)
		}
	})

	t.Run("credentials win over config on conflict", func(t *testing.T) {
		creds := "[p]\naws_access_key_id = FROM-CREDS\naws_secret_access_key = s\n"
		conf := "[profile p]\naws_access_key_id = FROM-CONFIG\naws_secret_access_key = s\n"
		if got := find(t, ParseAWSProfiles(creds, conf), "p").AccessKey; got != "FROM-CREDS" {
			t.Errorf("got %q, want the credentials-file value", got)
		}
	})

	t.Run("empty input yields no profiles", func(t *testing.T) {
		if got := ParseAWSProfiles("", ""); len(got) != 0 {
			t.Errorf("got %v, want none", names(got))
		}
	})
}

func TestAWSProfileEndpointURL(t *testing.T) {
	if got := (AWSProfile{Region: "ap-south-1"}).EndpointURL(); got != "https://s3.ap-south-1.amazonaws.com" {
		t.Errorf("got %q", got)
	}
	if got := (AWSProfile{}).EndpointURL(); got != "https://s3.amazonaws.com" {
		t.Errorf("got %q, want the global endpoint", got)
	}
}

func TestAWSSharedFilesHonorsEnv(t *testing.T) {
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/custom/creds")
	t.Setenv("AWS_CONFIG_FILE", "")

	creds, conf := AWSSharedFiles("/home/u")
	if creds != "/custom/creds" {
		t.Errorf("creds = %q", creds)
	}
	if want := filepath.Join("/home/u", ".aws", "config"); conf != want {
		t.Errorf("conf = %q, want %q", conf, want)
	}
}

func TestLoadAWSProfiles(t *testing.T) {
	home := t.TempDir()
	awsDir := filepath.Join(home, ".aws")
	if err := os.MkdirAll(awsDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "")
	t.Setenv("AWS_CONFIG_FILE", "")

	t.Run("no files at all is an error, not an empty success", func(t *testing.T) {
		if _, err := LoadAWSProfiles(home); err == nil {
			t.Error("want an error when nothing is there")
		}
	})

	t.Run("a credentials file alone is enough", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(awsDir, "credentials"), []byte(sampleCredentials), 0600); err != nil {
			t.Fatal(err)
		}
		ps, err := LoadAWSProfiles(home)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ps) != 3 {
			t.Errorf("got %v, want 3 profiles", names(ps))
		}
	})
}
