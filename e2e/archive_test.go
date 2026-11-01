//go:build e2e

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// + marks by pattern, * inverts, and , z packs the marks into one .tar.gz in
// the download directory.
func TestMarkByPatternThenArchive(t *testing.T) {
	s := newStore(t)
	s.put("src/a.log", "alpha")
	s.put("src/b.log", "bravo")
	s.put("src/c.txt", "charlie")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "src/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "src/")
	u.waitFor("a.log", "b.log", "c.txt")

	u.typeText("+")
	u.waitFor("Mark matching", "Include folders")
	u.press("C-u")
	u.typeText("*.log")
	u.press("Tab", "Tab", "Enter") // checkbox, then Mark
	u.waitFor("Selected: 2")

	u.typeText("*")
	u.waitFor("Selected: 1") // only c.txt now
	u.typeText("*")
	u.waitFor("Selected: 2") // and back

	u.typeText(",")
	u.typeText("z")
	u.waitFor("as an archive", "File name", "Directory", ".tar.gz")
	u.press("Tab", "Tab", "Tab", "Enter") // format, name, directory → Download
	u.waitFor("Download as one archive", "Resolved: 2 object(s)")
	u.press("Enter") // OK
	u.waitFor("Archive complete.")
	u.press("Enter") // Done

	matches, _ := filepath.Glob(filepath.Join(h.downloads, "*.tar.gz"))
	if len(matches) != 1 {
		t.Fatalf("archives in the download directory: %v", matches)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got[hdr.Name] = string(body)
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a.log,b.log" || got["a.log"] != "alpha" || got["b.log"] != "bravo" {
		t.Errorf("archive holds %v", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(h.downloads, ".*.s3duck-part")); len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
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
