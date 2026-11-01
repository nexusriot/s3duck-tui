//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// Two profiles side by side: , o opens the second in the other pane, whose
// title names it; Ctrl+T moves a file across — streamed, the source deleted
// only after the copy landed — and Tab shows each side's own account.
func TestMoveBetweenPanesOnTwoProfiles(t *testing.T) {
	src := newStore(t)
	src.put("in/report.txt", "quarterly")
	src.put("in/keep.txt", "k")
	dst := newStore(t)
	dst.put("archive/.keep", "")
	h := newHome(t)
	a := h.profile(src.name, "in/")
	a.Name = "prof-a"
	b := h.profile(dst.name, "archive/")
	b.Name = "prof-b"
	h.writeProfiles(a, b)

	u := startTUIWith(t, h, noClipboard)
	u.waitFor("(profiles)", "prof-a", "prof-b")
	u.press("Enter") // prof-a
	u.waitFor("(" + src.name + ")/in/")
	u.typeText(",")
	u.typeText("o")
	u.waitFor("Open a profile in the other pane")
	u.press("Enter", "Down", "Enter") // open the dropdown, pick prof-b
	u.press("Tab", "Enter")           // Open
	// The other pane lands where prof-b was last browsing, titled with it.
	u.waitFor("prof-b:(" + dst.name + ")/archive/")
	u.waitFor("prof-b @") // the header names the active pane's profile

	u.press("Tab") // back to prof-a's pane
	u.waitFor("prof-a @")
	u.waitUntil("prof-a's pane to list its folder", func(scr string) bool {
		return strings.Contains(scr, "report.txt") && strings.Contains(scr, "keep.txt")
	})
	u.press("Down") // past ".." onto the first file (keep.txt sorts first)
	u.press("Down")
	u.press("Space") // mark report.txt
	u.waitFor("Selected: 1")
	u.press("C-t")
	u.waitFor("Move 1 item(s) to prof-b:", "read here, written there")
	u.press("Enter")
	u.waitFor("Cross-profile move complete.", "Moved: 1")
	u.press("Enter")

	if got, ok := dst.get("archive/report.txt"); !ok || got != "quarterly" {
		t.Errorf("destination holds %q (exists %v)", got, ok)
	}
	if _, ok := src.get("in/report.txt"); ok {
		t.Error("the moved source is still there")
	}
	if _, ok := src.get("in/keep.txt"); !ok {
		t.Error("an unmarked object went too")
	}
}

// = with the panes on two profiles compares each side through its own
// account. A same-named bucket on one client would have compared a folder
// with itself — or with nothing.
func TestComparePanesOnTwoProfiles(t *testing.T) {
	left := newStore(t)
	left.put("d/same.txt", "s")
	left.put("d/only-left.txt", "l")
	right := newStore(t)
	right.put("d/same.txt", "s")
	right.put("d/only-right.txt", "r")
	h := newHome(t)
	a := h.profile(left.name, "d/")
	a.Name = "prof-a"
	b := h.profile(right.name, "d/")
	b.Name = "prof-b"
	h.writeProfiles(a, b)

	u := startTUIWith(t, h, noClipboard)
	u.waitFor("(profiles)", "prof-a")
	u.press("Enter")
	u.waitFor("(" + left.name + ")/d/")
	u.typeText(",")
	u.typeText("o")
	u.waitFor("Open a profile in the other pane")
	u.press("Enter", "Down", "Enter", "Tab", "Enter")
	u.waitFor("prof-b:(" + right.name + ")/d/")
	u.press("Tab")
	u.waitFor("prof-a @")
	u.typeText("=")
	u.waitFor("Pane comparison", "only-left.txt", "only-right.txt", "1 only on the left, 0 differing, 1 only on the right")
}
