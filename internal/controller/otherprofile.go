package controller

import (
	"fmt"

	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// Dual panes on different profiles. Each pane holds its own connection
// (session.Conn): the active one is what c.model and c.activeConfig mirror,
// so every action — and its read-only and safe-delete guards — runs against
// the account the user is looking at. Between the panes, a copy or a move
// that crosses accounts streams through this process (the cross-profile
// copy), since a server-side copy cannot reach another endpoint.

// OpenProfileInOtherPane asks for a profile and points the other pane at it,
// opening the dual layout if needed. Bound to leader o.
func (c *Controller) OpenProfileInOtherPane() {
	if c.activeConfig == nil {
		return
	}
	var names []string
	var profiles []*cfg.Config
	for _, p := range c.params.Config {
		if p != nil {
			names = append(names, p.Name)
			profiles = append(profiles, p)
		}
	}
	if len(profiles) == 0 {
		return
	}
	cur := 0
	for i, p := range profiles {
		if c.dual && p == c.other().Conn().Profile {
			cur = i
		}
	}

	// lay is assigned below; the buttons fire only after that.
	var lay *shell.Layer
	form := tview.NewForm()
	form.SetTitle(" Open a profile in the other pane ")
	form.AddDropDown("Profile", escapeAll(names), cur, nil)
	form.SetBorder(true)
	form.AddButton("Open", func() {
		i, _ := form.GetFormItemByLabel("Profile").(*tview.DropDown).GetCurrentOption()
		lay.Close()
		if i < 0 || i >= len(profiles) {
			return
		}
		c.openProfileInOtherPane(profiles[i])
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 64, 7)
	c.view.App.SetFocus(form)
}

// escapeAll escapes each name for a widget that parses colour tags.
func escapeAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = tview.Escape(n)
	}
	return out
}

// connFor returns a connection for profile p: the one a pane already holds
// for it, so the same profile on both sides shares one client (and a copy
// between them stays server-side), or a new one.
func (c *Controller) connFor(p *cfg.Config) (session.Conn, error) {
	for _, pane := range c.panes {
		if conn := pane.Conn(); conn.Profile == p && conn.Model != nil {
			return conn, nil
		}
	}
	mdl, err := modelForProfile(p)
	if err != nil {
		return session.Conn{}, err
	}
	return session.Conn{Model: mdl, Profile: p}, nil
}

// openProfileInOtherPane points the other pane at p and moves focus there,
// landing where p was last browsing when it remembers. UI goroutine.
func (c *Controller) openProfileInOtherPane(p *cfg.Config) {
	conn, err := c.connFor(p)
	if err != nil {
		c.error("Cannot open profile", err)
		return
	}
	// Record where the outgoing profile was, before its pane is repointed.
	if !c.dual {
		c.ToggleDualPane() // seeds and focuses pane 1 as a mirror
	} else {
		c.swapPane()
		c.view.SetActivePane(c.active)
		c.view.App.SetFocus(c.view.List)
	}
	pane := c.pane()
	if pane.IsLocal() {
		pane.SetLocalDir("")
	}
	c.rememberPane(pane)
	pane.SetConn(conn)
	c.useConnOf(pane)
	c.clearFilterUI()
	c.logActivity("opened profile %s in the other pane (%s)", p.Name, modelConfigFor(p).CredentialSource())
	if p.LastBucket != "" {
		c.jumpTo(p.LastBucket, p.LastPrefix, "")
		return
	}
	go c.updateList()
}

// paneProfileLabel prefixes a pane title with its profile when the two panes
// browse different accounts — otherwise two "(logs)/" titles would not say
// which is which.
func paneProfileLabel(conn session.Conn, differ bool) string {
	if !differ || conn.Profile == nil {
		return ""
	}
	return "[aqua]" + tview.Escape(conn.Profile.Name) + "[-]:"
}

// crossPaneItems is what a copy or move between panes of different accounts
// needs from the other side: its client, profile, bucket and prefix.
type crossPaneTarget struct {
	model   *model.Model
	profile *cfg.Config
	bucket  string
	prefix  string
}

// crossPaneDestination reads the other pane as a copy destination, or says
// why it cannot be one.
func crossPaneDestination(other *session.Pane) (crossPaneTarget, error) {
	conn := other.Conn()
	loc := other.Loc()
	switch {
	case conn.Model == nil || conn.Profile == nil:
		return crossPaneTarget{}, fmt.Errorf("the other pane has no profile")
	case loc.LocalDir != "":
		return crossPaneTarget{}, fmt.Errorf("the other pane is a local directory")
	case loc.Bucket == nil || loc.Bucket.Key == nil:
		return crossPaneTarget{}, fmt.Errorf("open a bucket in the other pane first: it is the destination")
	case conn.Profile.ReadOnly:
		return crossPaneTarget{}, fmt.Errorf("%q, in the other pane, is marked read-only, so it cannot be copied into", conn.Profile.Name)
	}
	return crossPaneTarget{model: conn.Model, profile: conn.Profile, bucket: *loc.Bucket.Key, prefix: model.NormalizePrefix(loc.Path)}, nil
}

// copyAcrossPanes is Ctrl+Y / Ctrl+T when the panes browse different
// accounts: a streamed copy into the other pane's folder, and for a move the
// source objects deleted once their copy has landed.
func (c *Controller) copyAcrossPanes(items []copyMoveItem, isMove bool) {
	dst, err := crossPaneDestination(c.other())
	if err != nil {
		c.error("Copy to the other pane", err)
		return
	}
	src := c.model
	srcBucket := c.pane().Bucket()
	srcPrefix := model.NormalizePrefix(c.pane().Path())
	verb := "Copy"
	if isMove {
		verb = "Move"
	}
	text := fmt.Sprintf("%s %d item(s) to %s:%s/%s?\n\nThe panes are on different profiles, so each object is streamed through this machine (read here, written there).",
		verb, len(items), dst.profile.Name, dst.bucket, dst.prefix)
	if isMove {
		text += "\nEach source object is deleted only after its copy has landed."
	}
	c.confirmThen(text, func() {
		c.runCrossCopy(items, src, srcBucket, srcPrefix, dst.model, dst.profile.Name, dst.bucket, dst.prefix, isMove)
	})
}
