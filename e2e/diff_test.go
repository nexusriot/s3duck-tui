//go:build e2e

package e2e

import (
	"testing"
)

// , d finds the local copy where a download would have put it and shows the
// changed line from both sides.
func TestDiffAgainstTheLocalCopy(t *testing.T) {
	s := newStore(t)
	s.put("cfg/app.yaml", "a: 1\nb: 2\nc: 3\n")
	h := newHome(t)
	h.file("Downloads/app.yaml", "a: 1\nb: 20\nc: 3\n")
	h.writeProfiles(h.profile(s.name, "cfg/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "cfg/")
	u.waitFor("app.yaml")
	u.highlight("cfg/app.yaml")

	u.typeText(",")
	u.typeText("d")
	u.waitFor("1 change(s)", "- b: 20", "+ b: 2", "next/previous change")
	u.press("Escape")
}

// Asking to diff an object with no local copy must offer the path rather than
// failing, and Esc must leave nothing behind.
func TestDiffWithNoLocalCopyAsksForThePath(t *testing.T) {
	s := newStore(t)
	s.put("p/cfg.yaml", "a: 1\n")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "p/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "p/")
	u.waitFor("cfg.yaml")
	u.highlight("p/cfg.yaml")

	u.typeText(",")
	u.typeText("d")
	u.waitFor("against local file", "Local file")
	u.press("Escape")
	u.waitGone("Local file")

	// Pointing it at a file that does not exist reports that, and does not
	// leave the browser in a dialog.
	u.typeText(",")
	u.typeText("d")
	u.waitFor("Local file")
	u.press("C-u")
	u.typeText("/nonexistent/path/cfg.yaml")
	u.press("Tab", "Enter")
	// The modal wraps a long path, so its halves are matched apart.
	u.waitFor("no file at /nonexistent/path/", "cfg.yaml")
	u.press("Enter")
	u.waitGone("no file at")
	u.waitFor("cfg.yaml")
}
