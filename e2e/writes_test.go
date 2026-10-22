//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every scenario here writes or deletes through the real key bindings and
// dialogs, then checks what the server holds — not what the app reported.
// Several pin a fix from the 2026-10 review against the whole stack, because
// the unit tests behind them stop at the function and the bug lived in what
// the dialogs handed it.

// A rename through Ctrl+R, then undone through `u`.
func TestRenameThenUndo(t *testing.T) {
	s := newStore(t)
	s.put("src/report.txt", "quarterly")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "src/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "src/")
	u.waitFor("report.txt")
	u.highlight("src/report.txt")

	u.press("C-r")
	u.waitFor("New name")
	u.press("C-u")
	u.typeText("final.txt")
	u.press("Tab", "Enter") // Save
	u.waitFor("Renamed")
	s.expect("src/", map[string]string{"src/final.txt": "quarterly"})
	u.press("Enter") // dismiss the notice: it is a modal, and holds the keys
	u.waitGone("Renamed")

	u.press("u")
	u.waitFor("Undo rename report.txt?")
	u.press("Enter") // OK
	u.waitFor("Undo complete.")
	u.press("Enter") // Done
	s.expect("src/", map[string]string{"src/report.txt": "quarterly"})
}

// The review's worst finding, end to end. A move answered with "Skip existing"
// must leave the kept destination AND its source alone — and so must the undo
// of that move. The undo used to "move back" the skipped item: it copied the
// destination's own, unrelated object over the source the user had kept, then
// deleted it, destroying both.
func TestMoveSkipExistingThenUndoKeepsBothSides(t *testing.T) {
	s := newStore(t)
	s.put("src/a.txt", "SRC-A")
	s.put("src/b.txt", "SRC-B")
	s.put("dst/a.txt", "DST-A") // occupies the move's destination
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "src/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "src/")
	u.waitFor("a.txt", "b.txt")

	u.press("C-s") // mark everything visible
	u.waitFor("Selected: 2")
	u.press("C-t")
	u.waitFor("Destination prefix")
	u.press("Tab") // bucket dropdown → prefix field
	u.press("C-u")
	u.typeText("dst/")
	u.press("Tab", "Enter") // Move
	u.waitFor("1 of 2 object(s) already exist at the destination", "dst/a.txt", "Skip existing")
	u.press("Tab", "Enter") // Skip existing
	u.waitFor("Move complete.")
	u.press("Enter") // Done

	s.expect("", map[string]string{
		"src/a.txt": "SRC-A", // skipped: the source stays
		"dst/a.txt": "DST-A", // skipped: the destination is untouched
		"dst/b.txt": "SRC-B", // moved
	})

	u.waitGone("Move complete.")
	u.press("u")
	u.waitFor("Undo Move of")
	u.press("Enter") // OK
	u.waitFor("Undo complete.")
	u.press("Enter") // Done

	s.expect("", map[string]string{
		"src/a.txt": "SRC-A",
		"src/b.txt": "SRC-B",
		"dst/a.txt": "DST-A",
	})
}

// A folder variant of the same: a partly skipped folder move, undone, must
// leave the objects it skipped exactly where they were.
func TestFolderMoveSkipExistingThenUndo(t *testing.T) {
	s := newStore(t)
	s.put("src/docs/kept.txt", "SRC-KEPT")
	s.put("src/docs/moved.txt", "SRC-MOVED")
	s.put("dst/docs/kept.txt", "DST-KEPT")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "src/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "src/")
	u.waitFor("docs/")
	u.highlight("src/docs/")
	u.press("C-t")
	u.waitFor("Destination prefix")
	u.press("Tab", "C-u")
	u.typeText("dst/")
	u.press("Tab", "Enter")
	u.waitFor("already exist at the destination", "Skip existing")
	u.press("Tab", "Enter")
	u.waitFor("Move complete.")
	u.press("Enter")

	s.expect("", map[string]string{
		"src/docs/kept.txt":  "SRC-KEPT",
		"dst/docs/kept.txt":  "DST-KEPT",
		"dst/docs/moved.txt": "SRC-MOVED",
	})

	u.waitGone("Move complete.")
	u.press("u")
	u.waitFor("Undo Move of")
	u.press("Enter")
	u.waitFor("Undo complete.")
	u.press("Enter")

	s.expect("", map[string]string{
		"src/docs/kept.txt":  "SRC-KEPT",
		"src/docs/moved.txt": "SRC-MOVED",
		"dst/docs/kept.txt":  "DST-KEPT",
	})
}

