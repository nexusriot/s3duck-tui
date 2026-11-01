package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCoveragePrefix(t *testing.T) {
	c := &coverage{}
	c.add(10, 5) // [10,15) — written out of order, before the start
	if got := c.prefix(0); got != 0 {
		t.Errorf("nothing at 0 yet: prefix = %d", got)
	}
	c.add(0, 10)
	c.add(20, 5) // a hole at [15,20)
	if got := c.prefix(0); got != 15 {
		t.Errorf("prefix = %d, want 15 (the hole stops it)", got)
	}
	c.add(15, 5)
	if got := c.prefix(0); got != 25 {
		t.Errorf("prefix = %d, want 25 once the hole fills", got)
	}
	if got := c.prefix(30); got != 30 {
		t.Errorf("prefix from an unwritten offset = %d", got)
	}
}

func TestPartialFilesAreRecognisedAndDistinct(t *testing.T) {
	p1, s1 := partialPaths("/d/report.pdf", "b", "a//report.pdf")
	p2, _ := partialPaths("/d/report.pdf", "b", "a/report.pdf")
	if p1 == p2 {
		t.Error("two keys landing on one path share a partial file")
	}
	if filepath.Dir(p1) != "/d" || !strings.HasPrefix(filepath.Base(p1), ".report.pdf.") {
		t.Errorf("partial %s is not a hidden sibling of the target", p1)
	}
	for _, n := range []string{filepath.Base(p1), filepath.Base(s1), "x.1234.s3duck-part"} {
		if !IsPartialFile(n) {
			t.Errorf("%s not recognised as a partial", n)
		}
	}
	if IsPartialFile("report.pdf") || IsPartialFile("s3duck-part.txt") {
		t.Error("an ordinary file read as a partial")
	}
}

