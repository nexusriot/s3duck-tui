package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"/":             "",
		"  ":            "",
		"photos":        "photos/",
		"photos/":       "photos/",
		"a/b":           "a/b/",
		"a/b/":          "a/b/",
		" photos/2024 ": "photos/2024/",
	}
	for in, want := range cases {
		if got := NormalizePrefix(in); got != want {
			t.Errorf("NormalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWalkLocal(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.txt", "hello")
	mustWrite("dir/b.txt", "worldly")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	entries, err := WalkLocal(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })

	var rels []string
	for _, e := range entries {
		rels = append(rels, e.Rel)
	}
	// Directories, including empty ones, are not entries.
	if want := []string{"a.txt", "dir/b.txt"}; !reflect.DeepEqual(rels, want) {
		t.Errorf("got %v, want %v", rels, want)
	}
	if entries[0].Size != 5 || entries[1].Size != 7 {
		t.Errorf("sizes = %d, %d; want 5, 7", entries[0].Size, entries[1].Size)
	}
	if entries[0].Mod.IsZero() {
		t.Error("mtime was not captured")
	}

	t.Run("relative paths use forward slashes", func(t *testing.T) {
		for _, e := range entries {
			if filepath.ToSlash(e.Rel) != e.Rel {
				t.Errorf("%q is not slash-separated", e.Rel)
			}
		}
	})

	t.Run("a missing root is an error", func(t *testing.T) {
		if _, err := WalkLocal(filepath.Join(root, "nope")); err == nil {
			t.Error("want an error for a missing directory")
		}
	})

	t.Run("a file root is an error", func(t *testing.T) {
		if _, err := WalkLocal(filepath.Join(root, "a.txt")); err == nil {
			t.Error("want an error when the root is not a directory")
		}
	})

	t.Run("an empty directory yields no entries and no error", func(t *testing.T) {
		got, err := WalkLocal(filepath.Join(root, "empty"))
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v; want no entries and no error", got, err)
		}
	})
}

func TestDeleteKeyRefusesPrefixes(t *testing.T) {
	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	bucket := &Object{Key: strPtr("b"), Ot: Bucket}

	// These must fail before any network call: a trailing slash would be a
	// recursive prefix in Delete, and sync only ever removes single files.
	if err := m.DeleteKey(context.Background(), "photos/", bucket); err == nil {
		t.Error("want an error for a prefix-like key")
	}
	if err := m.DeleteKey(context.Background(), "", bucket); err == nil {
		t.Error("want an error for an empty key")
	}
	if err := m.DeleteKey(context.Background(), "a.txt", nil); err == nil {
		t.Error("want an error for a nil bucket")
	}
}

func TestUploadFileRejectsBadInput(t *testing.T) {
	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))

	if err := m.UploadFile(context.Background(), "/nonexistent/file", "k", &Object{Key: strPtr("b")}, nil); err == nil {
		t.Error("want an error for a missing local file")
	}
	if err := m.UploadFile(context.Background(), "/etc/hostname", "k", nil, nil); err == nil {
		t.Error("want an error for a nil bucket")
	}
}

// DownloadTarget refuses to clobber an existing file. The guard runs before any
// network call, so this is offline-testable — and sync relies on the sentinel
// being identifiable to tell "already there" apart from a real transfer error.
func TestDownloadTargetReportsExistingFile(t *testing.T) {
	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "a.txt"), []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := m.DownloadTarget(context.Background(),
		DownloadTarget{Key: "pre/a.txt", Size: 8}, "pre/", dst, strPtr("b"), false, nil)

	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("err = %v, want it to wrap ErrFileExists", err)
	}
	if !strings.Contains(err.Error(), "a.txt") {
		t.Errorf("err = %q, want the path included", err)
	}

	got, _ := os.ReadFile(filepath.Join(dst, "a.txt"))
	if string(got) != "existing" {
		t.Errorf("the existing file was modified: %q", got)
	}
}
