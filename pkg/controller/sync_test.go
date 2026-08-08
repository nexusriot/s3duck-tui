package controller

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

var syncBase = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

func entry(rel string, size int64, offset time.Duration) model.SyncEntry {
	return model.SyncEntry{Rel: rel, Size: size, Mod: syncBase.Add(offset)}
}

func opKeys(ops []syncOp) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.Kind.String()+":"+o.Rel)
	}
	return out
}

func TestPlanSync(t *testing.T) {
	t.Run("missing at destination is a create", func(t *testing.T) {
		ops := planSync(
			[]model.SyncEntry{entry("a.txt", 10, 0), entry("dir/b.txt", 20, 0)},
			nil, false,
		)
		want := []string{"create:a.txt", "create:dir/b.txt"}
		if got := opKeys(ops); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
		if st := summarizeSync(ops); st.Bytes != 30 || st.Creates != 2 {
			t.Errorf("got %+v, want 2 creates / 30 bytes", st)
		}
	})

	t.Run("identical size and mtime is skipped", func(t *testing.T) {
		src := []model.SyncEntry{entry("a.txt", 10, 0)}
		dst := []model.SyncEntry{entry("a.txt", 10, 0)}
		if ops := planSync(src, dst, false); len(ops) != 0 {
			t.Errorf("got %v, want no ops", opKeys(ops))
		}
	})

	t.Run("differing size is an update in either direction", func(t *testing.T) {
		// Larger at the source...
		ops := planSync(
			[]model.SyncEntry{entry("a.txt", 20, 0)},
			[]model.SyncEntry{entry("a.txt", 10, 0)}, false,
		)
		if got := opKeys(ops); !reflect.DeepEqual(got, []string{"update:a.txt"}) {
			t.Fatalf("got %v, want update", got)
		}
		if ops[0].Bytes != 20 {
			t.Errorf("bytes = %d, want the source size 20", ops[0].Bytes)
		}
		// ...and smaller. A shrinking file must still be re-transferred.
		ops = planSync(
			[]model.SyncEntry{entry("a.txt", 5, 0)},
			[]model.SyncEntry{entry("a.txt", 10, 0)}, false,
		)
		if got := opKeys(ops); !reflect.DeepEqual(got, []string{"update:a.txt"}) {
			t.Errorf("got %v, want update", got)
		}
	})

	t.Run("same size but newer source is an update", func(t *testing.T) {
		ops := planSync(
			[]model.SyncEntry{entry("a.txt", 10, time.Hour)},
			[]model.SyncEntry{entry("a.txt", 10, 0)}, false,
		)
		if got := opKeys(ops); !reflect.DeepEqual(got, []string{"update:a.txt"}) {
			t.Errorf("got %v, want update", got)
		}
		if ops[0].Reason != "source is newer" {
			t.Errorf("reason = %q", ops[0].Reason)
		}
	})

	t.Run("older source is not re-transferred", func(t *testing.T) {
		ops := planSync(
			[]model.SyncEntry{entry("a.txt", 10, -time.Hour)},
			[]model.SyncEntry{entry("a.txt", 10, 0)}, false,
		)
		if len(ops) != 0 {
			t.Errorf("got %v, want no ops", opKeys(ops))
		}
	})

	t.Run("sub-tolerance skew does not trigger a transfer", func(t *testing.T) {
		ops := planSync(
			[]model.SyncEntry{entry("a.txt", 10, syncModTolerance-time.Millisecond)},
			[]model.SyncEntry{entry("a.txt", 10, 0)}, false,
		)
		if len(ops) != 0 {
			t.Errorf("got %v, want no ops within tolerance", opKeys(ops))
		}
	})

	t.Run("a zero mtime on either side falls back to size only", func(t *testing.T) {
		src := []model.SyncEntry{{Rel: "a.txt", Size: 10}}
		dst := []model.SyncEntry{entry("a.txt", 10, -time.Hour)}
		if ops := planSync(src, dst, false); len(ops) != 0 {
			t.Errorf("got %v, want no ops when a timestamp is unknown", opKeys(ops))
		}
	})

	t.Run("extraneous destination files are only deleted when asked", func(t *testing.T) {
		src := []model.SyncEntry{entry("a.txt", 10, 0)}
		dst := []model.SyncEntry{entry("a.txt", 10, 0), entry("stale.txt", 99, 0)}

		if ops := planSync(src, dst, false); len(ops) != 0 {
			t.Errorf("got %v, want no ops without the delete flag", opKeys(ops))
		}

		ops := planSync(src, dst, true)
		if got := opKeys(ops); !reflect.DeepEqual(got, []string{"delete:stale.txt"}) {
			t.Fatalf("got %v, want delete:stale.txt", got)
		}
		if ops[0].Bytes != 0 {
			t.Errorf("a delete must not count toward transfer bytes, got %d", ops[0].Bytes)
		}
	})

	t.Run("ordering is creates then updates then deletes, each by path", func(t *testing.T) {
		src := []model.SyncEntry{
			entry("z-new.txt", 1, 0),
			entry("a-new.txt", 1, 0),
			entry("z-changed.txt", 2, 0),
			entry("a-changed.txt", 2, 0),
		}
		dst := []model.SyncEntry{
			entry("z-changed.txt", 9, 0),
			entry("a-changed.txt", 9, 0),
			entry("z-gone.txt", 1, 0),
			entry("a-gone.txt", 1, 0),
		}
		want := []string{
			"create:a-new.txt", "create:z-new.txt",
			"update:a-changed.txt", "update:z-changed.txt",
			"delete:a-gone.txt", "delete:z-gone.txt",
		}
		if got := opKeys(planSync(src, dst, true)); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty source with delete clears the destination", func(t *testing.T) {
		dst := []model.SyncEntry{entry("a.txt", 1, 0), entry("b.txt", 1, 0)}
		st := summarizeSync(planSync(nil, dst, true))
		if st.Deletes != 2 || st.Creates != 0 || st.Updates != 0 {
			t.Errorf("got %+v, want 2 deletes", st)
		}
	})
}

