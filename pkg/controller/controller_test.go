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

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
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

func TestThroughput(t *testing.T) {
	tests := []struct {
		name     string
		done     int64
		total    int64
		elapsed  time.Duration
		wantRate float64
		wantETA  time.Duration
	}{
		{"zero elapsed -> unknown", 10, 100, 0, 0, -1},
		{"zero done -> unknown", 0, 100, time.Second, 0, -1},
		{"halfway", 50, 100, time.Second, 50, time.Second},
		{"unknown total -> rate only", 100, 0, time.Second, 100, -1},
		{"already complete -> no eta", 100, 100, time.Second, 100, -1},
		{"slow eta", 25, 100, 5 * time.Second, 5, 15 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rate, eta := throughput(tt.done, tt.total, tt.elapsed)
			if rate != tt.wantRate {
				t.Errorf("rate = %v, want %v", rate, tt.wantRate)
			}
			if eta != tt.wantETA {
				t.Errorf("eta = %v, want %v", eta, tt.wantETA)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		-1 * time.Second:                      "--",
		0:                                     "0:00",
		5 * time.Second:                       "0:05",
		65 * time.Second:                      "1:05",
		time.Hour:                             "1:00:00",
		time.Hour + time.Minute + time.Second: "1:01:01",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestByteRateETA(t *testing.T) {
	// no throughput yet -> both unknown
	if got := byteRateETA(0, 100, time.Second); got != "-- · ETA --" {
		t.Errorf("byteRateETA(idle) = %q, want %q", got, "-- · ETA --")
	}
	// 1 MiB of 2 MiB in 1s -> 1.0 MiB/s, ETA 0:01
	got := byteRateETA(1<<20, 2<<20, time.Second)
	if !strings.Contains(got, "MiB/s") || !strings.Contains(got, "ETA 0:01") {
		t.Errorf("byteRateETA = %q, want a MiB/s rate and ETA 0:01", got)
	}
}

func TestObjRateETA(t *testing.T) {
	if got := objRateETA(0, 10, time.Second); got != "-- · ETA --" {
		t.Errorf("objRateETA(idle) = %q, want %q", got, "-- · ETA --")
	}
	// 5 of 10 objects in 1s -> 5 obj/s, ETA 0:01
	got := objRateETA(5, 10, time.Second)
	if !strings.Contains(got, "5 obj/s") || !strings.Contains(got, "ETA 0:01") {
		t.Errorf("objRateETA = %q, want '5 obj/s' and ETA 0:01", got)
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
	c := &Controller{view: view.NewView()}

	bA := &model.Object{Key: strptr("A"), Ot: model.Bucket}
	bB := &model.Object{Key: strptr("B"), Ot: model.Bucket}

	// pane 0 is live; pane 1 is the stored snapshot. Every field is given a
	// distinct value so a field left out of snapshot/restore is caught.
	c.active = 0
	c.currentBucket, c.currentPath, c.filter = bA, "p0/", "f0"
	c.bucketPos, c.restoreNext = 5, "r0"
	c.buckets = []*model.Object{bA}
	c.objs = map[string]*model.Object{"a0": nil}
	c.selectedByScope = map[string]map[string]bool{"s0": {}}
	c.hist = histStack{back: []location{{path: "h0"}}}

	c.panes[1] = paneState{
		currentBucket:   bB,
		currentPath:     "p1/",
		filter:          "f1",
		bucketPos:       9,
		restoreNext:     "r1",
		buckets:         []*model.Object{bB},
		objs:            map[string]*model.Object{"a1": nil},
		selectedByScope: map[string]map[string]bool{"s1": {}},
		hist:            histStack{back: []location{{path: "h1"}}},
	}

	c.swapPane()

	// Live now reflects pane 1 in full.
	if c.active != 1 {
		t.Fatalf("active = %d, want 1", c.active)
	}
	if c.currentBucket != bB || c.currentPath != "p1/" || c.filter != "f1" ||
		c.bucketPos != 9 || c.restoreNext != "r1" {
		t.Errorf("scalars not loaded from pane 1: bucket=%v path=%q filter=%q pos=%d restore=%q",
			c.currentBucket, c.currentPath, c.filter, c.bucketPos, c.restoreNext)
	}
	if _, ok := c.objs["a1"]; !ok {
		t.Errorf("objs not swapped to pane 1: %v", c.objs)
	}
	if _, ok := c.selectedByScope["s1"]; !ok {
		t.Errorf("selectedByScope not swapped to pane 1")
	}
	if len(c.buckets) != 1 || c.buckets[0] != bB {
		t.Errorf("buckets not swapped to pane 1")
	}
	if len(c.hist.back) != 1 || c.hist.back[0].path != "h1" {
		t.Errorf("hist not swapped to pane 1: %+v", c.hist)
	}
	if c.view.List != c.view.PaneList(1) || c.view.Filter != c.view.PaneFilter(1) {
		t.Errorf("view widgets not repointed to pane 1")
	}

	// pane 0 was snapshotted intact.
	if c.panes[0].currentBucket != bA || c.panes[0].currentPath != "p0/" ||
		c.panes[0].filter != "f0" || c.panes[0].bucketPos != 5 || c.panes[0].restoreNext != "r0" {
		t.Errorf("pane 0 snapshot scalars wrong: %+v", c.panes[0])
	}
	if _, ok := c.panes[0].objs["a0"]; !ok {
		t.Errorf("pane 0 objs not snapshotted")
	}
	if len(c.panes[0].hist.back) != 1 || c.panes[0].hist.back[0].path != "h0" {
		t.Errorf("pane 0 hist not snapshotted")
	}

	// Round-trip restores pane 0 fully.
	c.swapPane()
	if c.active != 0 || c.currentBucket != bA || c.currentPath != "p0/" || c.filter != "f0" ||
		c.bucketPos != 5 || c.restoreNext != "r0" {
		t.Errorf("round-trip did not restore pane 0")
	}
	if _, ok := c.objs["a0"]; !ok {
		t.Errorf("round-trip objs wrong")
	}
	if c.view.List != c.view.PaneList(0) {
		t.Errorf("view.List not repointed back to pane 0")
	}
}

func TestResetPanes(t *testing.T) {
	c := &Controller{view: view.NewView()}
	// Dirty dual-pane state on pane 1.
	c.dual = true
	c.active = 1
	c.pane1Init = true
	c.filter = "stale"
	c.selectedByScope = map[string]map[string]bool{"x": {}}
	c.hist = histStack{back: []location{{path: "h"}}}
	c.view.List = c.view.PaneList(1)
	c.view.Filter = c.view.PaneFilter(1)

	c.resetPanes()

	if c.dual || c.active != 0 || c.pane1Init {
		t.Errorf("flags not reset: dual=%v active=%d pane1Init=%v", c.dual, c.active, c.pane1Init)
	}
	if c.view.List != c.view.PaneList(0) || c.view.Filter != c.view.PaneFilter(0) {
		t.Errorf("view widgets not repointed to pane 0")
	}
	if c.filter != "" {
		t.Errorf("filter not cleared: %q", c.filter)
	}
	if len(c.selectedByScope) != 0 {
		t.Errorf("selection not cleared: %v", c.selectedByScope)
	}
	if len(c.hist.back) != 0 || len(c.hist.fwd) != 0 {
		t.Errorf("history not cleared: %+v", c.hist)
	}
	if c.panes[1].selectedByScope == nil {
		t.Errorf("pane 1 selection map must be non-nil after reset (else selScopeLocked panics)")
	}
}

func TestHistStack(t *testing.T) {
	loc := func(p string) location { return location{path: p} }
	var h histStack

	if _, ok := h.goBack(loc("cur")); ok {
		t.Errorf("goBack on empty should fail")
	}
	if _, ok := h.goForward(loc("cur")); ok {
		t.Errorf("goForward on empty should fail")
	}

	// Visited A then B, now sitting at C.
	h.record(loc("A"))
	h.record(loc("B"))

	prev, ok := h.goBack(loc("C"))
	if !ok || prev.path != "B" {
		t.Fatalf("goBack = %+v ok=%v, want B", prev, ok)
	}
	prev, ok = h.goBack(loc("B"))
	if !ok || prev.path != "A" {
		t.Fatalf("goBack = %+v ok=%v, want A", prev, ok)
	}
	next, ok := h.goForward(loc("A"))
	if !ok || next.path != "B" {
		t.Fatalf("goForward = %+v ok=%v, want B", next, ok)
	}

	// Recording a new branch clears the forward stack.
	h.record(loc("B"))
	if _, ok := h.goForward(loc("X")); ok {
		t.Errorf("record should clear the forward stack")
	}
}

func TestJobStatusString(t *testing.T) {
	cases := map[jobStatus]string{
		jobRunning:  "running",
		jobQueued:   "queued",
		jobDone:     "done",
		jobFailed:   "failed",
		jobCanceled: "canceled",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("jobStatus(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestTransferRow(t *testing.T) {
	jv := jobView{
		kind: "download", desc: "5 obj → /dl", status: jobRunning,
		total: 200, done: 100, count: 5, doneCount: 2,
	}
	primary, secondary := transferRow(jv, time.Second)
	if !strings.Contains(primary, "running") || !strings.Contains(primary, "download") {
		t.Errorf("primary = %q", primary)
	}
	if !strings.Contains(secondary, "2/5 obj") || !strings.Contains(secondary, "50%") {
		t.Errorf("secondary = %q, want 2/5 obj and 50%%", secondary)
	}
	// A finished job shows "done" instead of a live rate/ETA.
	jv.status = jobDone
	if _, sec := transferRow(jv, time.Second); !strings.Contains(sec, "done") {
		t.Errorf("done-row secondary = %q, want 'done'", sec)
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
	if back[0] != fwd[0] || back[1] != fwd[1] {
		t.Errorf("double invert not identity: %+v", back)
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
