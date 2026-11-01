package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteConfigRejectsOutOfRange pins the bounds check. The profiles screen
// captures a list index when the confirm dialog opens and deletes with it
// later, so a stale index must be refused rather than panic the TUI.
func TestDeleteConfigRejectsOutOfRange(t *testing.T) {
	p := newTestParams(t)
	p.Config = []*Config{{Name: "a"}, {Name: "b"}}

	for _, i := range []int{-1, 2, 99} {
		if err := p.DeleteConfig(i); err == nil {
			t.Errorf("DeleteConfig(%d) = nil error, want out-of-range error", i)
		}
		if len(p.Config) != 2 {
			t.Fatalf("DeleteConfig(%d) changed the list: len = %d, want 2", i, len(p.Config))
		}
	}
}

// TestDeleteConfigKeepsListOnWriteFailure covers the desync that makes the
// stale index above reachable: if the entry is dropped in memory but the save
// fails, the on-screen list still shows every profile while the slice is
// shorter, and the next index-addressed action reads out of range.
func TestDeleteConfigKeepsListOnWriteFailure(t *testing.T) {
	p := newTestParams(t)
	p.Config = []*Config{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	// Make the save fail: a directory cannot be replaced by os.Rename of a file.
	if err := os.MkdirAll(p.FileName, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := p.DeleteConfig(2); err == nil {
		t.Fatal("DeleteConfig = nil error, want a write failure")
	}
	if len(p.Config) != 3 {
		t.Fatalf("len(Config) = %d after a failed save, want 3 — the in-memory list must match what the UI shows", len(p.Config))
	}
	if p.Config[2] == nil || p.Config[2].Name != "c" {
		t.Errorf("Config[2] = %v, want the untouched %q entry", p.Config[2], "c")
	}
}

// TestDeleteConfigRemovesEntry is the happy path, so the guards above cannot
// be satisfied by refusing every delete.
func TestDeleteConfigRemovesEntry(t *testing.T) {
	p := newTestParams(t)
	p.Config = []*Config{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	if err := p.DeleteConfig(1); err != nil {
		t.Fatalf("DeleteConfig(1): %v", err)
	}
	if len(p.Config) != 2 || p.Config[0].Name != "a" || p.Config[1].Name != "c" {
		t.Fatalf("after DeleteConfig(1) = %v, want [a c]", configNames(p.Config))
	}
	if _, err := os.Stat(filepath.Dir(p.FileName)); err != nil {
		t.Fatalf("config dir: %v", err)
	}
}

func configNames(cs []*Config) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// TestCopyConfigDoesNotShareBookmarks pins the independence of a duplicated
// profile. The copy is made by value from the original, so without an explicit
// clone the two Bookmarks slices share a backing array: each profile appends
// into the same spare capacity, and the second write silently overwrites the
// first profile's new bookmark while both slices still look the right length.
func TestCopyConfigDoesNotShareBookmarks(t *testing.T) {
	p := newTestParams(t)
	src := &Config{Name: "a", Bookmarks: []Bookmark{{Name: "b1"}, {Name: "b2"}}}
	// Grow once so the slice carries spare capacity, as adding a bookmark at
	// runtime does.
	src.Bookmarks = append(src.Bookmarks, Bookmark{Name: "b3"})
	p.Config = []*Config{src}

	dup := *src // what the profiles screen hands to CopyConfig
	dup.Name = "a-copy"
	if err := p.CopyConfig(dup); err != nil {
		t.Fatalf("CopyConfig: %v", err)
	}
	cp := p.Config[1]

	cp.Bookmarks = append(cp.Bookmarks, Bookmark{Name: "copy-only"})
	src.Bookmarks = append(src.Bookmarks, Bookmark{Name: "src-only"})

	if got := cp.Bookmarks[3].Name; got != "copy-only" {
		t.Errorf("copy's 4th bookmark = %q, want %q — the original's append reached into the copy", got, "copy-only")
	}
	if got := src.Bookmarks[3].Name; got != "src-only" {
		t.Errorf("original's 4th bookmark = %q, want %q", got, "src-only")
	}
}

// TestCopyConfigDoesNotShareRegion covers the other reference field a profile
// carries.
func TestCopyConfigDoesNotShareRegion(t *testing.T) {
	p := newTestParams(t)
	region := "eu-west-1"
	src := &Config{Name: "a", Region: &region}
	p.Config = []*Config{src}

	dup := *src
	dup.Name = "a-copy"
	if err := p.CopyConfig(dup); err != nil {
		t.Fatalf("CopyConfig: %v", err)
	}
	if p.Config[1].Region == src.Region {
		t.Error("the copy shares the original's Region pointer")
	}
	if got := *p.Config[1].Region; got != region {
		t.Errorf("copy's region = %q, want %q", got, region)
	}
}

// --config points the app at another profile list. The default path is
// never touched, so a scratch config cannot clobber the real one.
func TestNewParamsHonoursConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	alt := filepath.Join(t.TempDir(), "team.json")
	if err := os.WriteFile(alt, []byte(`[{"name":"shared","base_url":"http://minio:9000","anonymous":true,"public_buckets":["open"]}]`), 0600); err != nil {
		t.Fatal(err)
	}

	p := NewParams(alt)
	if p.LoadErr != nil {
		t.Fatalf("LoadErr: %v", p.LoadErr)
	}
	if p.FileName != alt {
		t.Errorf("FileName = %q, want %q", p.FileName, alt)
	}
	if len(p.Config) != 1 || p.Config[0].Name != "shared" {
		t.Fatalf("loaded %v, want [shared]", configNames(p.Config))
	}
	if !p.Config[0].Anonymous || len(p.Config[0].PublicBuckets) != 1 || p.Config[0].PublicBuckets[0] != "open" {
		t.Errorf("anonymous fields not loaded: %+v", *p.Config[0])
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "s3duck-tui", "config.json")); !os.IsNotExist(err) {
		t.Errorf("the default config was created alongside --config (stat err %v)", err)
	}

	def := NewParams("")
	if want := filepath.Join(home, ".config", "s3duck-tui", "config.json"); def.FileName != want {
		t.Errorf("default FileName = %q, want %q", def.FileName, want)
	}
}

// A copied profile must not share its public bucket list with the original.
func TestCloneCopiesPublicBuckets(t *testing.T) {
	src := &Config{Name: "a", Anonymous: true, PublicBuckets: []string{"x", "y"}}
	cp := src.clone()
	cp.PublicBuckets[0] = "changed"
	if src.PublicBuckets[0] != "x" {
		t.Errorf("editing the copy changed the original: %v", src.PublicBuckets)
	}
}

// A --config that cannot be read (here a directory) is a load error shown in
// the app, never a crash — and never replaced by an empty file.
func TestNewParamsUnreadableConfigPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	p := NewParams(dir)
	if p.LoadErr == nil {
		t.Fatal("a directory loaded as a profile list")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("the path was replaced: %v %v", fi, err)
	}
}

// A save never writes through a file already sitting at a predictable temp
// name — a planted 0666 file or a symlink would have received every secret.
// The config ends up 0600 and nothing is left beside it.
func TestWriteConfigIgnoresAPlantedTempFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	planted := filepath.Join(dir, "config.json.tmp")
	elsewhere := filepath.Join(t.TempDir(), "stolen")
	if err := os.Symlink(elsewhere, planted); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigBytes(cfgPath, []byte(`[{"name":"p","secret_key":"s3cr3t"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Errorf("the secret was written through the planted symlink (stat err %v)", err)
	}
	fi, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0600 {
		t.Errorf("config.json mode = %v, want a regular 0600 file", fi.Mode())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && e.Name() != "config.json.tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
