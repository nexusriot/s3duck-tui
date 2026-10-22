//go:build integration

// Corner cases for the archive operation against a real endpoint: the key
// shapes and mid-run changes that a fake store cannot produce faithfully.
package model_test

import (
	"archive/tar"
	"archive/zip"
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

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
)

// archiveInto runs an Archive over the resolved contents of prefix and returns
// the result plus the entries that landed in the file.
func archiveInto(t *testing.T, m *model.Model, b *model.Object, prefix string, format ops.ArchiveFormat, mutate func()) (ops.Result, map[string]string) {
	t.Helper()
	ctx := context.Background()
	targets, total, err := m.ResolveDownloadObjects(ctx, prefix, true, nil, b)
	if err != nil {
		t.Fatalf("ResolveDownloadObjects(%q): %v", prefix, err)
	}
	if mutate != nil {
		mutate() // happens between resolution and the first read, by design
	}
	dest := filepath.Join(t.TempDir(), "out"+format.Ext())
	res := (&ops.Archive{
		Bucket: b, SrcPrefix: prefix, Targets: targets, SelectedCount: 1,
		TotalBytes: total, Format: format, Dest: dest,
		Opener: m, FS: ops.OSFS{},
	}).Run(ctx, yesReporter{})

	entries := map[string]string{}
	if _, err := os.Stat(dest); err != nil {
		return res, entries
	}
	switch format {
	case ops.ArchiveZip:
		zr, err := zip.OpenReader(dest)
		if err != nil {
			t.Fatalf("the archive is not a readable zip: %v", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			entries[f.Name] = string(body)
		}
	default:
		f, err := os.Open(dest)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("the archive is not readable gzip: %v", err)
		}
		tr := tar.NewReader(zr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("the archive is not a readable tar: %v", err)
			}
			body, _ := io.ReadAll(tr)
			entries[h.Name] = string(body)
		}
	}
	return res, entries
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// An object that disappears between the listing and its turn in the archive.
// Nothing of it has been written yet, so it is left out and reported and the
// archive stays whole — the distinction the operation is built around.
func TestIntegrationArchiveWhenAnObjectVanishes(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	b := freshBucket(t, m, "s3duck-it-archive-vanish")
	putObject(t, m, b, "d/a.txt", "alpha")
	putObject(t, m, b, "d/gone.txt", "bravo")
	putObject(t, m, b, "d/c.txt", "charlie")

	res, entries := archiveInto(t, m, b, "d/", ops.ArchiveTarGz, func() {
		if err := m.DeleteKey(ctx, "d/gone.txt", b); err != nil {
			t.Fatalf("deleting the object mid-run: %v", err)
		}
	})

	if res.Done != 2 || res.Failed() != 1 {
		t.Fatalf("result = %+v, want two archived and one reported", res)
	}
	if res.Failures[0].Label != "d/gone.txt" {
		t.Errorf("failure names %q, want the vanished key", res.Failures[0].Label)
	}
	if got := keysOf(entries); strings.Join(got, ",") != "a.txt,c.txt" {
		t.Errorf("archive holds %v, want the two survivors", got)
	}
	if entries["a.txt"] != "alpha" || entries["c.txt"] != "charlie" {
		t.Errorf("contents = %v", entries)
	}
}

// Keys that are legal in S3 and awkward everywhere else.
func TestIntegrationArchiveOddKeys(t *testing.T) {
	m := testModel(t)
	b := freshBucket(t, m, "s3duck-it-archive-odd")

	bodies := map[string]string{
		"odd/with space.txt":  "space",
		"odd/[red]tagged.txt": "brackets",
		"odd/юникод-🦆.txt":    "unicode",
		"odd/empty.txt":       "",
		"odd/deep/er/one.txt": "nested",
		"odd/trailing .txt":   "trailing-space",
	}
	for k, v := range bodies {
		putObject(t, m, b, k, v)
	}

	res, entries := archiveInto(t, m, b, "odd/", ops.ArchiveZip, nil)
	if res.Failed() != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"[red]tagged.txt", "deep/er/one.txt", "empty.txt", "trailing .txt", "with space.txt", "юникод-🦆.txt"}
	if got := keysOf(entries); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("entry names:\n got  %v\n want %v", got, want)
	}
	if entries["with space.txt"] != "space" || entries["юникод-🦆.txt"] != "unicode" {
		t.Errorf("contents = %v", entries)
	}
	if body, ok := entries["empty.txt"]; !ok || body != "" {
		t.Errorf("a zero-byte object should be an empty entry, got %q (present=%v)", body, ok)
	}
}

