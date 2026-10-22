package controller

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
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
			got, err := model.SafeLocalPath(tt.destPath, tt.currentPath, tt.s3Key)
			if err != nil {
				t.Fatalf("model.SafeLocalPath(%q,%q,%q) unexpected error: %v",
					tt.currentPath, tt.destPath, tt.s3Key, err)
			}
			if got != tt.want {
				t.Errorf("model.SafeLocalPath(%q,%q,%q) = %q, want %q",
					tt.currentPath, tt.destPath, tt.s3Key, got, tt.want)
			}
		})
	}
}

// TestLocalDownloadPathTraversal pins the zip-slip guard: S3 keys may legally
// contain ".." segments, and an unchecked filepath.Join would resolve them
// OUTSIDE the chosen download directory.
func TestLocalDownloadPathTraversal(t *testing.T) {
	bad := []struct {
		name        string
		currentPath string
		s3Key       string
	}{
		{"plain dotdot", "", "../evil.txt"},
		{"nested dotdot escaping", "photos", "photos/../../../home/user/.bashrc"},
		{"dotdot after prefix trim", "a/", "a/../../x"},
		{"marker key escaping", "", "../outside/"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			got, err := model.SafeLocalPath("/dl/dest", tt.currentPath, tt.s3Key)
			if err == nil {
				t.Fatalf("model.SafeLocalPath(%q,%q) = %q, want traversal error",
					tt.currentPath, tt.s3Key, got)
			}
		})
	}

	// Keys that merely CONTAIN dot segments but stay inside are fine.
	good := []struct {
		name  string
		s3Key string
		want  string
	}{
		{"inner dotdot that stays local", "a/../b.txt", filepath.Join("/dl/dest", "b.txt")},
		{"leading slash is contained", "/etc/passwd", filepath.Join("/dl/dest", "etc/passwd")},
	}
	for _, tt := range good {
		t.Run(tt.name, func(t *testing.T) {
			got, err := model.SafeLocalPath("/dl/dest", "", tt.s3Key)
			if err != nil {
				t.Fatalf("model.SafeLocalPath(%q): unexpected error: %v", tt.s3Key, err)
			}
			if got != tt.want {
				t.Errorf("model.SafeLocalPath(%q) = %q, want %q", tt.s3Key, got, tt.want)
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
	// Ten named prefixes plus one row accounting for the rest: the cap used
	// to truncate silently, so a bucket with more prefixes than that showed
	// ten and looked complete.
	if len(groups) != summaryGroupRows+1 {
		t.Fatalf("len(groups) = %d, want %d (top-10 plus the overflow row)", len(groups), summaryGroupRows+1)
	}
	named := groups[:summaryGroupRows]
	for i := 1; i < len(named); i++ {
		if named[i-1].Bytes < named[i].Bytes {
			t.Errorf("groups not sorted desc by Bytes at %d: %d < %d",
				i, named[i-1].Bytes, named[i].Bytes)
		}
	}
	// Sizes are 1..15, so the ten largest are 6..15 and the overflow row
	// must account for exactly 1+2+3+4+5.
	over := groups[summaryGroupRows]
	if over.Bytes != 15 {
		t.Errorf("overflow row = %d bytes, want 15", over.Bytes)
	}
	if !strings.Contains(over.Name, "5 more") {
		t.Errorf("overflow row name = %q, should say how many are folded in", over.Name)
	}
}

func TestFilterSortObjects(t *testing.T) {
	obj := func(name string, ot model.FType) *model.Object {
		n := name
		return &model.Object{Key: &n, Ot: ot}
	}
	objs := []*model.Object{
		obj("Zeta.txt", model.File),
		obj("alpha", model.Folder),
		obj("Beta.txt", model.File),
		obj("gamma", model.Folder),
		nil,              // nil element skipped
		{Ot: model.File}, // nil Key skipped
	}

	names := func(in []*model.Object) []string {
		out := make([]string, 0, len(in))
		for _, o := range in {
			out = append(out, *o.Key)
		}
		return out
	}

	t.Run("empty filter returns all, folders first then name", func(t *testing.T) {
		got := names(filterSortObjects(objs, "", sortName, false))
		want := []string{"alpha", "gamma", "Beta.txt", "Zeta.txt"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("case-insensitive substring on the display name", func(t *testing.T) {
		got := names(filterSortObjects(objs, "ETA", sortName, false)) // Beta.txt, Zeta.txt
		want := []string{"Beta.txt", "Zeta.txt"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		if got := filterSortObjects(objs, "nomatch", sortName, false); len(got) != 0 {
			t.Errorf("got %d results, want 0", len(got))
		}
	})

	t.Run("does not mutate input order", func(t *testing.T) {
		_ = filterSortObjects(objs, "", sortName, false)
		if objs[0] == nil || *objs[0].Key != "Zeta.txt" {
			t.Errorf("input slice was reordered")
		}
	})
}

func TestParentPrefix(t *testing.T) {
	cases := map[string]string{
		"a/b/c.txt":              "a/b/",
		"c.txt":                  "",
		"a/b/":                   "a/", // folder marker -> parent folder
		"a/":                     "",
		"":                       "",
		"one/two/three/four.bin": "one/two/three/",
	}
	for in, want := range cases {
		if got := parentPrefix(in); got != want {
			t.Errorf("parentPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchMatch(t *testing.T) {
	cases := []struct {
		key, query string
		want       bool
	}{
		{"photos/2024/Vacation.JPG", "vacation", true}, // case-insensitive
		{"photos/2024/vacation.jpg", "PHOTOS", true},   // matches a path segment
		{"a/b/report.pdf", "xml", false},
		{"a/b/", "b", false},   // folder marker never matches
		{"", "x", false},       // empty key
		{"file.txt", "", true}, // empty query matches any real key
	}
	for _, tc := range cases {
		if got := searchMatch(tc.key, tc.query); got != tc.want {
			t.Errorf("searchMatch(%q,%q) = %v, want %v", tc.key, tc.query, got, tc.want)
		}
	}
}

func TestComputeHits(t *testing.T) {
	objs := []s3t.Object{
		{Key: strptr("docs/a.txt"), Size: 10},
		{Key: strptr("docs/"), Size: 0}, // folder marker never matches
		{Key: strptr("docs/notes/b.txt"), Size: 20},
		{Key: strptr("images/c.png"), Size: 30},
		{Key: nil, Size: 99}, // nil key skipped
	}

	t.Run("matches by substring, excludes markers and nil", func(t *testing.T) {
		hits, trunc := computeHits(objs, "txt", 0)
		if trunc {
			t.Errorf("unexpected truncation")
		}
		if len(hits) != 2 {
			t.Fatalf("len(hits) = %d, want 2", len(hits))
		}
		if hits[0].key != "docs/a.txt" || hits[0].size != 10 {
			t.Errorf("hits[0] = %+v, want {docs/a.txt 10}", hits[0])
		}
		if hits[1].key != "docs/notes/b.txt" || hits[1].size != 20 {
			t.Errorf("hits[1] = %+v, want {docs/notes/b.txt 20}", hits[1])
		}
	})

	t.Run("truncates at max and reports it", func(t *testing.T) {
		hits, trunc := computeHits(objs, "", 2) // "" matches every real key
		if !trunc {
			t.Errorf("expected truncation at max=2")
		}
		if len(hits) != 2 {
			t.Errorf("len(hits) = %d, want 2", len(hits))
		}
	})
}

func TestApplyRenamePattern(t *testing.T) {
	cases := []struct {
		name          string
		orig, pattern string
		find, replace string
		index, total  int
		want          string
	}{
		{"identity", "photo.jpg", "{name}{ext}", "", "", 0, 1, "photo.jpg"},
		{"numbering padded to total width", "a.txt", "img_{n}{ext}", "", "", 4, 10, "img_05.txt"},
		{"no extension", "README", "{name}_{n}", "", "", 0, 5, "README_1"},
		{"only last extension is split", "x.tar.gz", "{name}{ext}", "", "", 0, 1, "x.tar.gz"},
		{"find/replace on the base only", "IMG_1234.png", "{name}{ext}", "IMG", "photo", 0, 1, "photo_1234.png"},
		{"literal pattern ignores tokens absent", "a.txt", "fixed{ext}", "", "", 0, 1, "fixed.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyRenamePattern(tc.orig, tc.pattern, tc.find, tc.replace, tc.index, tc.total)
			if got != tc.want {
				t.Errorf("applyRenamePattern(%q,%q,find=%q,repl=%q,%d,%d) = %q, want %q",
					tc.orig, tc.pattern, tc.find, tc.replace, tc.index, tc.total, got, tc.want)
			}
		})
	}
}

func TestPlanBatchRename(t *testing.T) {
	items := []renameItem{
		{shortName: "a.txt", srcKey: "dir/a.txt", isFolder: false},
		{shortName: "b.txt", srcKey: "dir/b.txt", isFolder: false},
	}

	t.Run("prefix add", func(t *testing.T) {
		ops, err := planBatchRename(items, "dir/", "x_{name}{ext}", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ops) != 2 || ops[0].dstKey != "dir/x_a.txt" || ops[1].dstKey != "dir/x_b.txt" {
			t.Errorf("ops = %+v", ops)
		}
	})

	t.Run("all no-op errors", func(t *testing.T) {
		if _, err := planBatchRename(items, "dir/", "{name}{ext}", "", ""); err == nil {
			t.Errorf("expected 'no names changed' error")
		}
	})

	t.Run("slash rejected", func(t *testing.T) {
		if _, err := planBatchRename(items, "dir/", "{name}/{n}", "", ""); err == nil {
			t.Errorf("expected error for '/' in name")
		}
	})

	t.Run("duplicate target rejected", func(t *testing.T) {
		if _, err := planBatchRename(items, "dir/", "same.txt", "", ""); err == nil {
			t.Errorf("expected duplicate-target error")
		}
	})

	t.Run("collision with another source rejected", func(t *testing.T) {
		// find a->b turns a.txt into b.txt, which is b.txt's source key.
		if _, err := planBatchRename(items, "dir/", "{name}{ext}", "a", "b"); err == nil {
			t.Errorf("expected collision-with-source error")
		}
	})

	t.Run("folder target keeps trailing slash", func(t *testing.T) {
		folders := []renameItem{{shortName: "old", srcKey: "old/", isFolder: true}}
		ops, err := planBatchRename(folders, "", "new", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ops) != 1 || ops[0].dstKey != "new/" {
			t.Errorf("ops = %+v, want dstKey new/", ops)
		}
	})
}

func TestAppendCapped(t *testing.T) {
	var list []activityEntry
	for i := 0; i < 5; i++ {
		list = appendCapped(list, activityEntry{msg: fmt.Sprintf("e%d", i)}, 3)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3 (capped)", len(list))
	}
	// Oldest dropped; newest kept in order: e2, e3, e4.
	if list[0].msg != "e2" || list[2].msg != "e4" {
		t.Errorf("kept %q..%q, want e2..e4", list[0].msg, list[2].msg)
	}
	// max <= 0 means unbounded.
	list = appendCapped(list, activityEntry{msg: "x"}, 0)
	if len(list) != 4 {
		t.Errorf("len = %d, want 4 (uncapped)", len(list))
	}
}

func TestSubsequence(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"download", "dl", true},
		{"download", "dwn", true}, // in order, not contiguous
		{"download", "", true},    // empty always matches
		{"download", "z", false},
		{"download", "dload", true},
		{"abc", "abcd", false}, // sub longer than s
		{"rén", "én", true},    // rune-safe (é is multi-byte)
	}
	for _, tc := range cases {
		if got := subsequence(tc.s, tc.sub); got != tc.want {
			t.Errorf("subsequence(%q,%q) = %v, want %v", tc.s, tc.sub, got, tc.want)
		}
	}
}

func TestFilterActions(t *testing.T) {
	acts := []paletteAction{
		{label: "Download"},
		{label: "Delete"},
		{label: "Rename"},
		{label: "Recursive search"},
	}
	labels := func(in []paletteAction) []string {
		out := make([]string, 0, len(in))
		for _, a := range in {
			out = append(out, a.label)
		}
		return out
	}

	if got := filterActions(acts, ""); len(got) != 4 {
		t.Errorf("empty query returned %d, want all 4", len(got))
	}
	if got := labels(filterActions(acts, "ren")); !reflect.DeepEqual(got, []string{"Rename"}) {
		t.Errorf(`"ren" -> %v, want [Rename]`, got)
	}
	if got := labels(filterActions(acts, "REN")); !reflect.DeepEqual(got, []string{"Rename"}) {
		t.Errorf(`"REN" (case-insensitive) -> %v, want [Rename]`, got)
	}
	// Subsequence match, not substring: d..w..n in "Download".
	if got := labels(filterActions(acts, "dwn")); !reflect.DeepEqual(got, []string{"Download"}) {
		t.Errorf(`"dwn" subsequence -> %v, want [Download]`, got)
	}
	if got := filterActions(acts, "zzz"); len(got) != 0 {
		t.Errorf(`"zzz" -> %d results, want 0`, len(got))
	}
}

func TestSwapPane(t *testing.T) {
	c := &Controller{view: widgets.NewView(), panes: [2]*session.Pane{session.New(), session.New()}}

	bA := &model.Object{Key: strptr("A"), Ot: model.Bucket}
	bB := &model.Object{Key: strptr("B"), Ot: model.Bucket}

	c.panes[0].Enter(bA, "p0/", "r0")
	c.panes[0].SetFilter("f0")
	c.panes[1].Enter(bB, "p1/", "r1")
	c.panes[1].SetFilter("f1")

	c.swapPane()

	// Swapping now changes which pane is addressed; nothing is copied, so
	// neither pane's contents can be lost or half-written by a concurrent
	// navigation.
	if c.active != 1 {
		t.Fatalf("active = %d, want 1", c.active)
	}
	if got := c.pane(); got != c.panes[1] {
		t.Errorf("pane() returned the wrong pane after a swap")
	}
	if c.pane().Bucket() != bB || c.pane().Path() != "p1/" || c.pane().Filter() != "f1" {
		t.Errorf("active pane = %v/%q/%q, want B/p1//f1",
			c.pane().Bucket(), c.pane().Path(), c.pane().Filter())
	}
	if c.other() != c.panes[0] || c.other().Bucket() != bA {
		t.Errorf("other() should be pane 0, still holding bucket A")
	}
	if c.view.List != c.view.PaneList(1) || c.view.Filter != c.view.PaneFilter(1) {
		t.Errorf("view widgets not repointed to pane 1")
	}

	c.swapPane()
	if c.active != 0 || c.pane().Bucket() != bA || c.pane().Path() != "p0/" || c.pane().Filter() != "f0" {
		t.Errorf("round-trip did not return to pane 0 intact")
	}
	if c.view.List != c.view.PaneList(0) {
		t.Errorf("widgets.List not repointed back to pane 0")
	}
}

// A navigation that captured its pane must land there even if the user Tabs
// away while it is in flight. This is the "navigation race" from DESIGN.md:
// the old code wrote the controller's live fields, which a swap moved under it.
func TestCapturedPaneSurvivesASwap(t *testing.T) {
	c := &Controller{view: widgets.NewView(), panes: [2]*session.Pane{session.New(), session.New()}}
	bA := &model.Object{Key: strptr("A"), Ot: model.Bucket}

	pane := c.pane() // what Down/jumpTo/navigateTo capture before spawning
	c.swapPane()     // the user hits Tab while the listing is still running
	pane.Enter(bA, "deep/", "..")

	if c.panes[0].Bucket() != bA || c.panes[0].Path() != "deep/" {
		t.Errorf("navigation did not land in the pane it started from")
	}
	if c.panes[1].Bucket() != nil {
		t.Errorf("navigation leaked into the pane the user switched to: %v", c.panes[1].Bucket())
	}
}

func TestResetPanes(t *testing.T) {
	c := &Controller{view: widgets.NewView(), panes: [2]*session.Pane{session.New(), session.New()}}
	// Dirty dual-pane state on pane 1.
	c.dual = true
	c.active = 1
	c.pane1Init = true
	c.pane().SetFilter("stale")
	c.pane().SetLocalDir("/tmp/x")
	c.pane().Hist().Record(session.Visit{Path: "h"})
	c.view.List = c.view.PaneList(1)
	c.view.Filter = c.view.PaneFilter(1)

	c.resetPanes()

	if c.dual || c.active != 0 || c.pane1Init {
		t.Errorf("flags not reset: dual=%v active=%d pane1Init=%v", c.dual, c.active, c.pane1Init)
	}
	if c.view.List != c.view.PaneList(0) || c.view.Filter != c.view.PaneFilter(0) {
		t.Errorf("view widgets not repointed to pane 0")
	}
	for i, p := range c.panes {
		if p == nil {
			t.Fatalf("pane %d is nil after reset", i)
		}
		if p.Filter() != "" {
			t.Errorf("pane %d filter not cleared: %q", i, p.Filter())
		}
		if p.LocalDir() != "" {
			t.Errorf("pane %d still local after a profile switch: %q", i, p.LocalDir())
		}
		if back, fwd := p.Hist().Depths(); back != 0 || fwd != 0 {
			t.Errorf("pane %d history not cleared: back=%d fwd=%d", i, back, fwd)
		}
		// A fresh pane must accept a mark without panicking on a nil map.
		p.ToggleSelected("x")
	}
}

func TestInvertOps(t *testing.T) {
	bA := &model.Object{Key: strptr("A"), Ot: model.Bucket}
	bB := &model.Object{Key: strptr("B"), Ot: model.Bucket}
	fwd := []transferPair{
		{srcBucket: bA, dstBucket: bB, srcKey: "a/x.txt", dstKey: "b/x.txt", isFolder: false},
		{srcBucket: bA, dstBucket: bA, srcKey: "old/", dstKey: "new/", isFolder: true},
	}
	inv := invertOps(fwd)
	if len(inv) != 2 {
		t.Fatalf("len = %d, want 2", len(inv))
	}
	// Each inverted op swaps src/dst so applying it moves the object back.
	if inv[0].srcBucket != bB || inv[0].dstBucket != bA || inv[0].srcKey != "b/x.txt" || inv[0].dstKey != "a/x.txt" {
		t.Errorf("inv[0] = %+v", inv[0])
	}
	if inv[1].srcKey != "new/" || inv[1].dstKey != "old/" || !inv[1].isFolder {
		t.Errorf("inv[1] = %+v", inv[1])
	}
	// invert twice is identity.
	back := invertOps(inv)
	same := func(a, b transferPair) bool {
		return a.srcBucket == b.srcBucket && a.dstBucket == b.dstBucket &&
			a.srcKey == b.srcKey && a.dstKey == b.dstKey && a.isFolder == b.isFolder
	}
	if !same(back[0], fwd[0]) || !same(back[1], fwd[1]) {
		t.Errorf("double invert not identity: %+v", back)
	}
}

// TestUndoSkipLeavesKeptObjectsAlone pins the undo of a move that skipped some
// destinations. The kept destination still holds what was there before and its
// source never moved, so the undo must skip the source key: reversing the
// folder wholesale copied the pre-existing destination object over the kept
// source and then deleted it.
func TestUndoSkipLeavesKeptObjectsAlone(t *testing.T) {
	b := &model.Object{Key: strptr("b"), Ot: model.Bucket}
	fwd := []transferPair{{
		srcBucket: b, dstBucket: b,
		srcKey: "from/d/", dstKey: "to/d/", isFolder: true,
		// The run's whole skip set: one key under this folder, one belonging
		// to another item of the same run.
		skip: map[string]bool{"to/d/x.txt": true, "to/other.txt": true},
	}}
	inv := invertOps(fwd)
	if got := inv[0].skip; len(got) != 1 || !got["from/d/x.txt"] {
		t.Errorf("undo skip = %v, want exactly from/d/x.txt", got)
	}

	// A file pair carries no undo skip: a kept file is never recorded.
	file := invertOps([]transferPair{{srcBucket: b, dstBucket: b, srcKey: "a", dstKey: "b",
		skip: map[string]bool{"b": true}}})
	if file[0].skip != nil {
		t.Errorf("file undo skip = %v, want nil", file[0].skip)
	}
}

func TestClipItems(t *testing.T) {
	obj := func(name, full string, ot model.FType) *model.Object {
		n, f := name, full
		return &model.Object{Key: &n, FullPath: &f, Ot: ot}
	}
	bucket := &model.Object{Key: strptr("b"), Ot: model.Bucket} // no FullPath
	objs := []*model.Object{
		obj("file.txt", "dir/file.txt", model.File),
		obj("sub", "dir/sub/", model.Folder),
		bucket, // skipped (not file/folder, nil FullPath)
		nil,    // skipped
	}
	items := clipItems(objs)
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if items[0].srcKey != "dir/file.txt" || items[0].isFolder {
		t.Errorf("items[0] = %+v", items[0])
	}
	if items[1].srcKey != "dir/sub/" || !items[1].isFolder {
		t.Errorf("items[1] = %+v", items[1])
	}
}

func TestAddBookmark(t *testing.T) {
	var list []cfg.Bookmark
	list, added := addBookmark(list, cfg.Bookmark{Name: "a", Bucket: "b1", Prefix: "p/"})
	if !added || len(list) != 1 {
		t.Fatalf("first add: added=%v len=%d", added, len(list))
	}
	// Same bucket+prefix (even with a different name) is a duplicate.
	list, added = addBookmark(list, cfg.Bookmark{Name: "different", Bucket: "b1", Prefix: "p/"})
	if added || len(list) != 1 {
		t.Errorf("duplicate add: added=%v len=%d", added, len(list))
	}
	// Different prefix is a new bookmark.
	list, added = addBookmark(list, cfg.Bookmark{Name: "c", Bucket: "b1", Prefix: "q/"})
	if !added || len(list) != 2 {
		t.Errorf("distinct add: added=%v len=%d", added, len(list))
	}
}

func TestFilterSortObjectsOrdering(t *testing.T) {
	ts := func(offset time.Duration) *time.Time {
		v := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
		return &v
	}
	size := func(n int64) *int64 { return &n }
	file := func(name string, n int64, offset time.Duration) *model.Object {
		return &model.Object{Key: strptr(name), Ot: model.File, Size: size(n), LastModified: ts(offset)}
	}
	objs := []*model.Object{
		file("big.bin", 300, 3*time.Hour),
		file("small.txt", 100, time.Hour),
		file("mid.txt", 200, 2*time.Hour),
		{Key: strptr("zdir"), Ot: model.Folder},
		{Key: strptr("adir"), Ot: model.Folder},
	}
	names := func(in []*model.Object) []string {
		out := make([]string, 0, len(in))
		for _, o := range in {
			out = append(out, *o.Key)
		}
		return out
	}

	t.Run("folders stay first whatever the key or direction", func(t *testing.T) {
		for _, key := range []sortKey{sortName, sortSize, sortDate} {
			for _, desc := range []bool{false, true} {
				got := names(filterSortObjects(objs, "", key, desc))
				if got[0] != "adir" && got[0] != "zdir" {
					t.Errorf("key=%v desc=%v: got %v, want a folder first", key, desc, got)
				}
				if got[1] != "adir" && got[1] != "zdir" {
					t.Errorf("key=%v desc=%v: got %v, want both folders first", key, desc, got)
				}
			}
		}
	})

	t.Run("size ascending and descending", func(t *testing.T) {
		got := names(filterSortObjects(objs, "", sortSize, false))[2:]
		if want := []string{"small.txt", "mid.txt", "big.bin"}; !reflect.DeepEqual(got, want) {
			t.Errorf("asc: got %v, want %v", got, want)
		}
		got = names(filterSortObjects(objs, "", sortSize, true))[2:]
		if want := []string{"big.bin", "mid.txt", "small.txt"}; !reflect.DeepEqual(got, want) {
			t.Errorf("desc: got %v, want %v", got, want)
		}
	})

	t.Run("date ascending and descending", func(t *testing.T) {
		got := names(filterSortObjects(objs, "", sortDate, false))[2:]
		if want := []string{"small.txt", "mid.txt", "big.bin"}; !reflect.DeepEqual(got, want) {
			t.Errorf("asc: got %v, want %v", got, want)
		}
		got = names(filterSortObjects(objs, "", sortDate, true))[2:]
		if want := []string{"big.bin", "mid.txt", "small.txt"}; !reflect.DeepEqual(got, want) {
			t.Errorf("desc: got %v, want %v", got, want)
		}
	})

	t.Run("name descending reverses the files", func(t *testing.T) {
		got := names(filterSortObjects(objs, "", sortName, true))[2:]
		if want := []string{"small.txt", "mid.txt", "big.bin"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("equal keys tie-break on name ascending in both directions", func(t *testing.T) {
		// Folders carry no size or timestamp, so every folder pair is a tie.
		for _, desc := range []bool{false, true} {
			got := names(filterSortObjects(objs, "dir", sortSize, desc))
			if want := []string{"adir", "zdir"}; !reflect.DeepEqual(got, want) {
				t.Errorf("desc=%v: got %v, want %v", desc, got, want)
			}
		}
	})

	t.Run("filter still applies under every key", func(t *testing.T) {
		got := names(filterSortObjects(objs, ".txt", sortSize, false))
		if want := []string{"small.txt", "mid.txt"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestSortKeyCycleAndLabel(t *testing.T) {
	if got := sortName.next(); got != sortSize {
		t.Errorf("name.next() = %v, want size", got)
	}
	if got := sortSize.next(); got != sortDate {
		t.Errorf("size.next() = %v, want date", got)
	}
	if got := sortDate.next(); got != sortName {
		t.Errorf("date.next() = %v, want name (wraps)", got)
	}
	if got := sortLabel(sortSize, true); got != "sort:size↓" {
		t.Errorf("got %q", got)
	}
	if got := sortLabel(sortName, false); got != "sort:name↑" {
		t.Errorf("got %q", got)
	}
}

func TestDeleteConfirmText(t *testing.T) {
	t.Run("single target names the key and its cost", func(t *testing.T) {
		got := deleteConfirmText([]deleteTarget{{key: "photos/a.jpg", objects: 1, bytes: 2048}})
		if !strings.Contains(got, "Delete photos/a.jpg?") {
			t.Errorf("got %q", got)
		}
		if !strings.Contains(got, "1 object(s), 2.0 KiB") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("multiple targets are counted and totalled", func(t *testing.T) {
		targets := []deleteTarget{
			{key: "a/", isFolder: true, objects: 10, bytes: 1024},
			{key: "b.txt", objects: 1, bytes: 1024},
		}
		got := deleteConfirmText(targets)
		if !strings.Contains(got, "Delete 2 item(s)?") {
			t.Errorf("got %q", got)
		}
		if !strings.Contains(got, "11 object(s), 2.0 KiB") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("long lists are truncated", func(t *testing.T) {
		var targets []deleteTarget
		for i := 0; i < 10; i++ {
			targets = append(targets, deleteTarget{key: fmt.Sprintf("f%d", i), objects: 1})
		}
		if got := deleteConfirmText(targets); !strings.Contains(got, "...and 4 more") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a failed scan is disclosed, not hidden", func(t *testing.T) {
		targets := []deleteTarget{
			{key: "ok/", isFolder: true, objects: 3, bytes: 10},
			{key: "bad/", isFolder: true, scanErr: errors.New("access denied")},
		}
		got := deleteConfirmText(targets)
		if !strings.Contains(got, "could not be sized") {
			t.Errorf("expected an incomplete-totals warning, got %q", got)
		}
	})
}

func TestUniqueProfileName(t *testing.T) {
	existing := []*cfg.Config{{Name: "aws-default"}, {Name: "aws-default-2"}, nil}
	if got := uniqueProfileName("aws-prod", existing); got != "aws-prod" {
		t.Errorf("got %q, want the base name when free", got)
	}
	if got := uniqueProfileName("aws-default", existing); got != "aws-default-3" {
		t.Errorf("got %q, want aws-default-3 (2 is taken)", got)
	}
	if got := uniqueProfileName("x", nil); got != "x" {
		t.Errorf("got %q", got)
	}
}

func TestAwsProfileConfig(t *testing.T) {
	p := cfg.AWSProfile{Name: "prod", AccessKey: "AK", SecretKey: "SK", SessionToken: "TOK", Region: "eu-west-1"}
	got := awsProfileConfig(p, []*cfg.Config{{Name: "aws-prod"}})

	if got.Name != "aws-prod-2" {
		t.Errorf("name = %q, want the de-duplicated aws-prod-2", got.Name)
	}
	if got.BaseUrl != "https://s3.eu-west-1.amazonaws.com" {
		t.Errorf("url = %q", got.BaseUrl)
	}
	if got.Region == nil || *got.Region != "eu-west-1" {
		t.Errorf("region = %v", got.Region)
	}
	if got.SessionToken != "TOK" {
		t.Errorf("session token was dropped: %q", got.SessionToken)
	}

	noRegion := awsProfileConfig(cfg.AWSProfile{Name: "d", AccessKey: "A", SecretKey: "S"}, nil)
	if noRegion.Region != nil {
		t.Errorf("region = %v, want nil when unset", noRegion.Region)
	}
	if noRegion.BaseUrl != "https://s3.amazonaws.com" {
		t.Errorf("url = %q, want the global endpoint", noRegion.BaseUrl)
	}
}

func TestAwsProfileRow(t *testing.T) {
	primary, secondary := awsProfileRow(cfg.AWSProfile{Name: "prod", AccessKey: "A", SecretKey: "S", Region: "us-east-1"})
	if secondary != "prod" {
		t.Errorf("secondary = %q", secondary)
	}
	if !strings.Contains(primary, "us-east-1") || !strings.Contains(primary, "long-lived key") {
		t.Errorf("primary = %q", primary)
	}

	primary, _ = awsProfileRow(cfg.AWSProfile{Name: "tmp", AccessKey: "A", SecretKey: "S", SessionToken: "T"})
	if !strings.Contains(primary, "temporary credentials") || !strings.Contains(primary, "no region") {
		t.Errorf("primary = %q", primary)
	}

	primary, _ = awsProfileRow(cfg.AWSProfile{Name: "sso", Err: "SSO profile: ..."})
	if !strings.Contains(primary, "SSO profile") {
		t.Errorf("unusable profiles must show why: %q", primary)
	}
}

func TestValidateEntryName(t *testing.T) {
	for _, bad := range []string{"a/b", ".", ".."} {
		if err := validateEntryName(bad); err == nil {
			t.Errorf("validateEntryName(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"notes.txt", "..hidden", "a.b", "...three"} {
		if err := validateEntryName(good); err != nil {
			t.Errorf("validateEntryName(%q) = %v, want nil", good, err)
		}
	}
}

// newTestController builds a Controller with its panes wired, which is the
// minimum any per-location helper needs. Nothing here touches the network or
// the terminal.
func newTestController() *Controller {
	return &Controller{panes: [2]*session.Pane{session.New(), session.New()}}
}

// The confirmation must name the object it deletes. A modal renders tview
// tags, so an unescaped "backup[old].tar" read as "backup.tar".
func TestDeleteConfirmTextEscapesKeys(t *testing.T) {
	for _, targets := range [][]deleteTarget{
		{{key: "backup[old].tar", objects: 1}},
		{{key: "backup[old].tar", objects: 1}, {key: "b.txt", objects: 1}},
	} {
		text := deleteConfirmText(targets)
		if !strings.Contains(text, tview.Escape("backup[old].tar")) {
			t.Errorf("confirm text %q does not carry the escaped key", text)
		}
	}
}

// The properties modal renders colour tags, and both the name and the link it
// shows are built from an S3 key — which may contain one. Unescaped, "a[red]b"
// was drawn as "ab" and recoloured everything after it, so the panel named a
// different object than the one it was opened on.
func TestFilePropertiesTextEscapesKeys(t *testing.T) {
	when := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	text := filePropertiesText("a[red]b.txt", "10 B", &when, "etag", "https://h/bucket/a[red]b.txt")

	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetText(text)
	shown := tv.GetText(true)
	if !strings.Contains(shown, "a[red]b.txt") {
		t.Errorf("the key was eaten as markup:\n%s", shown)
	}
	if !strings.Contains(shown, "https://h/bucket/a[red]b.txt") {
		t.Errorf("the link was eaten as markup:\n%s", shown)
	}
}
