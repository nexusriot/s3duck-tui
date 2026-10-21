//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
)

// The basic loop, end to end: open a profile, land in its remembered
// location, download one object, quit. The download must hold the object's
// bytes, and quitting must record the location without rewriting anything
// else in the profile list.
func TestBrowseDownloadAndQuit(t *testing.T) {
	s := newStore(t)
	s.put("src/a.txt", "alpha")
	s.put("src/b.txt", "bravo")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "src/"), &cfg.Config{Name: "untouched", BaseUrl: "http://elsewhere:9000"})

	u := startTUI(t, h)
	u.waitFor("(profiles)", "untouched")
	u.press("Enter")
	u.waitFor("(" + s.name + ")/src/")
	u.waitFor("a.txt", "b.txt")

	u.highlight("src/b.txt")
	u.press("C-d")
	u.waitFor("Download to " + h.downloads)
	u.press("Enter") // OK
	u.waitFor("Download complete.")
	u.press("Enter") // Done
	u.waitGone("Download complete.")

	if got, ok := readFile(t, filepath.Join(h.downloads, "b.txt")); !ok || got != "bravo" {
		t.Errorf("downloaded b.txt = %q (exists %v), want bravo", got, ok)
	}
	if _, ok := readFile(t, filepath.Join(h.downloads, "a.txt")); ok {
		t.Error("a.txt was downloaded too; only the highlighted object was asked for")
	}
	if matches, _ := filepath.Glob(filepath.Join(h.downloads, "*.s3duck-part")); len(matches) > 0 {
		t.Errorf("temp files left behind: %v", matches)
	}

	// Move off the remembered prefix, then quit: the exit save must record
	// where the profile was, and only that.
	u.press("BSpace")
	root := "(" + s.name + ")/ "
	u.waitUntil("the bucket root's listing", func(scr string) bool {
		return strings.Contains(scr, root) && strings.Contains(scr, "src/")
	})
	u.quit()

	loaded, err := cfg.LoadConfiguration(h.configPath())
	if err != nil {
		t.Fatalf("config after quit: %v", err)
	}
	if len(loaded) != 2 || loaded[1].Name != "untouched" || loaded[1].BaseUrl != "http://elsewhere:9000" {
		t.Fatalf("the exit save rewrote the profile list: %+v", loaded)
	}
	if loaded[0].LastBucket != s.name || loaded[0].LastPrefix != "" {
		t.Errorf("remembered location = %q/%q, want %q/\"\"", loaded[0].LastBucket, loaded[0].LastPrefix, s.name)
	}
	if _, err := os.Stat(h.configPath() + ".bak"); !os.IsNotExist(err) {
		t.Error("a clean config was backed up as if it were unreadable")
	}
}
