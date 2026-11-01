//go:build e2e

package e2e

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The 0.14.0 round through the real keys: a listing exported to CSV, a move
// between panes on two profiles, and a download cancelled and resumed.

// , e on a folder writes the rows on screen — filter applied — as CSV in the
// download directory, a key with a comma intact.
func TestExportListingToCSV(t *testing.T) {
	s := newStore(t)
	s.put("rep/a,b.csv", "12345")
	s.put("rep/c.txt", "x")
	s.put("rep/sub/d.txt", "y")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "rep/"))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "rep/")
	u.waitFor("a,b.csv", "c.txt", "sub/")
	u.press("/")
	u.typeText("re:\\.(csv|txt)$")
	u.press("Enter")
	u.waitUntil("the filter to hide sub/", func(scr string) bool { return !strings.Contains(scr, "sub/") })
	u.typeText(",")
	u.typeText("e")
	u.waitFor("Export 2 row(s)", "CSV", "JSON")
	u.press("Enter") // CSV
	u.waitFor("Exported 2 row(s) to")

	matches, _ := filepath.Glob(filepath.Join(h.downloads, "*.csv"))
	if len(matches) != 1 {
		t.Fatalf("exports in the download dir: %v", matches)
	}
	data, _ := os.ReadFile(matches[0])
	recs, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("CSV %v: %q", err, data)
	}
	keys := recs[1][1] + "|" + recs[2][1]
	if !strings.Contains(keys, "rep/a,b.csv") || !strings.Contains(keys, "rep/c.txt") || recs[1][0] != s.name {
		t.Errorf("rows = %q", recs[1:])
	}
}

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

// A throttled download cancelled part-way leaves a hidden partial file and
// no target; downloading again resumes it, and the result is the object
// byte for byte, with nothing left over.
func TestDownloadCancelThenResume(t *testing.T) {
	s := newStore(t)
	body := strings.Repeat("0123456789abcdef", (40<<20)/16)
	s.put("big.bin", body)
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 8 << 20
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitUntil("some of it to be on disk", func(string) bool {
		m, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
		if len(m) == 0 {
			return false
		}
		fi, err := os.Stat(m[0])
		return err == nil && fi.Size() >= 12<<20
	})
	u.press("Tab", "Enter") // Cancel (Background is first)
	u.waitFor("Download canceled.")
	u.press("Enter") // Done — the summary must not keep Cancel's button position

	if _, err := os.Stat(filepath.Join(h.downloads, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("a cancelled download left a file at the target name")
	}
	partials, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
	if len(partials) != 1 {
		t.Fatalf("partial files after the cancel: %v", partials)
	}
	fi, _ := os.Stat(partials[0])
	if fi.Size() == 0 || fi.Size() >= int64(len(body)) {
		t.Fatalf("the kept partial is %d bytes", fi.Size())
	}

	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Download complete.")
	u.press("Enter")

	got, _ := readFile(t, filepath.Join(h.downloads, "big.bin"))
	if got != body {
		t.Errorf("the resumed file is %d bytes and differs from the object", len(got))
	}
	left, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*"))
	if len(left) != 0 {
		t.Errorf("left after the resumed download: %v", left)
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
