//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The reading, packaging and watching round, through the real keys: each
// scenario ends in what the disk or the server holds where there is something
// to hold, and in what the screen shows where the feature is the screen.

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

// , b shows the new sections, and t writes the bucket's tags.
func TestBucketDashboardEditsTags(t *testing.T) {
	s := newStore(t)
	s.put("k.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("k.txt")

	u.typeText(",")
	u.typeText("b")
	u.waitFor("Bucket config", "Public access block", "Lifecycle", "Replication", "Website hosting", "Policy")
	u.typeText("t")
	u.waitFor("Tags (key=value per line)")
	u.typeText("env=e2e")
	u.press("Tab", "Enter") // Save
	u.waitFor("env=e2e", "1 tag(s)")

	out, err := s.m.Client.GetBucketTagging(context.Background(), &s3.GetBucketTaggingInput{Bucket: aws.String(s.name)})
	if err != nil {
		t.Fatalf("reading the tags back: %v", err)
	}
	if len(out.TagSet) != 1 || aws.ToString(out.TagSet[0].Key) != "env" || aws.ToString(out.TagSet[0].Value) != "e2e" {
		t.Errorf("bucket tags = %+v", out.TagSet)
	}
	u.press("Escape")
}

// , r opens the tracker; MinIO has no archive tier, so it is empty and says
// how to fill it.
func TestRestoreTrackerOpens(t *testing.T) {
	s := newStore(t)
	s.put("k.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("k.txt")
	u.typeText(",")
	u.typeText("r")
	u.waitFor("Glacier restores", "no restores tracked")
	u.press("Escape")
}

// x on the profiles screen: the export cycles formats and writes the shown one
// to the download directory, with the secret left out.
func TestExportProfileWritesAFile(t *testing.T) {
	h := newHome(t)
	h.writeProfiles(h.profile("", ""))

	u := startTUI(t, h)
	u.waitFor("(profiles)", "e2e")
	u.typeText("x")
	u.waitFor("Export e2e", "AWS CLI", "endpoint_url = "+endpoint())
	u.typeText("f")
	u.waitFor("mc alias set e2e")
	u.typeText("w")
	want := filepath.Join(h.downloads, "s3duck-e2e.mc.txt")
	// The notice wraps a long path, so its two halves are matched apart.
	u.waitFor("Written to "+filepath.Dir(want), filepath.Base(want))

	got, ok := readFile(t, want)
	if !ok || !strings.Contains(got, "mc alias set e2e "+endpoint()) || !strings.Contains(got, "not exported") {
		t.Errorf("export file:\n%s", got)
	}
}
