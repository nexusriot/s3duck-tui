package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	u "github.com/nexusriot/s3duck-tui/internal/utils"
)

// Profile export: the same connection, written out for the other tools a user
// reaches for — the AWS CLI, MinIO's mc, rclone. Secrets are never exported:
// the secret key and session token become placeholders, so the snippet can be
// pasted into a ticket or a dotfiles repository without leaking anything that
// opens the bucket. The access key ID is kept, as an identifier: it says which
// secret to fill in.

// exportFormat names one target tool.
type exportFormat string

const (
	exportAWS    exportFormat = "AWS CLI"
	exportMC     exportFormat = "mc"
	exportRclone exportFormat = "rclone"
)

// exportFormats is the cycle order of the export viewer's f key.
var exportFormats = []exportFormat{exportAWS, exportMC, exportRclone}

const (
	secretPlaceholder  = "<secret key — not exported>"
	sessionPlaceholder = "<session token — not exported>"
)

// exportName makes a profile name usable as an INI section or an mc alias:
// whitespace and the characters those formats treat specially become dashes.
func exportName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "s3duck"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '\t' || r == '[' || r == ']' || r == '/' || r == '\\' || r == '"' || r == '\'':
			return '-'
		}
		return r
	}, name)
}

// isAWSEndpoint reports a profile aimed at AWS itself, where the tools pick
// the endpoint from the region and an explicit one would only pin it.
func isAWSEndpoint(baseURL string) bool {
	return strings.TrimSpace(baseURL) == "" || strings.Contains(endpointHost(baseURL), "amazonaws.com")
}

// shellQuote quotes a word for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/@%+=,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// profileRegion is the region a profile names, or "" when it leaves it to the
// endpoint.
func profileRegion(p *cfg.Config) string {
	if p.Region == nil {
		return ""
	}
	return strings.TrimSpace(*p.Region)
}

// exportProfile renders p for one tool. Pure: what is written is decided here,
// and tested here.
func exportProfile(p *cfg.Config, f exportFormat) string {
	switch f {
	case exportMC:
		return exportMCAlias(p)
	case exportRclone:
		return exportRcloneRemote(p)
	default:
		return exportAWSProfile(p)
	}
}

func exportAWSProfile(p *cfg.Config) string {
	name := exportName(p.Name)
	region := profileRegion(p)
	custom := !isAWSEndpoint(p.BaseUrl)
	var b strings.Builder

	writeEndpoint := func() {
		if region != "" {
			fmt.Fprintf(&b, "region = %s\n", region)
		}
		if custom {
			// endpoint_url in the config file needs AWS CLI v2.13 or newer.
			// Path-style because that is how this app addresses a custom
			// endpoint (bucket in the path, host left alone).
			fmt.Fprintf(&b, "endpoint_url = %s\n", strings.TrimRight(p.BaseUrl, "/"))
			b.WriteString("s3 =\n  addressing_style = path\n")
		}
	}

	if p.AWSProfile != "" {
		fmt.Fprintf(&b, "# This profile delegates its credentials to the ~/.aws profile %q.\n", p.AWSProfile)
		fmt.Fprintf(&b, "# Add these lines to that profile's section in ~/.aws/config:\n")
		fmt.Fprintf(&b, "[profile %s]\n", p.AWSProfile)
		writeEndpoint()
	} else {
		b.WriteString("# ~/.aws/config\n")
		fmt.Fprintf(&b, "[profile %s]\n", name)
		writeEndpoint()
		b.WriteString("\n# ~/.aws/credentials\n")
		fmt.Fprintf(&b, "[%s]\n", name)
		fmt.Fprintf(&b, "aws_access_key_id = %s\n", p.AccessKey)
		fmt.Fprintf(&b, "aws_secret_access_key = %s\n", secretPlaceholder)
		if p.SessionToken != "" {
			fmt.Fprintf(&b, "aws_session_token = %s\n", sessionPlaceholder)
		}
	}
	if p.IgnoreSsl {
		b.WriteString("\n# This profile skips TLS verification: add --no-verify-ssl to aws commands.\n")
	}
	fmt.Fprintf(&b, "\n# Then: aws s3 ls --profile %s\n", awsCLIProfileName(p))
	return b.String()
}

// awsCLIProfileName is the profile the exported AWS block is reached by.
func awsCLIProfileName(p *cfg.Config) string {
	if p.AWSProfile != "" {
		return p.AWSProfile
	}
	return exportName(p.Name)
}

