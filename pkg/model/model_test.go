package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewConfig(t *testing.T) {
	region := "eu-west-1"
	c := NewConfig("https://example.com", &region, "ak", "sk", true)

	if c.Url != "https://example.com" {
		t.Errorf("Url = %q", c.Url)
	}
	if c.Region == nil || *c.Region != region {
		t.Errorf("Region = %v, want %q", c.Region, region)
	}
	if c.AccessKey != "ak" || c.SecretKey != "sk" {
		t.Errorf("keys not mapped: %+v", c)
	}
	if !c.SSl {
		t.Errorf("SSl = false, want true")
	}

	c2 := NewConfig("u", nil, "", "", false)
	if c2.Region != nil {
		t.Errorf("Region = %v, want nil", c2.Region)
	}
}

// PrepareUpload does not touch the S3 client or the bucket argument, so it is
// exercised here purely against the local filesystem.
//
// Note: PrepareUpload computes remote keys relative to localPath (the selected
// directory itself), i.e. WITHOUT the top-level directory name. model.Upload
// uses a different base (parent of localPath) and DOES include it. This test
// pins PrepareUpload's current contract; the divergence between the two is a
// known issue, not something this test endorses.
func TestPrepareUploadSingleFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "hello.txt")
	content := []byte("hello world")
	if err := os.WriteFile(fp, content, 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("no prefix", func(t *testing.T) {
		targets, total, err := (&Model{}).PrepareUpload(fp, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 {
			t.Fatalf("len(targets) = %d, want 1", len(targets))
		}
		if targets[0].RemotePath != "hello.txt" {
			t.Errorf("RemotePath = %q, want %q", targets[0].RemotePath, "hello.txt")
		}
		if targets[0].Size != int64(len(content)) || total != int64(len(content)) {
			t.Errorf("size/total = %d/%d, want %d", targets[0].Size, total, len(content))
		}
	})

	t.Run("with prefix normalization", func(t *testing.T) {
		targets, _, err := (&Model{}).PrepareUpload(fp, "data", nil)
		if err != nil {
			t.Fatal(err)
		}
		if targets[0].RemotePath != "data/hello.txt" {
			t.Errorf("RemotePath = %q, want %q", targets[0].RemotePath, "data/hello.txt")
		}
	})
}

func TestPrepareUploadDirectory(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.txt", []byte("aa"))              // size 2
	mustWrite("sub/b.txt", []byte("bbbb"))        // size 4
	mustWrite("sub/deep/c.txt", []byte("cccccc")) // size 6

	targets, total, err := (&Model{}).PrepareUpload(root, "pre", nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 {
		t.Errorf("total = %d, want 12", total)
	}

	got := make(map[string]int64, len(targets))
	for _, tg := range targets {
		got[tg.RemotePath] = tg.Size
	}
	want := map[string]int64{
		"pre/a.txt":          2,
		"pre/sub/b.txt":      4,
		"pre/sub/deep/c.txt": 6,
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want keys %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("RemotePath %q size = %d, want %d", k, got[k], v)
		}
	}
}

func TestPrepareUploadMissingPath(t *testing.T) {
	_, _, err := (&Model{}).PrepareUpload(filepath.Join(t.TempDir(), "nope"), "", nil)
	if err == nil {
		t.Errorf("expected error for non-existent path, got nil")
	}
}
