package ops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// fakeOpener serves object bodies from memory.
type fakeOpener struct {
	bodies map[string]string
	// failOpen makes OpenObject itself fail, before any byte is written.
	failOpen map[string]error
	// failMid hands back a body that errors after its first half.
	failMid map[string]bool
	// onOpen runs before each open (a test cancels from here).
	onOpen func(key string)
	opened []string
}

type halfBody struct {
	r   io.Reader
	err error
}

func (h *halfBody) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if err == io.EOF {
		return n, h.err
	}
	return n, err
}

func (f *fakeOpener) OpenObject(ctx context.Context, _ *model.Object, key string) (io.ReadCloser, model.ObjectStat, error) {
	f.opened = append(f.opened, key)
	if f.onOpen != nil {
		f.onOpen(key)
	}
	if err := ctx.Err(); err != nil {
		return nil, model.ObjectStat{}, err
	}
	if err := f.failOpen[key]; err != nil {
		return nil, model.ObjectStat{}, err
	}
	body := f.bodies[key]
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st := model.ObjectStat{Size: int64(len(body)), LastModified: &mod}
	if f.failMid[key] {
		return io.NopCloser(&halfBody{r: strings.NewReader(body[:len(body)/2]), err: errors.New("connection reset")}), st, nil
	}
	return io.NopCloser(strings.NewReader(body)), st, nil
}

func archiveOp(t *testing.T, format ArchiveFormat, op *fakeOpener, keys ...string) *Archive {
	t.Helper()
	var ts []model.DownloadTarget
	var total int64
	for _, k := range keys {
		ts = append(ts, model.DownloadTarget{Key: k, Size: int64(len(op.bodies[k]))})
		total += int64(len(op.bodies[k]))
	}
	return &Archive{
		Bucket:        bucketObj("b"),
		SrcPrefix:     "data/",
		Targets:       ts,
		SelectedCount: 1,
		TotalBytes:    total,
		Format:        format,
		Dest:          filepath.Join(t.TempDir(), "out"+format.Ext()),
		Opener:        op,
		FS:            OSFS{},
	}
}

func readTarGz(t *testing.T, p string) map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
}

func readZip(t *testing.T, p string) (map[string]string, map[string]uint16) {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, methods := map[string]string{}, map[string]uint16{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(body)
		methods[f.Name] = f.Method
	}
	return out, methods
}

// noLeftovers asserts that a run left nothing but (maybe) the archive itself:
// no temp file from a failed or cancelled build.
func noLeftovers(t *testing.T, a *Archive) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(a.Dest))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".s3duck-part") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestArchiveTarGzRoundTrip(t *testing.T) {
	op := &fakeOpener{bodies: map[string]string{
		"data/logs/":         "",
		"data/logs/a.log":    "alpha\n",
		"data/logs/x/b.json": `{"b":1}`,
	}}
	a := archiveOp(t, ArchiveTarGz, op, "data/logs/", "data/logs/a.log", "data/logs/x/b.json")
	res := a.Run(context.Background(), &recorder{})
	if res.Failed() != 0 || res.Done != 3 || res.Bytes != int64(len("alpha\n")+len(`{"b":1}`)) {
		t.Fatalf("result = %+v", res)
	}
	got := readTarGz(t, a.Dest)
	want := map[string]string{"logs/": "", "logs/a.log": "alpha\n", "logs/x/b.json": `{"b":1}`}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("entry %q = %q, want %q (all: %v)", name, got[name], body, got)
		}
	}
	noLeftovers(t, a)
}

func TestArchiveZipStoresWhatIsAlreadyCompressed(t *testing.T) {
	op := &fakeOpener{bodies: map[string]string{
		"data/notes.txt": strings.Repeat("compress me ", 100),
		"data/pic.png":   "\x89PNG fake",
	}}
	a := archiveOp(t, ArchiveZip, op, "data/notes.txt", "data/pic.png")
	if res := a.Run(context.Background(), &recorder{}); res.Done != 2 || res.Failed() != 0 {
		t.Fatalf("result = %+v", res)
	}
	got, methods := readZip(t, a.Dest)
	if got["notes.txt"] != op.bodies["data/notes.txt"] || got["pic.png"] != op.bodies["data/pic.png"] {
		t.Errorf("contents differ: %v", got)
	}
	if methods["notes.txt"] != zip.Deflate || methods["pic.png"] != zip.Store {
		t.Errorf("methods = %v, want text deflated and the PNG stored", methods)
	}
}

func TestArchiveLeavesOutWhatCannotBeOpened(t *testing.T) {
	op := &fakeOpener{
		bodies:   map[string]string{"data/a": "a", "data/cold": "zzz", "data/c": "c"},
		failOpen: map[string]error{"data/cold": errors.New("InvalidObjectState: restore first")},
	}
	a := archiveOp(t, ArchiveTarGz, op, "data/a", "data/cold", "data/c")
	res := a.Run(context.Background(), &recorder{})
	if res.Done != 2 || res.Failed() != 1 || res.Failures[0].Label != "data/cold" {
		t.Fatalf("result = %+v, want two in and the archived one reported", res)
	}
	got := readTarGz(t, a.Dest)
	if _, in := got["cold"]; in || got["a"] != "a" || got["c"] != "c" {
		t.Errorf("archive = %v", got)
	}
}

