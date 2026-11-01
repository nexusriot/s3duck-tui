//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A pattern that matches nothing must leave the marks alone and say so,
// rather than marking everything or clearing what was marked.
func TestMarkPatternWithNoMatches(t *testing.T) {
	s := newStore(t)
	s.put("p/a.log", "a")
	s.put("p/b.txt", "b")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "p/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "p/")
	u.waitFor("a.log", "b.txt")

	u.typeText("+")
	u.waitFor("Mark matching")
	u.press("C-u")
	u.typeText("*.nothing")
	u.press("Tab", "Tab", "Enter")
	// The title carries the count; a match of nothing leaves no "Selected:".
	screen := u.waitUntil("the mark dialog to close", func(s string) bool {
		return !strings.Contains(s, "Mark matching")
	})
	if strings.Contains(screen, "Selected:") {
		t.Errorf("a pattern matching nothing marked something:\n%s", screen)
	}
}

// A regex that does not compile must be refused with the reason, not treated
// as "matches nothing" (which would silently unmark) or as a crash.
func TestMarkPatternWithABrokenRegex(t *testing.T) {
	s := newStore(t)
	s.put("p/a.log", "a")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "p/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "p/")
	u.waitFor("a.log")

	u.typeText("+")
	u.waitFor("Mark matching")
	u.press("C-u")
	u.typeText("re:[unclosed")
	u.press("Tab", "Tab", "Enter")
	u.waitFor("Mark by pattern", "error parsing regexp")
	u.press("Enter")
	u.waitGone("error parsing regexp")

	// The app is still usable afterwards.
	u.typeText("+")
	u.waitFor("Mark matching")
	u.press("Escape")
	u.waitGone("Mark matching")
}
