package model

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWalkDirFoldersFirstAndPathsAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}

	objs, err := WalkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("got %d entries, want 3", len(objs))
	}
	// Folders first, then files by name — the same shape a remote listing has,
	// which is what lets the pane renderer stay unchanged.
	if objs[0].Ot != Folder || *objs[0].Key != "sub" {
		t.Errorf("first entry = %+v, want the directory", objs[0])
	}
	if *objs[1].Key != "a.txt" || *objs[2].Key != "b.txt" {
		t.Errorf("files not sorted by name: %q, %q", *objs[1].Key, *objs[2].Key)
	}
	// A directory's FullPath ends with a separator, so its identity can never
	// collide with a file of the same name.
	if got := *objs[0].FullPath; got != filepath.Join(dir, "sub")+string(filepath.Separator) {
		t.Errorf("directory FullPath = %q", got)
	}
	if got := *objs[1].FullPath; got != filepath.Join(dir, "a.txt") {
		t.Errorf("file FullPath = %q", got)
	}
	if objs[1].Size == nil || *objs[1].Size != 5 {
		t.Errorf("size not read: %+v", objs[1].Size)
	}
	if objs[1].LastModified == nil {
		t.Error("mtime not read")
	}
	if _, err := WalkDir(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing directory should error")
	}
}

func TestWalkDirSkipsIrregularEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets in the filesystem are POSIX-only")
	}
	dir := t.TempDir()
	// A socket is not transferable and must not appear as a file: opening one
	// for an upload would block or fail mid-transfer. (A unix socket is used
	// rather than a fifo because it needs no extra dependency.)
	ln, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Skipf("cannot create a socket here: %v", err)
	}
	defer ln.Close()
	objs, werr := WalkDir(dir)
	if werr != nil {
		t.Fatal(werr)
	}
	if len(objs) != 0 {
		t.Errorf("irregular entry was listed: %+v", objs)
	}
}

func TestParentDir(t *testing.T) {
	if got := ParentDir("/a/b/c"); got != "/a/b" {
		t.Errorf("ParentDir = %q, want /a/b", got)
	}
	if got := ParentDir("/a/b/c/"); got != "/a/b" {
		t.Errorf("a trailing separator should not change the answer: %q", got)
	}
	// At the root there is nowhere up: the pane must stay put rather than
	// turn into something else.
	if got := ParentDir("/"); got != "" {
		t.Errorf("ParentDir(/) = %q, want empty", got)
	}
}

func TestLocalDirSize(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("12345"), 0600)
	os.WriteFile(filepath.Join(dir, "b"), []byte("123"), 0600)
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "deep"), []byte("ignored"), 0600)

	bytes, files := LocalDirSize(dir)
	if bytes != 8 || files != 2 {
		t.Errorf("LocalDirSize = %d bytes / %d files, want 8/2 (one level only)", bytes, files)
	}
	if b, f := LocalDirSize(filepath.Join(dir, "missing")); b != 0 || f != 0 {
		t.Errorf("an unreadable directory should report nothing, got %d/%d", b, f)
	}
}

func TestLocalTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "reports", "q1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "reports", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "reports", "top.txt"), []byte("aa"), 0600)
	os.WriteFile(filepath.Join(root, "reports", "q1", "a.csv"), []byte("bbb"), 0600)
	os.WriteFile(filepath.Join(root, "loose.bin"), []byte("cccc"), 0600)

	targets, total, err := LocalTargets(
		[]string{filepath.Join(root, "reports"), filepath.Join(root, "loose.bin")}, "dst/")
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 {
		t.Errorf("total = %d, want 9 (2+3+4)", total)
	}
	keys := map[string]bool{}
	for _, tg := range targets {
		keys[tg.RemotePath] = true
	}
	// A selected directory keeps its own name at the destination, so the tree
	// arrives as a tree rather than flattened.
	for _, want := range []string{"dst/reports/top.txt", "dst/reports/q1/a.csv", "dst/loose.bin"} {
		if !keys[want] {
			t.Errorf("missing key %q in %v", want, keys)
		}
	}
	// An empty directory becomes a marker object, matching the walk-based
	// upload's convention.
	if !keys["dst/reports/empty/"] {
		t.Errorf("empty directory got no marker: %v", keys)
	}
	if _, _, err := LocalTargets([]string{filepath.Join(root, "gone")}, "dst/"); err == nil {
		t.Error("a missing path should error rather than upload nothing silently")
	}
}

func TestDisplayDir(t *testing.T) {
	if got := DisplayDir("/home/u/x", "/home/u"); got != "~/x" {
		t.Errorf("DisplayDir = %q, want ~/x", got)
	}
	if got := DisplayDir("/etc", "/home/u"); got != "/etc" {
		t.Errorf("DisplayDir = %q, want /etc", got)
	}
	if got := DisplayDir("/etc", ""); got != "/etc" {
		t.Errorf("no home should change nothing: %q", got)
	}
}
