package controller

import (
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

func TestOverwriteText(t *testing.T) {
	t.Run("lists the conflicts and states the scale", func(t *testing.T) {
		got := overwriteText("Copy", []string{"a/x.txt", "a/y.txt"}, 5)
		if !strings.Contains(got, "2 of 5 object(s)") {
			t.Errorf("missing the scale in:\n%s", got)
		}
		for _, k := range []string{"a/x.txt", "a/y.txt"} {
			if !strings.Contains(got, k) {
				t.Errorf("missing %q in:\n%s", k, got)
			}
		}
	})

	t.Run("long lists are summarised, never truncated silently", func(t *testing.T) {
		var keys []string
		for i := 0; i < overwriteRows+4; i++ {
			keys = append(keys, "k"+string(rune('a'+i)))
		}
		got := overwriteText("Paste", keys, len(keys))
		if !strings.Contains(got, "...and 4 more") {
			t.Errorf("dropped rows without saying so:\n%s", got)
		}
	})
}

func TestUnskippedUploads(t *testing.T) {
	files := []model.UploadTarget{
		{LocalPath: "/l/a", RemotePath: "up/a", Size: 10},
		{LocalPath: "/l/b", RemotePath: "up/b", Size: 20},
		{LocalPath: "/l/c", RemotePath: "up/c", Size: 30},
	}

	t.Run("no skips keeps everything and totals the bytes", func(t *testing.T) {
		kept, total := unskippedUploads(files, nil)
		if len(kept) != 3 || total != 60 {
			t.Errorf("got %d files / %d bytes, want 3 / 60", len(kept), total)
		}
	})

	t.Run("skipped files leave the total measuring only what is sent", func(t *testing.T) {
		kept, total := unskippedUploads(files, map[string]bool{"up/b": true})
		if len(kept) != 2 || total != 40 {
			t.Errorf("got %d files / %d bytes, want 2 / 40", len(kept), total)
		}
		for _, f := range kept {
			if f.RemotePath == "up/b" {
				t.Error("skipped file was kept")
			}
		}
	})

	t.Run("skipping everything leaves nothing to do", func(t *testing.T) {
		kept, total := unskippedUploads(files, map[string]bool{"up/a": true, "up/b": true, "up/c": true})
		if len(kept) != 0 || total != 0 {
			t.Errorf("got %d files / %d bytes, want 0 / 0", len(kept), total)
		}
	})
}

func TestKeepUnskippedRenames(t *testing.T) {
	ops := []renameOp{
		{srcKey: "a.txt", dstKey: "x.txt", label: "a→x"},
		{srcKey: "b.txt", dstKey: "y.txt", label: "b→y"},
		{srcKey: "dir/", dstKey: "newdir/", label: "dir→newdir", isFolder: true},
	}

	if got := keepUnskippedRenames(ops, nil); len(got) != 3 {
		t.Errorf("no skips dropped %d op(s)", 3-len(got))
	}

	kept := keepUnskippedRenames(ops, map[string]bool{"y.txt": true})
	if len(kept) != 2 {
		t.Fatalf("got %d ops, want 2", len(kept))
	}
	for _, op := range kept {
		if op.dstKey == "y.txt" {
			t.Error("skipped file rename was kept")
		}
	}

	// A folder op survives a skip of objects underneath it: the per-object
	// skips are applied inside MoveKeys, and dropping the whole op would
	// leave the rest of the folder behind.
	kept = keepUnskippedRenames(ops, map[string]bool{"newdir/inner.txt": true})
	if len(kept) != 3 {
		t.Errorf("a folder rename was dropped by a per-object skip: %d ops left", len(kept))
	}
}

func TestRenameItemsToCopyItems(t *testing.T) {
	items := renameItemsToCopyItems([]renameOp{
		{srcKey: "docs/a.txt", dstKey: "docs/b.txt"},
		{srcKey: "docs/old/", dstKey: "docs/new/", isFolder: true},
	})
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	// The short name must be the destination's last segment: plannedCopyKeys
	// appends it to the prefix to rebuild the destination key.
	if items[0].shortName != "b.txt" || items[0].srcKey != "docs/a.txt" {
		t.Errorf("file item = %+v", items[0])
	}
	if items[1].shortName != "new" || !items[1].isFolder {
		t.Errorf("folder item = %+v", items[1])
	}
}
