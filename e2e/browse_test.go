//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fmt"

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

// A location on the command line opens there in place of the profile list —
// as a folder when the key has no slash but holds objects, and as its folder
// with the object highlighted when it is an object.
func TestLaunchArgumentOpensLocation(t *testing.T) {
	s := newStore(t)
	s.put("a/b/c.txt", "c")
	s.put("a/top.txt", "t")
	h := newHome(t)
	h.writeProfiles(h.profile("", ""))

	u := startTUIWith(t, h, nil, "-p", "e2e", "s3://"+s.name+"/a/b")
	u.waitFor(fmt.Sprintf("(%s)/a/b/", s.name), "c.txt")
	u.quit()

	u = startTUIWith(t, h, nil, "s3://"+s.name+"/a/top.txt") // one profile: no -p needed
	u.waitFor(fmt.Sprintf("(%s)/a/", s.name), "top.txt")
	u.waitUntil("a/top.txt highlighted", func(sc string) bool { return highlighted(sc) == "a/top.txt" })
}

// g goes to a URI, then to a relative path with "..", landing on an object;
// , c then names that object as a URI (copied, or shown when there is no
// clipboard — either way on screen).
func TestGoToThenCopyURI(t *testing.T) {
	s := newStore(t)
	s.put("a/b/c.txt", "c")
	s.put("a/top.txt", "t")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")

	u.typeText("g")
	u.waitFor("Go to", "s3://"+s.name+"/")
	u.press("C-u")
	u.typeText("s3://" + s.name + "/a/b/")
	u.press("Enter")
	u.waitFor(fmt.Sprintf("(%s)/a/b/", s.name), "c.txt")

	u.typeText("g")
	u.waitFor("s3://" + s.name + "/a/b/")
	u.press("C-u")
	u.typeText("../top.txt")
	u.press("Enter")
	u.waitFor(fmt.Sprintf("(%s)/a/", s.name))
	u.waitUntil("a/top.txt highlighted", func(sc string) bool { return highlighted(sc) == "a/top.txt" })

	u.typeText(",")
	u.typeText("c")
	u.waitFor("s3://" + s.name + "/a/top.txt")
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