// The other critical finding: the local pane's upload of ONE file sent every
// file in the directory it sat in — here, the whole download directory,
// including the "secret" sibling and the subdirectory — past an overwrite scan
// that never saw those keys.
func TestLocalPaneUploadsOnlyTheHighlightedFile(t *testing.T) {
	s := newStore(t)
	h := newHome(t)
	h.file("Downloads/keep.txt", "upload me")
	h.file("Downloads/secret.txt", "never upload me")
	h.file("Downloads/sub/deep.txt", "nor me")
	h.writeProfiles(h.profile(s.name, "in/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "in/")

	u.press("l") // the other pane becomes the local download directory, focused
	u.waitFor("[local]", "keep.txt", "secret.txt")
	u.press("/")
	u.typeText("keep")
	u.press("Enter") // keep the filter: keep.txt is the only row, under the cursor
	u.waitUntil("the filter to leave only keep.txt", func(scr string) bool {
		return strings.Contains(scr, "keep.txt") && !strings.Contains(scr, "secret.txt")
	})
	u.press("C-y") // upload the highlighted file to the other pane's prefix
	u.waitFor("Upload complete.")
	u.press("Enter")

	s.expect("", map[string]string{"in/keep.txt": "upload me"})
}

// Marks in the local pane: Space and Ctrl+S used to do nothing there (they
// were gated on a bucket, which a local pane never has), so the advertised
// multi-file upload could not be reached. Marked files go up, and only they.
func TestLocalPaneUploadsTheMarkedFiles(t *testing.T) {
	s := newStore(t)
	h := newHome(t)
	h.file("Downloads/one.txt", "first")
	h.file("Downloads/two.txt", "second")
	h.file("Downloads/notes.md", "not marked")
	h.file("Downloads/sub/deep.txt", "not marked either")
	h.writeProfiles(h.profile(s.name, "in/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "in/")

	u.press("l")
	u.waitFor("[local]", "one.txt", "notes.md")
	u.press("/")
	u.typeText("*.txt")
	u.press("Enter")
	u.waitUntil("the glob to hide notes.md and sub/", func(scr string) bool {
		return strings.Contains(scr, "two.txt") && !strings.Contains(scr, "notes.md") && !strings.Contains(scr, "sub/")
	})
	u.press("C-s") // mark everything visible
	u.waitFor("Selected: 2")
	u.press("C-y")
	u.waitFor("Upload complete.", "Done: 2 of 2")
	u.press("Enter")

	s.expect("", map[string]string{"in/one.txt": "first", "in/two.txt": "second"})
}

// A config.json that fails to parse must be reported and left exactly as it
// is. Startup used to save the profile list before showing the error: the
// unreadable file was moved aside, replaced with an empty list, and the error
// cleared — so the user saw an empty profiles screen and no explanation.
func TestCorruptConfigIsReportedAndLeftAlone(t *testing.T) {
	h := newHome(t)
	corrupt := `[{"name": "prod", "access_key": "AKIA", "secret_key": "s3cret",}]`
	h.writeConfig([]byte(corrupt))

	u := startTUI(t, h)
	u.waitFor("Config error")
	u.press("Enter") // dismiss
	u.waitGone("Config error")
	u.quit()

	if got, _ := readFile(t, h.configPath()); got != corrupt {
		t.Errorf("config.json was rewritten to %q", got)
	}
	if _, err := os.Stat(h.configPath() + ".bak"); !os.IsNotExist(err) {
		t.Error("an automatic save moved the unreadable config aside")
	}
}

// Deleting a folder takes its subtree and nothing else — not the sibling that
// shares its name stem.
func TestDeleteFolderSparesItsSibling(t *testing.T) {
	s := newStore(t)
	s.put("docs/a.txt", "a")
	s.put("docs/sub/b.txt", "b")
	s.put("docs2/c.txt", "c")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("docs/", "docs2/")
	u.highlight("docs/")
	u.press("DC")
	u.waitFor("docs/", "2 object(s)")
	u.press("Enter") // OK
	u.waitFor("Delete complete.")
	u.press("Enter")

	s.expect("", map[string]string{"docs2/c.txt": "c"})
}

// A folder created with Ctrl+N is a zero-byte marker at exactly the typed name.
func TestCreateFolder(t *testing.T) {
	s := newStore(t)
	s.put("top/x.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "top/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "top/")
	u.waitFor("x.txt")
	u.press("C-n")
	u.waitFor("Create folder")
	u.typeText("fresh")
	u.press("Tab", "Enter") // Save
	u.waitFor("fresh/")

	s.expect("top/", map[string]string{"top/fresh/": "", "top/x.txt": "x"})
}

// A local → remote sync with delete-extraneous and an exclude, at the bucket
// root of a safe-delete profile. The mirror must end up equal to the local
// tree, minus what the exclude protects on BOTH sides, and the trash — which a
// root-level mirror used to plan, and run, as extraneous deletes — must
// survive untouched.
func TestSyncUploadMirrorsAndSparesExcludesAndTrash(t *testing.T) {
	s := newStore(t)
	s.put("old.txt", "extraneous")
	s.put("same.txt", "stale remote copy")
	s.put("node_modules/remote.js", "excluded on the remote side")
	s.put(".s3duck-trash/20260101-000000/gone.txt", "in the trash")
	h := newHome(t)
	local := filepath.Join(h.dir, "site")
	h.file("site/index.html", "<h1>hi</h1>")
	h.file("site/same.txt", "fresh local copy, longer than before")
	h.file("site/node_modules/local.js", "excluded on the local side")
	p := h.profile(s.name, "")
	p.Trash = true
	h.writeProfiles(p)

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("old.txt")

	u.press("C-e")
	u.waitFor("Sync — this pane")
	u.press("Tab", "C-u") // direction (local → remote) → local directory
	u.typeText(local)
	u.press("Tab", "Tab", "Tab") // → destination bucket → prefix → delete
	u.press("Space")             // delete extraneous
	u.press("Tab", "Tab")        // → checksum → exclude
	u.typeText("node_modules")
	u.press("Tab", "Enter") // Preview
	u.waitFor("Sync plan", "index.html", "same.txt", "old.txt")
	plan := u.screen()
	for _, never := range []string{"remote.js", "local.js", "gone.txt"} {
		if strings.Contains(plan, never) {
			t.Errorf("the plan names %s, which the exclude or the trash should keep out:\n%s", never, plan)
		}
	}
	u.press("Enter") // Apply
	u.waitFor("Sync complete.")
	u.press("Enter")

	s.expect("", map[string]string{
		"index.html":                             "<h1>hi</h1>",
		"same.txt":                               "fresh local copy, longer than before",
		"node_modules/remote.js":                 "excluded on the remote side",
		".s3duck-trash/20260101-000000/gone.txt": "in the trash",
	})
}