func TestLoadResumeAcceptsOnlyTheSameVersion(t *testing.T) {
	dir := t.TempDir()
	partial, sidecar := partialPaths(filepath.Join(dir, "big.bin"), "b", "big.bin")
	write := func(st resumeState, partialLen int) {
		if err := os.WriteFile(partial, make([]byte, partialLen), 0600); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(st)
		if err := os.WriteFile(sidecar, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	good := resumeState{Bucket: "b", Key: "big.bin", ETag: "e1", Size: 100, Have: 40}
	write(good, 40)
	if got := loadResume(partial, sidecar, "b", "big.bin", "e1", 100); got != 40 {
		t.Errorf("same version: have = %d, want 40", got)
	}
	for name, tc := range map[string]struct {
		st      resumeState
		fileLen int
		etag    string
		size    int64
	}{
		"other version":         {good, 40, "e2", 100},
		"other size":            {good, 40, "e1", 101},
		"partial shorter":       {good, 10, "e1", 100},
		"already complete":      {resumeState{Bucket: "b", Key: "big.bin", ETag: "e1", Size: 100, Have: 100}, 100, "e1", 100},
		"another key's sidecar": {resumeState{Bucket: "b", Key: "other", ETag: "e1", Size: 100, Have: 40}, 40, "e1", 100},
	} {
		write(tc.st, tc.fileLen)
		if got := loadResume(partial, sidecar, "b", "big.bin", tc.etag, tc.size); got != 0 {
			t.Errorf("%s: have = %d, want a fresh start", name, got)
		}
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("%s: the stale sidecar was kept", name)
		}
	}
}

// rangedObject serves one object's ranged GETs, recording each request's
// Range and If-Match. failFrom makes any range starting at or after it fail
// (the first attempt's network dying mid-way); etag is the version served.
type rangedObject struct {
	mu       sync.Mutex
	body     []byte
	etag     string
	failFrom int64
	ranges   []string
	ifMatch  []string
}

func (o *rangedObject) handler(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.ranges = append(o.ranges, r.Header.Get("Range"))
	o.ifMatch = append(o.ifMatch, r.Header.Get("If-Match"))
	body, etag, failFrom := o.body, o.etag, o.failFrom
	o.mu.Unlock()

	if m := r.Header.Get("If-Match"); m != "" && m != `"`+etag+`"` {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`<Error><Code>PreconditionFailed</Code><Message>changed</Message></Error>`))
		return
	}
	start, end := int64(0), int64(len(body))-1
	if rg := r.Header.Get("Range"); rg != "" {
		spec := strings.TrimPrefix(rg, "bytes=")
		a, b, _ := strings.Cut(spec, "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			end, _ = strconv.ParseInt(b, 10, 64)
		}
		if end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
	}
	if failFrom > 0 && start >= failFrom {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>link died</Message></Error>`))
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body[start : end+1])
}

func bigBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/1000)
	}
	return b
}

// An interrupted download keeps its complete prefix; the next one fetches
// only the rest — a ranged GET pinned to the same version — and the file is
// whole and byte-identical.
func TestDownloadResumesWhereItStopped(t *testing.T) {
	obj := &rangedObject{body: bigBody(12 << 20), etag: "v1", failFrom: 5 << 20}
	f := newFakeS3(t, obj.handler)
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "bkt"
	target := DownloadTarget{Key: "big.bin", Size: 12 << 20, ETag: "v1"}

	if _, err := m.DownloadTarget(context.Background(), target, "", dir, &b, false, nil); err == nil {
		t.Fatal("the first attempt was meant to fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("a failed download left something at the target name")
	}
	partial, sidecar := partialPaths(filepath.Join(dir, "big.bin"), "bkt", "big.bin")
	if fi, err := os.Stat(partial); err != nil || fi.Size() != 5<<20 {
		t.Fatalf("partial = %v, %v; want the 5 MiB that arrived", fi, err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("no sidecar: %v", err)
	}

	obj.mu.Lock()
	obj.failFrom, obj.ranges, obj.ifMatch = 0, nil, nil
	obj.mu.Unlock()
	var firstProgress int64 = -1
	n, err := m.DownloadTarget(context.Background(), target, "", dir, &b, false, func(w, _ int64, _ string) {
		if firstProgress < 0 {
			firstProgress = w
		}
	})
	if err != nil || n != 12<<20 {
		t.Fatalf("resume: n=%d err=%v", n, err)
	}
	if len(obj.ranges) != 1 || obj.ranges[0] != fmt.Sprintf("bytes=%d-", 5<<20) {
		t.Errorf("resume requested %v, want one GET of the rest", obj.ranges)
	}
	if obj.ifMatch[0] != `"v1"` {
		t.Errorf("the resumed range was not pinned to the version: If-Match %q", obj.ifMatch[0])
	}
	if firstProgress != 5<<20 {
		t.Errorf("progress started at %d, want the kept 5 MiB", firstProgress)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "big.bin"))
	if !bytes.Equal(got, obj.body) {
		t.Error("the resumed file differs from the object")
	}
	for _, p := range []string{partial, sidecar} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind after success", filepath.Base(p))
		}
	}
}

// What was kept belongs to an older version: the object changed, so the
// download starts over rather than stitching two versions together.
func TestDownloadRestartsWhenTheObjectChanged(t *testing.T) {
	obj := &rangedObject{body: bigBody(12 << 20), etag: "v1", failFrom: 5 << 20}
	f := newFakeS3(t, obj.handler)
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "bkt"
	_, _ = m.DownloadTarget(context.Background(), DownloadTarget{Key: "big.bin", Size: 12 << 20, ETag: "v1"}, "", dir, &b, false, nil)

	obj.mu.Lock()
	obj.body, obj.etag, obj.failFrom, obj.ranges = bigBody(12 << 20)[1:], "v2", 0, nil
	obj.body = append(obj.body, 0xEE)
	obj.mu.Unlock()
	// The listing now reports v2: the kept v1 bytes must not be reused.
	if _, err := m.DownloadTarget(context.Background(), DownloadTarget{Key: "big.bin", Size: 12 << 20, ETag: "v2"}, "", dir, &b, false, nil); err != nil {
		t.Fatal(err)
	}
	// A fresh download: every range is a bounded part from a fresh start,
	// none the open-ended "rest of the object" a resume asks for.
	for _, r := range obj.ranges {
		if strings.HasSuffix(r, "-") {
			t.Errorf("resumed onto another version's bytes: %v", obj.ranges)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "big.bin"))
	if !bytes.Equal(got, obj.body) {
		t.Error("the file is not the new version")
	}
}

// A version change in the middle of a download (412 on a later range) is
// reported as such, and nothing of it is kept.
func TestDownloadMidwayVersionChangeKeepsNothing(t *testing.T) {
	obj := &rangedObject{body: bigBody(12 << 20), etag: "v1"}
	f := newFakeS3(t, obj.handler)
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "bkt"
	_, err := m.DownloadTarget(context.Background(), DownloadTarget{Key: "big.bin", Size: 12 << 20, ETag: "stale"}, "", dir, &b, false, nil)
	if !errors.Is(err, ErrObjectChanged) {
		t.Fatalf("err = %v, want ErrObjectChanged", err)
	}
	partial, sidecar := partialPaths(filepath.Join(dir, "big.bin"), "bkt", "big.bin")
	for _, p := range []string{partial, sidecar} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s kept for a version that no longer exists", filepath.Base(p))
		}
	}
}

// A cancelled download is the commonest interruption (Esc, or quitting): it
// keeps its prefix too.
func TestCancelledDownloadKeepsItsPrefix(t *testing.T) {
	obj := &rangedObject{body: bigBody(12 << 20), etag: "v1"}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			obj.handler(w, r)
			return
		}
		once.Do(cancel)
		<-r.Context().Done()
	})
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "bkt"
	_, err := m.DownloadTarget(ctx, DownloadTarget{Key: "big.bin", Size: 12 << 20, ETag: "v1"}, "", dir, &b, false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	partial, _ := partialPaths(filepath.Join(dir, "big.bin"), "bkt", "big.bin")
	fi, statErr := os.Stat(partial)
	if statErr != nil || fi.Size() != 5<<20 {
		t.Errorf("partial after cancel = %v, %v; want the first 5 MiB kept", fi, statErr)
	}
}

// A small object downloads as before: no partial file, no sidecar, and no
// version pin it would not need.
func TestSmallDownloadLeavesNoPartial(t *testing.T) {
	f := newFakeS3(t, serveBody("hello"))
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "b"
	if _, err := m.DownloadTarget(context.Background(), DownloadTarget{Key: "s.txt", Size: 5}, "", dir, &b, false, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "s.txt" {
		t.Errorf("dir holds %v", entries)
	}
}

// Partial files are not the user's: the local pane, sync and upload walks
// leave them out.
func TestWalksSkipPartialFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"keep.txt", ".big.bin.0a1b2c3d.s3duck-part", ".big.bin.0a1b2c3d.s3duck-resume"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	objs, err := WalkDir(dir)
	if err != nil || len(objs) != 1 || *objs[0].Key != "keep.txt" {
		t.Errorf("WalkDir = %d entries, %v", len(objs), err)
	}
	entries, _, err := WalkLocalTree(dir)
	if err != nil || len(entries) != 1 || entries[0].Rel != "keep.txt" {
		t.Errorf("WalkLocalTree = %+v, %v", entries, err)
	}
	targets, _, err := LocalTargets([]string{dir}, "in/")
	if err != nil || len(targets) != 1 {
		t.Errorf("LocalTargets = %+v, %v", targets, err)
	}
}

// A cancel that lands after the last range is on disk is a finished
// download, not a partial for the next attempt to throw away.
func TestCompleteDownloadIsKeptDespiteALateCancel(t *testing.T) {
	obj := &rangedObject{body: bigBody(12 << 20), etag: "v1"}
	f := newFakeS3(t, obj.handler)
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "bkt"
	ctx, cancel := context.WithCancel(context.Background())
	_, err := m.DownloadTarget(ctx, DownloadTarget{Key: "big.bin", Size: 12 << 20, ETag: "v1"}, "", dir, &b, false,
		func(w, total int64, _ string) {
			if w == total {
				cancel() // the last byte has been written
			}
		})
	if err != nil {
		t.Fatalf("a complete download reported %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "big.bin")); !bytes.Equal(got, obj.body) {
		t.Error("the target is not the object")
	}
}