func TestSyncPlanText(t *testing.T) {
	t.Run("empty plan says so and offers nothing", func(t *testing.T) {
		got := syncPlanText(syncUpload, "/tmp/x", "bucket/pre/", nil, 10)
		if !strings.Contains(got, "Already in sync") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("totals and direction are stated", func(t *testing.T) {
		ops := []syncOp{
			{Kind: syncCreate, Rel: "a.txt", Bytes: 1024},
			{Kind: syncUpdate, Rel: "b.txt", Bytes: 1024, Reason: "source is newer"},
			{Kind: syncDelete, Rel: "c.txt"},
		}
		got := syncPlanText(syncDownload, "/tmp/x", "bucket/pre/", ops, 10)
		for _, want := range []string{"remote → local", "1 create, 1 update, 1 delete", "2.0 KiB", "source is newer", "/tmp/x", "bucket/pre/"} {
			if !strings.Contains(got, want) {
				t.Errorf("plan missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("long plans are truncated with a count", func(t *testing.T) {
		var ops []syncOp
		for i := 0; i < 25; i++ {
			ops = append(ops, syncOp{Kind: syncCreate, Rel: string(rune('a'+i)) + ".txt", Bytes: 1})
		}
		got := syncPlanText(syncUpload, "/tmp/x", "bucket/", ops, 10)
		if !strings.Contains(got, "...and 15 more") {
			t.Errorf("expected truncation note:\n%s", got)
		}
	})
}

func TestSummarizeSync(t *testing.T) {
	ops := []syncOp{
		{Kind: syncCreate, Bytes: 100},
		{Kind: syncCreate, Bytes: 200},
		{Kind: syncUpdate, Bytes: 50},
		{Kind: syncDelete},
	}
	want := syncStats{Creates: 2, Updates: 1, Deletes: 1, Bytes: 350}
	if got := summarizeSync(ops); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := summarizeSync(nil); got != (syncStats{}) {
		t.Errorf("got %+v, want zero", got)
	}
}

func TestSyncDirectionString(t *testing.T) {
	if syncUpload.String() != "local → remote" || syncDownload.String() != "remote → local" {
		t.Errorf("unexpected direction labels: %q / %q", syncUpload, syncDownload)
	}
}

func TestSyncWorkerCount(t *testing.T) {
	cases := map[int]int{0: 1, -3: 1, 1: 1, 2: 2, 3: 3, 4: 4, 5: 4, 500: 4}
	for in, want := range cases {
		if got := syncWorkerCount(in); got != want {
			t.Errorf("syncWorkerCount(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestSplitSyncPhases(t *testing.T) {
	ops := []syncOp{
		{Kind: syncCreate, Rel: "a"},
		{Kind: syncDelete, Rel: "gone1"},
		{Kind: syncUpdate, Rel: "b"},
		{Kind: syncDelete, Rel: "gone2"},
	}
	writes, deletes := splitSyncPhases(ops)

	t.Run("writes are creates and updates, deletes are separate", func(t *testing.T) {
		if len(writes) != 2 || len(deletes) != 2 {
			t.Fatalf("writes=%d deletes=%d, want 2/2", len(writes), len(deletes))
		}
		for _, w := range writes {
			if w.op.Kind == syncDelete {
				t.Errorf("a delete leaked into the write phase: %+v", w.op)
			}
		}
		for _, d := range deletes {
			if d.op.Kind != syncDelete {
				t.Errorf("a non-delete leaked into the delete phase: %+v", d.op)
			}
		}
	})

	t.Run("original plan indices are preserved for progress reporting", func(t *testing.T) {
		if writes[0].index != 0 || writes[1].index != 2 {
			t.Errorf("write indices = %d,%d; want 0,2", writes[0].index, writes[1].index)
		}
		if deletes[0].index != 1 || deletes[1].index != 3 {
			t.Errorf("delete indices = %d,%d; want 1,3", deletes[0].index, deletes[1].index)
		}
	})

	t.Run("every op lands in exactly one phase", func(t *testing.T) {
		seen := map[int]bool{}
		for _, x := range append(append([]indexedOp{}, writes...), deletes...) {
			if seen[x.index] {
				t.Errorf("op %d appears twice", x.index)
			}
			seen[x.index] = true
		}
		if len(seen) != len(ops) {
			t.Errorf("covered %d of %d ops", len(seen), len(ops))
		}
	})

	t.Run("empty plan yields empty phases", func(t *testing.T) {
		w, d := splitSyncPhases(nil)
		if len(w) != 0 || len(d) != 0 {
			t.Errorf("got %d/%d, want 0/0", len(w), len(d))
		}
	})
}

func obj(name string) *model.Object {
	n := name
	return &model.Object{Key: &n, Ot: model.Bucket}
}

func TestSyncDirectionLabelsMatchConstants(t *testing.T) {
	// The dropdown index IS the direction, so the two must stay in lockstep.
	labels := syncDirectionLabels()
	if len(labels) != 3 {
		t.Fatalf("got %d labels, want 3", len(labels))
	}
	for dir, want := range map[syncDirection]string{
		syncUpload:   "upload",
		syncDownload: "download",
		syncRemote:   "remote → remote",
	} {
		if !strings.Contains(labels[int(dir)], want) {
			t.Errorf("labels[%d] = %q, want it to mention %q", int(dir), labels[int(dir)], want)
		}
	}
	if syncRemote.String() != "remote → remote" {
		t.Errorf("syncRemote.String() = %q", syncRemote)
	}
}

func TestSyncSpecLabels(t *testing.T) {
	t.Run("upload reads local as source", func(t *testing.T) {
		s := syncSpec{dir: syncUpload, localDir: "/tmp/x", dstBucket: obj("b"), dstPrefix: "pre/"}
		if got := s.srcLabel(); got != "/tmp/x" {
			t.Errorf("src = %q", got)
		}
		if got := s.dstLabel(); got != "b/pre/" {
			t.Errorf("dst = %q", got)
		}
	})

	t.Run("download reverses them", func(t *testing.T) {
		s := syncSpec{dir: syncDownload, localDir: "/tmp/x", srcBucket: obj("b"), srcPrefix: "pre/"}
		if got := s.srcLabel(); got != "b/pre/" {
			t.Errorf("src = %q", got)
		}
		if got := s.dstLabel(); got != "/tmp/x" {
			t.Errorf("dst = %q", got)
		}
	})

	t.Run("remote uses both buckets and never the local dir", func(t *testing.T) {
		s := syncSpec{dir: syncRemote, localDir: "/ignored",
			srcBucket: obj("src"), srcPrefix: "a/", dstBucket: obj("dst"), dstPrefix: "b/"}
		if got := s.srcLabel(); got != "src/a/" {
			t.Errorf("src = %q", got)
		}
		if got := s.dstLabel(); got != "dst/b/" {
			t.Errorf("dst = %q", got)
		}
	})

	t.Run("a nil bucket degrades instead of panicking", func(t *testing.T) {
		s := syncSpec{dir: syncRemote}
		if got := s.srcLabel(); got != "(no bucket)" {
			t.Errorf("src = %q", got)
		}
	})
}

func TestComparePlanText(t *testing.T) {
	t.Run("identical sides say so", func(t *testing.T) {
		got := comparePlanText("a/", "b/", nil, 10)
		if !strings.Contains(got, "Identical") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("kinds are re-worded for a comparison", func(t *testing.T) {
		ops := []syncOp{
			{Kind: syncCreate, Rel: "onlyleft.txt"},
			{Kind: syncUpdate, Rel: "both.txt", Reason: "size 1 B → 2 B"},
			{Kind: syncDelete, Rel: "onlyright.txt"},
		}
		got := comparePlanText("bucketA/x/", "bucketB/y/", ops, 10)
		for _, want := range []string{
			"bucketA/x/", "bucketB/y/",
			"1 only on the left, 1 differing, 1 only on the right",
			"left-only", "differs", "right-only",
			"size 1 B → 2 B",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("compare output missing %q:\n%s", want, got)
			}
		}
		// Sync verbs must not leak into a read-only comparison.
		for _, unwanted := range []string{"create ", "delete "} {
			if strings.Contains(got, unwanted) {
				t.Errorf("comparison should not use the verb %q:\n%s", unwanted, got)
			}
		}
	})

	t.Run("states that only names and sizes were compared", func(t *testing.T) {
		got := comparePlanText("a", "b", []syncOp{{Kind: syncCreate, Rel: "x"}}, 10)
		if !strings.Contains(got, "contents are not read") {
			t.Errorf("the comparison must disclose its limits:\n%s", got)
		}
	})

	t.Run("long comparisons truncate", func(t *testing.T) {
		var ops []syncOp
		for i := 0; i < 30; i++ {
			ops = append(ops, syncOp{Kind: syncCreate, Rel: fmt.Sprintf("f%02d", i)})
		}
		if got := comparePlanText("a", "b", ops, 10); !strings.Contains(got, "...and 20 more") {
			t.Errorf("expected truncation:\n%s", got)
		}
	})
}

func TestCompareLabel(t *testing.T) {
	cases := map[syncOpKind]string{
		syncCreate: "left-only",
		syncUpdate: "differs",
		syncDelete: "right-only",
	}
	for k, want := range cases {
		if got := compareLabel(k); got != want {
			t.Errorf("compareLabel(%v) = %q, want %q", k, got, want)
		}
	}
}

func TestRemoteLabel(t *testing.T) {
	if got := remoteLabel(obj("b"), "p/"); got != "b/p/" {
		t.Errorf("got %q", got)
	}
	if got := remoteLabel(obj("b"), ""); got != "b/" {
		t.Errorf("bucket root should render as b/, got %q", got)
	}
	if got := remoteLabel(nil, "p/"); got != "(no bucket)" {
		t.Errorf("got %q", got)
	}
}

func TestSyncPlanTextUsesFromTo(t *testing.T) {
	// The plan header must name both sides generically — with a remote→remote
	// run there is no "local" side to label.
	got := syncPlanText(syncRemote, "src/a/", "dst/b/", []syncOp{{Kind: syncCreate, Rel: "x", Bytes: 1}}, 10)
	for _, want := range []string{"remote → remote", "from: src/a/", "to:   dst/b/"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan missing %q:\n%s", want, got)
		}
	}
}

func TestPrefixesOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"data/", "mirror/", false},
		{"data/", "data/", true},
		{"", "mirror/", true},
		{"mirror/", "", true},
		{"data/", "data/sub/", true},
		{"data/sub/", "data/", true},
		// A shared name prefix that is not a shared path prefix still overlaps
		// under HasPrefix ("data" vs "data2/") — but normalized prefixes always
		// end in "/" (or are empty), so "data2/" does not start with "data/".
		{"data/", "data2/", false},
	}
	for _, c := range cases {
		if got := prefixesOverlap(c.a, c.b); got != c.want {
			t.Errorf("prefixesOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
