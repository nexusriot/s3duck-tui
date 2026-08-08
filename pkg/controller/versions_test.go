package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

func vtime(offset time.Duration) *time.Time {
	v := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Add(offset)
	return &v
}

func TestVersionRow(t *testing.T) {
	t.Run("an ordinary version shows date and size", func(t *testing.T) {
		primary, secondary := versionRow(model.ObjectVersion{
			VersionID: "v1", Size: 2048, LastModified: vtime(0),
		})
		if secondary != "v1" {
			t.Errorf("secondary = %q, want the version id", secondary)
		}
		for _, want := range []string{"2026-07-30", "2.0 KiB"} {
			if !strings.Contains(primary, want) {
				t.Errorf("row missing %q: %s", want, primary)
			}
		}
	})

	t.Run("the latest version is marked", func(t *testing.T) {
		primary, _ := versionRow(model.ObjectVersion{VersionID: "v", Size: 1, LastModified: vtime(0), IsLatest: true})
		if !strings.Contains(primary, "latest") || !strings.Contains(primary, "→") {
			t.Errorf("row = %s", primary)
		}
	})

	t.Run("a delete marker is called out and has no size", func(t *testing.T) {
		primary, _ := versionRow(model.ObjectVersion{VersionID: "dm", LastModified: vtime(0), IsDeleteMark: true})
		if !strings.Contains(primary, "delete marker") {
			t.Errorf("row = %s", primary)
		}
		if strings.Contains(primary, "B") && strings.Contains(primary, "0 B") {
			t.Errorf("a delete marker should not show a size: %s", primary)
		}
	})

	t.Run("a missing timestamp degrades gracefully", func(t *testing.T) {
		primary, _ := versionRow(model.ObjectVersion{VersionID: "v", Size: 1})
		if !strings.Contains(primary, "unknown date") {
			t.Errorf("row = %s", primary)
		}
	})

	t.Run("a non-standard storage class is shown", func(t *testing.T) {
		primary, _ := versionRow(model.ObjectVersion{VersionID: "v", Size: 1, LastModified: vtime(0), StorageClass: "GLACIER"})
		if !strings.Contains(primary, "GLACIER") {
			t.Errorf("row = %s", primary)
		}
		plain, _ := versionRow(model.ObjectVersion{VersionID: "v", Size: 1, LastModified: vtime(0), StorageClass: "STANDARD"})
		if strings.Contains(plain, "STANDARD") {
			t.Errorf("STANDARD is the default and should stay implicit: %s", plain)
		}
	})
}

func TestVersionsTitle(t *testing.T) {
	vs := []model.ObjectVersion{
		{VersionID: "a"},
		{VersionID: "b", IsDeleteMark: true},
		{VersionID: "c", IsDeleteMark: true},
	}
	got := versionsTitle("file.txt", vs)
	for _, want := range []string{"file.txt", "3 total", "2 delete marker(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("title missing %q: %s", want, got)
		}
	}

	clean := versionsTitle("file.txt", []model.ObjectVersion{{VersionID: "a"}})
	if strings.Contains(clean, "delete marker") {
		t.Errorf("no markers should mean no marker note: %s", clean)
	}
}
