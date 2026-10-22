//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// The awkward inputs, through the real dialogs: a pattern that matches
// nothing, one that does not compile, a file name that is really a path, an
// object with nothing in it, and a key that looks like markup. Each of these
// is a place where the app either says something useful or quietly does the
// wrong thing, and only the real widgets can tell which.

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

// The archive dialog has a directory field of its own, so a file name holding
// a separator is a mistake worth naming rather than a path to silently honour.
func TestArchiveRefusesAPathAsAFileName(t *testing.T) {
	s := newStore(t)
	s.put("p/a.txt", "a")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "p/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "p/")
	u.waitFor("a.txt")
	u.highlight("p/a.txt")

	u.typeText(",")
	u.typeText("z")
	u.waitFor("as an archive", "File name")
	u.press("Tab") // format → file name
	u.press("C-u")
	u.typeText("sub/dir/out.tar.gz")
	u.press("Tab", "Tab", "Enter") // directory → Download
	u.waitFor("Archive", "is a path")
	u.press("Enter")
	u.waitGone("is a path")
}

// A zero-byte object has no satisfiable byte range, so the preview's ranged
// GET is refused by the server. That must read as an empty file.
func TestPreviewOfAnEmptyObject(t *testing.T) {
	s := newStore(t)
	s.put("p/empty.txt", "")
	s.put("p/other.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "p/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "p/")
	u.waitFor("empty.txt")
	u.highlight("p/empty.txt")

	u.typeText("P")
	u.waitFor("empty.txt — 0 B of 0 B")
	screen := u.screen()
	if strings.Contains(screen, "hexdump") {
		t.Errorf("an empty object previewed as binary:\n%s", screen)
	}
	if strings.Contains(strings.ToLower(screen), "failed") {
		t.Errorf("the preview reported a failure:\n%s", screen)
	}
	u.press("Escape")
	u.waitGone("0 B of 0 B")
}

// A key that looks like a colour tag must survive every stage: the listing,
// the details panel and the preview's title and body. tview would otherwise
// eat it as markup and show a row naming a different object than the one the
// cursor acts on.
func TestKeysThatLookLikeMarkup(t *testing.T) {
	s := newStore(t)
	s.put("p/[red]alert.log", "level=[red] something\n")
	s.put("p/plain.log", "ordinary\n")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "p/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "p/")
	u.waitFor("[red]alert.log")
	u.highlight("p/[red]alert.log")

	u.typeText("P")
	u.waitFor("[red]alert.log —", "level=[red] something")
	u.press("Escape")
	u.waitGone("level=[red] something")

	// And the object it acts on is the one named: a download lands under the
	// literal key, not a tag-stripped one.
	u.press("C-d")
	u.waitFor("Download to " + h.downloads)
	u.press("Enter")
	u.waitFor("Download complete.")
	u.press("Enter")
	if got, ok := readFile(t, h.downloads+"/[red]alert.log"); !ok || got != "level=[red] something\n" {
		t.Errorf("downloaded file = %q (found=%v); the literal key must survive", got, ok)
	}
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

// A profile name is user-typed and the profiles screen renders colour tags,
// so a name that looks like one must still be shown as itself — in the list
// and in the details panel beside it.
func TestProfileNameThatLooksLikeMarkup(t *testing.T) {
	h := newHome(t)
	p := h.profile("", "")
	p.Name = "[red]prod"
	h.writeProfiles(p)

	u := startTUI(t, h)
	u.waitFor("(profiles)")
	screen := u.waitFor("[red]prod")
	if strings.Count(screen, "[red]prod") < 2 {
		t.Errorf("the name should appear in both the list and the details panel:\n%s", screen)
	}
	// The details panel must still describe that profile, not a mangled name.
	// The details panel pads after the label, so the two are matched apart.
	u.waitFor("Config: [red]prod", endpoint())
}