// Two keys that are different objects in S3 but one path once extracted.
// Writing both would silently keep only the last, so the second is refused.
func TestIntegrationArchiveRefusesCollidingKeys(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	b := freshBucket(t, m, "s3duck-it-archive-collide")
	putObject(t, m, b, "c/x/y.txt", "single-slash")

	// Written through the raw client, not putObject: a backend that refuses
	// the key must skip this test rather than fail it. MinIO is one —
	// XMinioInvalidObjectName — so the collision this guards against can only
	// arise on a backend that accepts both spellings (AWS S3 does).
	if _, err := m.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(*b.Key),
		Key:    aws.String("c/x//y.txt"),
		Body:   strings.NewReader("double-slash"),
	}); err != nil {
		t.Skipf("this endpoint will not store a double-slash key, so there is no collision to refuse: %v", err)
	}
	keys, err := m.ListObjects(ctx, "c/", b)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Skipf("this endpoint stores %d key(s) for the two spellings, so there is no collision to refuse", len(keys))
	}

	res, entries := archiveInto(t, m, b, "c/", ops.ArchiveTarGz, nil)
	if res.Done != 1 || res.Failed() != 1 {
		t.Fatalf("result = %+v, want one archived and one refused", res)
	}
	if !strings.Contains(res.Failures[0].Err.Error(), "lands on") {
		t.Errorf("failure = %v, want it to explain the collision", res.Failures[0].Err)
	}
	if got := keysOf(entries); strings.Join(got, ",") != "x/y.txt" {
		t.Errorf("archive holds %v, want one entry", got)
	}
}

// A folder marker is a directory entry, and the archive of a whole bucket
// takes its names from the root.
func TestIntegrationArchiveFolderMarkersFromTheBucketRoot(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	b := freshBucket(t, m, "s3duck-it-archive-root")
	// CreateFolder writes the key verbatim, so the trailing slash that makes
	// it a folder marker is the caller's: the controller appends it, and a
	// name without it would create an ordinary zero-byte object.
	name := "emptydir/"
	if err := m.CreateFolder(&name, b); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	putObject(t, m, b, "file.txt", "top")

	// Resolved the way the controller resolves a selection: one call per
	// marked item. (A bucket root is never resolved as a folder of its own —
	// an empty prefix would be normalised to "/" and match nothing.)
	var targets []model.DownloadTarget
	var total int64
	for _, sel := range []struct {
		key    string
		folder bool
		size   int64
	}{
		{"emptydir/", true, 0},
		{"file.txt", false, int64(len("top"))},
	} {
		size := sel.size
		objs, n, err := m.ResolveDownloadObjects(ctx, sel.key, sel.folder, &size, b)
		if err != nil {
			t.Fatalf("resolving %q: %v", sel.key, err)
		}
		targets = append(targets, objs...)
		total += n
	}
	dest := filepath.Join(t.TempDir(), "root.tar.gz")
	res := (&ops.Archive{
		Bucket: b, SrcPrefix: "", Targets: targets, SelectedCount: 1,
		TotalBytes: total, Format: ops.ArchiveTarGz, Dest: dest,
		Opener: m, FS: ops.OSFS{},
	}).Run(ctx, yesReporter{})
	if res.Failed() != 0 {
		t.Fatalf("result = %+v", res)
	}

	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	kinds := map[string]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds[h.Name] = h.Typeflag
	}
	if kinds["file.txt"] != tar.TypeReg {
		t.Errorf("file.txt is type %q, want a regular file", kinds["file.txt"])
	}
	if kinds["emptydir/"] != tar.TypeDir {
		t.Errorf("entries = %v, want emptydir/ as a directory", kinds)
	}
}

// A few megabytes through the streaming writer, to prove nothing is staged and
// the bytes survive the round trip.
func TestIntegrationArchiveStreamsALargeObject(t *testing.T) {
	m := testModel(t)
	b := freshBucket(t, m, "s3duck-it-archive-large")
	// Incompressible, so the zip cannot hide a truncation behind deflate.
	body := make([]byte, 2<<20)
	for i := range body {
		body[i] = byte(i * 2654435761 >> 13)
	}
	putObject(t, m, b, "big/payload.bin", string(body))

	res, entries := archiveInto(t, m, b, "big/", ops.ArchiveZip, nil)
	if res.Failed() != 0 || res.Done != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := entries["payload.bin"]; got != string(body) {
		t.Errorf("round trip changed the bytes: %d in, %d out", len(body), len(got))
	}
	if res.Bytes != int64(len(body)) {
		t.Errorf("reported %d bytes, want %d", res.Bytes, len(body))
	}
}
