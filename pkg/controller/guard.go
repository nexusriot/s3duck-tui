package controller

import (
	"fmt"
	"strings"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
)

// identityLabel is the header's left field: the profile, its endpoint host,
// and a read-only marker.
func identityLabel(p *cfg.Config) string {
	if p == nil {
		return ""
	}
	label := p.Name
	if host := endpointHost(p.BaseUrl); host != "" {
		label += " @ " + host
	}
	if p.Region != nil && *p.Region != "" {
		label += " (" + *p.Region + ")"
	}
	if p.ReadOnly {
		label += "  [READ-ONLY]"
	}
	return " " + label + " "
}

// endpointHost reduces an endpoint URL to its host, which is the part worth
// screen space. A profile with no URL (plain AWS) reports "aws".
func endpointHost(raw string) string {
	if raw == "" {
		return "aws"
	}
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// showIdentity refreshes the header for the profile now open. Called from the
// UI goroutine (profile open/close).
func (c *Controller) showIdentity() {
	if c.activeConfig == nil {
		c.view.SetIdentity("", false)
		return
	}
	c.view.SetIdentity(identityLabel(c.activeConfig), c.activeConfig.ReadOnly)
}

// readOnly reports whether the open profile forbids writes.
func (c *Controller) readOnly() bool {
	return c.activeConfig != nil && c.activeConfig.ReadOnly
}

// readOnlyBlocked reports the profile's read-only state to the user and
// returns true when the caller must stop. Called from key handlers on the UI
// goroutine, hence the goroutine around c.error.
func (c *Controller) readOnlyBlocked(action string) bool {
	if !c.readOnly() {
		return false
	}
	name := ""
	if c.activeConfig != nil {
		name = c.activeConfig.Name
	}
	go c.error("Read-only profile", fmt.Errorf(
		"%q is marked read-only, so it cannot %s.\n\nClear the flag in the profile's options (o on the profiles screen) if this is deliberate",
		name, action))
	return true
}