func TestArchiveAbandonsOnAMidObjectFailure(t *testing.T) {
	op := &fakeOpener{
		bodies:  map[string]string{"data/a": "aaaa", "data/b": "bbbbbbbb", "data/c": "c"},
		failMid: map[string]bool{"data/b": true},
	}
	a := archiveOp(t, ArchiveZip, op, "data/a", "data/b", "data/c")
	res := a.Run(context.Background(), &recorder{})
	if res.Done != 0 || res.Failed() != 1 || !errors.Is(res.Failures[0].Err, errArchiveAbandoned) {
		t.Fatalf("result = %+v, want the run abandoned with nothing counted", res)
	}
	if _, err := os.Stat(a.Dest); !os.IsNotExist(err) {
		t.Error("a half-written archive must not land under its final name")
	}
	if len(op.opened) != 2 {
		t.Errorf("opened %v; nothing should be fetched after the archive is abandoned", op.opened)
	}
	noLeftovers(t, a)
}

func TestArchiveWithNothingOpenableWritesNoFile(t *testing.T) {
	op := &fakeOpener{
		bodies:   map[string]string{"data/a": "a"},
		failOpen: map[string]error{"data/a": errors.New("AccessDenied")},
	}
	a := archiveOp(t, ArchiveTarGz, op, "data/a")
	if res := a.Run(context.Background(), &recorder{}); res.Failed() != 1 || res.Done != 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(a.Dest); !os.IsNotExist(err) {
		t.Error("an archive of nothing should not be written")
	}
	noLeftovers(t, a)
}

func TestArchiveAsksBeforeReplacingAFile(t *testing.T) {
	op := &fakeOpener{bodies: map[string]string{"data/a": "new"}}
	a := archiveOp(t, ArchiveTarGz, op, "data/a")
	if err := os.WriteFile(a.Dest, []byte("old archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := a.Run(context.Background(), &recorder{answers: []Answer{AnswerNo}})
	if res.Skipped != 1 || len(op.opened) != 0 {
		t.Fatalf("declined: %+v, opened %v", res, op.opened)
	}
	if data, _ := os.ReadFile(a.Dest); string(data) != "old archive" {
		t.Error("declining must leave the existing file alone")
	}

	res = a.Run(context.Background(), &recorder{answers: []Answer{AnswerYes}})
	if res.Done != 1 {
		t.Fatalf("accepted: %+v", res)
	}
	if got := readTarGz(t, a.Dest); got["a"] != "new" {
		t.Errorf("replaced archive = %v", got)
	}
}

func TestArchiveCancelLeavesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	op := &fakeOpener{bodies: map[string]string{"data/a": "a", "data/b": "b"}}
	op.onOpen = func(key string) {
		if key == "data/b" {
			cancel()
		}
	}
	a := archiveOp(t, ArchiveZip, op, "data/a", "data/b")
	res := a.Run(ctx, &recorder{})
	if !res.Canceled || res.Done != 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(a.Dest); !os.IsNotExist(err) {
		t.Error("a cancelled archive must not be written")
	}
	noLeftovers(t, a)
}

func TestArchiveRefusesTwoKeysOnOnePath(t *testing.T) {
	op := &fakeOpener{bodies: map[string]string{"data/a/b": "1", "data/a//b": "2"}}
	a := archiveOp(t, ArchiveTarGz, op, "data/a/b", "data/a//b")
	res := a.Run(context.Background(), &recorder{})
	if res.Done != 1 || res.Failed() != 1 || res.Failures[0].Label != "data/a//b" {
		t.Fatalf("result = %+v", res)
	}
}

func TestArchiveEntryName(t *testing.T) {
	cases := []struct {
		prefix, key, want string
		bad               bool
	}{
		{"data/", "data/logs/a.log", "logs/a.log", false},
		{"data", "data/logs/", "logs/", false},
		{"", "/abs//x/./y", "abs/x/y", false},
		{"data/", "data/../../etc/passwd", "", true},
		{"data/", "data/", "", true},
		{"other/", "data/k", "data/k", false},
	}
	for _, c := range cases {
		got, err := ArchiveEntryName(c.prefix, c.key)
		if c.bad {
			if err == nil {
				t.Errorf("ArchiveEntryName(%q, %q) = %q, want a refusal", c.prefix, c.key, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ArchiveEntryName(%q, %q) = %q, %v; want %q", c.prefix, c.key, got, err, c.want)
		}
	}
}

func TestArchiveDescribeNamesTheFile(t *testing.T) {
	a := &Archive{Dest: "/tmp/x.zip", Targets: targets("k", 5), TotalBytes: 5, SelectedCount: 1}
	d := a.Describe()
	if d.Kind != KindArchive || !strings.Contains(d.ConfirmText, "/tmp/x.zip") || d.RefreshAfter {
		t.Errorf("descriptor = %+v", d)
	}
	if !bytes.Contains([]byte(d.Title), []byte("x.zip")) {
		t.Errorf("job title %q should name the archive", d.Title)
	}
}