func exportMCAlias(p *cfg.Config) string {
	var b strings.Builder
	url := strings.TrimRight(p.BaseUrl, "/")
	if strings.TrimSpace(url) == "" {
		url = "https://s3.amazonaws.com"
	}
	if p.AWSProfile != "" {
		fmt.Fprintf(&b, "# This profile delegates to the ~/.aws profile %q, which mc cannot read:\n", p.AWSProfile)
		b.WriteString("# fill in a key pair for it below.\n")
	}
	if p.SessionToken != "" {
		b.WriteString("# The profile uses temporary credentials (a session token), which\n")
		b.WriteString("# `mc alias set` cannot take: export them through MC_HOST_<alias> instead.\n")
	}
	access := p.AccessKey
	if access == "" {
		access = "<access key id>"
	}
	lookup := "on"
	if isAWSEndpoint(p.BaseUrl) {
		lookup = "auto"
	}
	fmt.Fprintf(&b, "mc alias set %s %s %s %s --api S3v4 --path %s\n",
		shellQuote(exportName(p.Name)), shellQuote(url), shellQuote(access), shellQuote(secretPlaceholder), lookup)
	if p.IgnoreSsl {
		b.WriteString("# This profile skips TLS verification: add --insecure to mc commands.\n")
	}
	fmt.Fprintf(&b, "# Then: mc ls %s\n", shellQuote(exportName(p.Name)))
	return b.String()
}

func exportRcloneRemote(p *cfg.Config) string {
	var b strings.Builder
	name := exportName(p.Name)
	region := profileRegion(p)
	b.WriteString("# ~/.config/rclone/rclone.conf\n")
	fmt.Fprintf(&b, "[%s]\n", name)
	b.WriteString("type = s3\n")
	if isAWSEndpoint(p.BaseUrl) {
		b.WriteString("provider = AWS\n")
	} else {
		b.WriteString("provider = Other\n")
	}
	if p.AWSProfile != "" {
		// rclone resolves a shared-config profile itself, SSO and roles
		// included, so a delegating profile exports without any keys.
		b.WriteString("env_auth = true\n")
		fmt.Fprintf(&b, "profile = %s\n", p.AWSProfile)
	} else {
		b.WriteString("env_auth = false\n")
		fmt.Fprintf(&b, "access_key_id = %s\n", p.AccessKey)
		fmt.Fprintf(&b, "secret_access_key = %s\n", secretPlaceholder)
		if p.SessionToken != "" {
			fmt.Fprintf(&b, "session_token = %s\n", sessionPlaceholder)
		}
	}
	if region != "" {
		fmt.Fprintf(&b, "region = %s\n", region)
	}
	if !isAWSEndpoint(p.BaseUrl) {
		fmt.Fprintf(&b, "endpoint = %s\n", strings.TrimRight(p.BaseUrl, "/"))
		b.WriteString("force_path_style = true\n")
	}
	if p.IgnoreSsl {
		b.WriteString("\n# This profile skips TLS verification: add --no-check-certificate to rclone commands.\n")
	}
	fmt.Fprintf(&b, "\n# Then: rclone lsd %s:\n", name)
	return b.String()
}

// exportFileName is where the w key writes a snippet.
func exportFileName(p *cfg.Config, f exportFormat) string {
	suffix := map[exportFormat]string{exportAWS: "aws", exportMC: "mc", exportRclone: "rclone"}[f]
	return fmt.Sprintf("s3duck-%s.%s.txt", exportName(p.Name), suffix)
}

// ExportProfile shows the highlighted profile as an AWS CLI profile, an mc
// alias or an rclone remote: f switches between them, c copies the one shown,
// w writes it to a file in the profile's download directory. Profiles screen.
func (c *Controller) ExportProfile() {
	p := c.configAt(c.view.List.GetCurrentItem())
	if p == nil {
		return
	}
	idx := 0
	title := func() string {
		return fmt.Sprintf(" Export %s — %s (secrets left out) ", tview.Escape(p.Name), exportFormats[idx])
	}
	c.showTextOverlay(title(), escapeForTextView(exportProfile(p, exportFormats[idx])),
		"[gray]f[white] next format (AWS CLI / mc / rclone) · [gray]c[white] copy · [gray]w[white] write to a file · [gray]Esc/q[white] close",
		func(ev *tcell.EventKey, _ *shell.Layer, tv *tview.TextView) bool {
			if ev.Key() != tcell.KeyRune {
				return false
			}
			text := exportProfile(p, exportFormats[idx])
			switch ev.Rune() {
			case 'f':
				idx = (idx + 1) % len(exportFormats)
				tv.SetText(escapeForTextView(exportProfile(p, exportFormats[idx])))
				tv.SetTitle(title())
				tv.ScrollToBeginning()
				return true
			case 'c':
				if err := u.CopyToClipboard(text); err != nil {
					c.error("Copy failed", fmt.Errorf("%w — use w to write it to a file instead", err))
					return true
				}
				c.success(fmt.Sprintf("%s export of %s copied to the clipboard", exportFormats[idx], p.Name))
				return true
			case 'w':
				dir := filepath.Join(c.params.HomeDir, "Downloads")
				if d := strings.TrimSpace(p.DownloadDir); d != "" {
					dir = expandHome(d, c.params.HomeDir)
				}
				dest := filepath.Join(dir, exportFileName(p, exportFormats[idx]))
				// 0600: the snippet names the access key ID and the endpoint.
				err := os.MkdirAll(dir, 0o700)
				if err == nil {
					err = os.WriteFile(dest, []byte(text), 0o600)
				}
				if err != nil {
					c.error("Export failed", err)
					return true
				}
				c.logActivity("exported profile %s for %s to %s", p.Name, exportFormats[idx], dest)
				c.success("Written to " + dest)
				return true
			}
			return false
		})
}
