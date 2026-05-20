package controller

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func strptr(s string) *string { return &s }

func TestLocalDownloadPath(t *testing.T) {
	tests := []struct {
		name        string
		currentPath string
		destPath    string
		s3Key       string
		want        string
	}{
		{
			name:        "empty prefix keeps full key",
			currentPath: "",
			destPath:    "/dl",
			s3Key:       "foo/bar.txt",
			want:        filepath.Join("/dl", "foo/bar.txt"),
		},
		{
			name:        "prefix without trailing slash is normalized and trimmed",
			currentPath: "sub",
			destPath:    "/dl",
			s3Key:       "sub/foo/bar.txt",
			want:        filepath.Join("/dl", "foo/bar.txt"),
		},
		{
			name:        "prefix with trailing slash trimmed",
			currentPath: "sub/",
			destPath:    "/dl",
			s3Key:       "sub/x",
			want:        filepath.Join("/dl", "x"),
		},
		{
			name:        "key not under prefix is kept as-is",
			currentPath: "a/b",
			destPath:    "/dl",
			s3Key:       "other.txt",
			want:        filepath.Join("/dl", "other.txt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localDownloadPath(tt.currentPath, tt.destPath, tt.s3Key)
			if got != tt.want {
				t.Errorf("localDownloadPath(%q,%q,%q) = %q, want %q",
					tt.currentPath, tt.destPath, tt.s3Key, got, tt.want)
			}
		})
	}
}

func TestGetPosition(t *testing.T) {
	slice := []string{"a", "b", "c"}
	cases := map[string]struct {
		el   string
		want int
	}{
		"first":           {"a", 0},
		"middle":          {"b", 1},
		"last":            {"c", 2},
		"missing -> zero": {"z", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := getPosition(tc.el, slice); got != tc.want {
				t.Errorf("getPosition(%q) = %d, want %d", tc.el, got, tc.want)
			}
		})
	}
}

func TestDetectCategory(t *testing.T) {
	cases := map[string]string{
		"a/b/file.pdf":  "documents",
		"REPORT.PDF":    "documents", // case-insensitive
		"notes.txt":     "documents",
		"backup.zip":    "archives",
		"data.TGZ":      "archives",
		"clip.mp4":      "media",
		"photo.jpeg":    "media",
		"binary":        "other", // no extension
		"thing.xyz":     "other", // unknown extension
		"dir/sub/a.csv": "documents",
	}
	for key, want := range cases {
		if got := detectCategory(key); got != want {
			t.Errorf("detectCategory(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestBuildSummary(t *testing.T) {
	objs := []s3t.Object{
		{Key: strptr("doc.pdf"), Size: 100},
		{Key: strptr("sub/a.zip"), Size: 200},
		{Key: strptr("sub/b.mp4"), Size: 50},
		{Key: strptr("sub/"), Size: 0},     // folder marker, ignored
		{Key: nil, Size: 999},              // nil key, ignored
		{Key: strptr("neg.bin"), Size: -5}, // negative size clamped to 0
	}

	total, cats, groups := buildSummary(objs, "")

	if total != 350 {
		t.Errorf("total = %d, want 350", total)
	}

	wantCats := map[string]int64{
		"Documents": 100,
		"Archives":  200,
		"Media":     50,
		"Other":     0,
	}
	for _, row := range cats {
		if w, ok := wantCats[row.Name]; !ok || w != row.Bytes {
			t.Errorf("category %s = %d, want %d", row.Name, row.Bytes, wantCats[row.Name])
		}
	}

	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	// Sorted by Bytes desc: "sub/" (250) before "(root)" (100).
	if groups[0].Name != "sub/" || groups[0].Bytes != 250 {
		t.Errorf("groups[0] = %+v, want {sub/ 250}", groups[0])
	}
	if groups[1].Name != "(root)" || groups[1].Bytes != 100 {
		t.Errorf("groups[1] = %+v, want {(root) 100}", groups[1])
	}
}

func TestBuildSummaryGroupCap(t *testing.T) {
	var objs []s3t.Object
	for i := 0; i < 15; i++ {
		objs = append(objs, s3t.Object{
			Key:  strptr(fmt.Sprintf("g%02d/file.bin", i)),
			Size: int64(i + 1),
		})
	}
	_, _, groups := buildSummary(objs, "")
	if len(groups) != 10 {
		t.Errorf("len(groups) = %d, want 10 (top-10 cap)", len(groups))
	}
	for i := 1; i < len(groups); i++ {
		if groups[i-1].Bytes < groups[i].Bytes {
			t.Errorf("groups not sorted desc by Bytes at %d: %d < %d",
				i, groups[i-1].Bytes, groups[i].Bytes)
		}
	}
}

func TestDownloadSummaryText(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		s := downloadSummary{totalObjects: 3, downloaded: 3}
		out := s.text(123, false)
		if !strings.Contains(out, "Download complete.") {
			t.Errorf("missing success status in:\n%s", out)
		}
		if !strings.Contains(out, "Objects: 3 total") {
			t.Errorf("missing object count in:\n%s", out)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		s := downloadSummary{totalObjects: 1}
		if !strings.Contains(s.text(0, true), "Download canceled.") {
			t.Errorf("canceled status not reported")
		}
	})

	t.Run("with errors", func(t *testing.T) {
		s := downloadSummary{totalObjects: 1}
		s.addFailed("k", errors.New("boom"))
		out := s.text(0, false)
		if !strings.Contains(out, "Download finished with errors.") {
			t.Errorf("error status not reported in:\n%s", out)
		}
		if !strings.Contains(out, "k: boom") {
			t.Errorf("failed item detail missing in:\n%s", out)
		}
	})
}

func TestDownloadSummaryCaps(t *testing.T) {
	var s downloadSummary
	for i := 0; i < 10; i++ {
		s.addSkipped(fmt.Sprintf("/path/%d", i))
		s.addFailed(fmt.Sprintf("key%d", i), errors.New("err"))
	}

	if s.skipped != 10 {
		t.Errorf("skipped counter = %d, want 10", s.skipped)
	}
	if len(s.skippedPaths) != 8 {
		t.Errorf("len(skippedPaths) = %d, want 8 (capped)", len(s.skippedPaths))
	}
	if s.failed != 10 {
		t.Errorf("failed counter = %d, want 10", s.failed)
	}
	if len(s.failedItems) != 8 {
		t.Errorf("len(failedItems) = %d, want 8 (capped)", len(s.failedItems))
	}

	out := s.text(0, false)
	if !strings.Contains(out, "...and 2 more") {
		t.Errorf("expected overflow note '...and 2 more' in:\n%s", out)
	}
}
