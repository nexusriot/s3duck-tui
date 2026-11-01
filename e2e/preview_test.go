//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func gz(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// P on a gzipped JSON object: decoded, indented, and r flips to the raw text.
func TestPreviewDecodesGzippedJSON(t *testing.T) {
	s := newStore(t)
	s.put("logs/doc.json.gz", gz(t, `{"name":"duck","tags":["a","b"]}`))
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "logs/"))

	u := startTUI(t, h)
	u.openProfile(s.name, "logs/")
	u.waitFor("doc.json.gz")
	u.highlight("logs/doc.json.gz")

	u.typeText("P")
	u.waitFor("gzip →", "JSON, pretty-printed", `  "name": "duck",`)
	u.typeText("r")
	u.waitFor(`{"name":"duck","tags":["a","b"]}`, "(raw")
	u.press("Escape")
	u.waitGone("pretty-printed")
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
