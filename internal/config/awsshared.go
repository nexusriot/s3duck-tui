package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AWSProfile is one profile read from the AWS shared credentials/config files.
// Only the fields s3duck needs are kept; role-assumption and SSO profiles carry
// no static keys and are reported through Err instead.
type AWSProfile struct {
	Name         string
	AccessKey    string
	SecretKey    string
	SessionToken string
	Region       string
	// Err explains why a profile can't be imported as-is (e.g. it delegates to
	// source_profile or sso_session, which s3duck cannot resolve itself).
	Err string
}

// Usable reports whether the profile carries credentials s3duck can use.
func (p AWSProfile) Usable() bool {
	return p.Err == "" && p.AccessKey != "" && p.SecretKey != ""
}

// EndpointURL returns the regional S3 endpoint for the profile's region,
// falling back to the global endpoint when no region is set.
func (p AWSProfile) EndpointURL() string {
	if p.Region == "" {
		return "https://s3.amazonaws.com"
	}
	return fmt.Sprintf("https://s3.%s.amazonaws.com", p.Region)
}

// parseINI parses the minimal INI dialect the AWS credential files use:
// "[section]" headers, "key = value" pairs, "#"/";" comments, and blank lines.
// Keys are lowercased and values trimmed. Later duplicate keys win, matching
// the SDK. Nested (indented) sub-properties such as those under
// "sso_session" are ignored — they carry no static credentials.
func parseINI(data string) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""

	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := out[section]; !ok {
				out[section] = map[string]string{}
			}
			continue
		}
		if section == "" {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[section][strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

// normalizeProfileSection maps a section name to its profile name. The config
// file prefixes non-default profiles with "profile "; the credentials file does
// not. Sections that aren't profiles (e.g. "sso-session foo") return false.
func normalizeProfileSection(section string) (string, bool) {
	if rest, ok := strings.CutPrefix(section, "profile "); ok {
		name := strings.TrimSpace(rest)
		return name, name != ""
	}
	if strings.Contains(section, " ") {
		return "", false
	}
	return section, section != ""
}

// ParseAWSProfiles merges the two AWS shared files into a profile list, sorted
// by name with "default" first. Credentials come from either file; the
// credentials file wins on conflict, matching the SDK's precedence.
func ParseAWSProfiles(credentialsData, configData string) []AWSProfile {
	merged := map[string]map[string]string{}

	absorb := func(data string, override bool) {
		for section, kv := range parseINI(data) {
			name, ok := normalizeProfileSection(section)
			if !ok {
				continue
			}
			if merged[name] == nil {
				merged[name] = map[string]string{}
			}
			for k, v := range kv {
				if _, exists := merged[name][k]; exists && !override {
					continue
				}
				merged[name][k] = v
			}
		}
	}
	absorb(configData, false)
	absorb(credentialsData, true)

	out := make([]AWSProfile, 0, len(merged))
	for name, kv := range merged {
		p := AWSProfile{
			Name:         name,
			AccessKey:    kv["aws_access_key_id"],
			SecretKey:    kv["aws_secret_access_key"],
			SessionToken: kv["aws_session_token"],
			Region:       kv["region"],
		}
		if p.AccessKey == "" || p.SecretKey == "" {
			switch {
			case kv["sso_session"] != "" || kv["sso_start_url"] != "":
				p.Err = "SSO profile: run `aws sso login`, then import the cached credentials"
			case kv["role_arn"] != "":
				p.Err = "role profile: assume the role first, then import the temporary credentials"
			case kv["credential_process"] != "":
				p.Err = "credential_process profile: not resolvable by s3duck"
			default:
				p.Err = "no aws_access_key_id / aws_secret_access_key"
			}
		}
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name == "default") != (out[j].Name == "default") {
			return out[i].Name == "default"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AWSSharedFiles returns the paths of the AWS credentials and config files,
// honoring AWS_SHARED_CREDENTIALS_FILE / AWS_CONFIG_FILE.
func AWSSharedFiles(homeDir string) (credentials, config string) {
	credentials = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credentials == "" {
		credentials = filepath.Join(homeDir, ".aws", "credentials")
	}
	config = os.Getenv("AWS_CONFIG_FILE")
	if config == "" {
		config = filepath.Join(homeDir, ".aws", "config")
	}
	return credentials, config
}

// LoadAWSProfiles reads and parses the AWS shared files. A missing file is not
// an error (it simply contributes nothing); an error is returned only when both
// files are unreadable for a reason other than absence.
func LoadAWSProfiles(homeDir string) ([]AWSProfile, error) {
	credPath, cfgPath := AWSSharedFiles(homeDir)

	read := func(p string) (string, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				return "", nil
			}
			return "", fmt.Errorf("failed to read %s: %w", p, err)
		}
		return string(b), nil
	}

	credData, credErr := read(credPath)
	cfgData, cfgErr := read(cfgPath)
	if credErr != nil && cfgErr != nil {
		return nil, credErr
	}

	profiles := ParseAWSProfiles(credData, cfgData)
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no AWS profiles found in %s or %s", credPath, cfgPath)
	}
	return profiles, nil
}
