package model

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ts(offset time.Duration) *time.Time {
	v := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Add(offset)
	return &v
}

func TestSortVersions(t *testing.T) {
	t.Run("newest first", func(t *testing.T) {
		vs := []ObjectVersion{
			{VersionID: "old", LastModified: ts(-2 * time.Hour)},
			{VersionID: "new", LastModified: ts(0)},
			{VersionID: "mid", LastModified: ts(-time.Hour)},
		}
		SortVersions(vs)
		if vs[0].VersionID != "new" || vs[1].VersionID != "mid" || vs[2].VersionID != "old" {
			t.Errorf("got %s,%s,%s", vs[0].VersionID, vs[1].VersionID, vs[2].VersionID)
		}
	})

	t.Run("versions without a timestamp sort last", func(t *testing.T) {
		vs := []ObjectVersion{
			{VersionID: "no-date"},
			{VersionID: "dated", LastModified: ts(-time.Hour)},
		}
		SortVersions(vs)
		if vs[0].VersionID != "dated" || vs[1].VersionID != "no-date" {
			t.Errorf("got %s,%s", vs[0].VersionID, vs[1].VersionID)
		}
	})

	t.Run("equal timestamps break ties on version id, stably", func(t *testing.T) {
		same := ts(0)
		vs := []ObjectVersion{
			{VersionID: "b", LastModified: same},
			{VersionID: "a", LastModified: same},
		}
		SortVersions(vs)
		if vs[0].VersionID != "a" || vs[1].VersionID != "b" {
			t.Errorf("got %s,%s", vs[0].VersionID, vs[1].VersionID)
		}
	})

	t.Run("empty and single-element inputs are fine", func(t *testing.T) {
		SortVersions(nil)
		one := []ObjectVersion{{VersionID: "x"}}
		SortVersions(one)
		if one[0].VersionID != "x" {
			t.Error("single element was disturbed")
		}
	})
}

func TestVersionCopySource(t *testing.T) {
	got := versionCopySource("my-bucket", "dir/file name.txt", "abc123")
	if !strings.HasPrefix(got, "my-bucket/") {
		t.Errorf("got %q, want a bucket-prefixed source", got)
	}
	if !strings.Contains(got, "?versionId=abc123") {
		t.Errorf("got %q, want the version pinned", got)
	}
	// The key must stay percent-encoded the way copySource produced it.
	if strings.Contains(got, " ") {
		t.Errorf("got %q, want the space escaped", got)
	}
}

func TestVersionFileName(t *testing.T) {
	cases := []struct{ key, version, want string }{
		{"dir/report.pdf", "abcdefghijklmnop", "report.abcdefgh.pdf"},
		{"report.pdf", "short", "report.short.pdf"},
		{"noext", "abcdefghij", "noext.abcdefgh"},
		{"dir/archive.tar.gz", "12345678", "archive.tar.12345678.gz"},
		{"file.txt", "null", "file.null.txt"},
		{"file.txt", "", "file.null.txt"},
	}
	for _, c := range cases {
		if got := VersionFileName(c.key, c.version); got != c.want {
			t.Errorf("VersionFileName(%q, %q) = %q, want %q", c.key, c.version, got, c.want)
		}
	}
}

func TestVersionGuards(t *testing.T) {
	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	ctx := context.Background()
	b := &Object{Key: strPtr("bucket"), Ot: Bucket}

	if _, err := m.ListVersions(ctx, nil, "k"); err == nil {
		t.Error("nil bucket: want error")
	}
	if _, err := m.ListVersions(ctx, b, ""); err == nil {
		t.Error("empty key: want error")
	}
	if err := m.RestoreVersion(ctx, b, "k", "", ""); err == nil {
		t.Error("empty version: want error")
	}
	if err := m.DeleteVersion(ctx, b, "", "v"); err == nil {
		t.Error("empty key: want error")
	}
	if err := m.DeleteVersion(ctx, nil, "k", "v"); err == nil {
		t.Error("nil bucket: want error")
	}
}
