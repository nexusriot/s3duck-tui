package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func dupObj(key, etag string, size int64, offset time.Duration) s3t.Object {
	when := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC).Add(offset)
	return s3t.Object{
		Key:          aws.String(key),
		ETag:         aws.String(etag),
		Size:         size,
		LastModified: &when,
	}
}

func TestFindDuplicates(t *testing.T) {
	t.Run("groups by etag and size, drops singletons", func(t *testing.T) {
		groups := findDuplicates([]s3t.Object{
			dupObj("a/one.txt", `"abc"`, 10, 0),
			dupObj("b/two.txt", `"abc"`, 10, time.Hour),
			dupObj("unique.txt", `"zzz"`, 10, 0),
		})
		if len(groups) != 1 {
			t.Fatalf("got %d groups, want 1", len(groups))
		}
		g := groups[0]
		if g.ETag != "abc" || g.Size != 10 || len(g.Members) != 2 {
			t.Errorf("group = %+v", g)
		}
		if g.wasted() != 10 {
			t.Errorf("wasted = %d, want 10 (one redundant copy)", g.wasted())
		}
	})

	t.Run("same etag but different size does not group", func(t *testing.T) {
		// A multipart ETag can collide across different objects; size is the
		// second factor that keeps such collisions out.
		groups := findDuplicates([]s3t.Object{
			dupObj("a", `"abc-2"`, 10, 0),
			dupObj("b", `"abc-2"`, 20, 0),
		})
		if len(groups) != 0 {
			t.Errorf("got %d groups, want 0", len(groups))
		}
	})

	t.Run("folder markers and missing etags are ignored", func(t *testing.T) {
		noTag := dupObj("x", "", 5, 0)
		noTag.ETag = nil
		groups := findDuplicates([]s3t.Object{
			dupObj("dir/", `"abc"`, 0, 0),
			dupObj("dir2/", `"abc"`, 0, 0),
			noTag,
			dupObj("y", `""`, 5, 0),
		})
		if len(groups) != 0 {
			t.Errorf("got %d groups, want 0: %+v", len(groups), groups)
		}
	})

	t.Run("etag quotes are normalized", func(t *testing.T) {
		groups := findDuplicates([]s3t.Object{
			dupObj("a", `"abc"`, 7, 0),
			dupObj("b", `abc`, 7, 0), // some backends omit the quotes
		})
		if len(groups) != 1 || len(groups[0].Members) != 2 {
			t.Fatalf("quote forms should group together: %+v", groups)
		}
	})

	t.Run("groups sort by wasted bytes descending", func(t *testing.T) {
		groups := findDuplicates([]s3t.Object{
			dupObj("small1", `"s"`, 10, 0),
			dupObj("small2", `"s"`, 10, 0),
			dupObj("big1", `"b"`, 1000, 0),
			dupObj("big2", `"b"`, 1000, 0),
			dupObj("big3", `"b"`, 1000, 0), // wasted 2000
		})
		if len(groups) != 2 {
			t.Fatalf("got %d groups, want 2", len(groups))
		}
		if groups[0].ETag != "b" || groups[0].wasted() != 2000 {
			t.Errorf("first group should waste the most: %+v", groups[0])
		}
	})

	t.Run("members sort oldest first with key tiebreak", func(t *testing.T) {
		groups := findDuplicates([]s3t.Object{
			dupObj("newer.txt", `"e"`, 5, 2*time.Hour),
			dupObj("oldest.txt", `"e"`, 5, 0),
			dupObj("b-same-time.txt", `"e"`, 5, 2*time.Hour),
		})
		if len(groups) != 1 {
			t.Fatalf("got %d groups", len(groups))
		}
		got := []string{}
		for _, m := range groups[0].Members {
			got = append(got, m.Key)
		}
		want := "oldest.txt,b-same-time.txt,newer.txt"
		if strings.Join(got, ",") != want {
			t.Errorf("member order = %v, want %s", got, want)
		}
	})

	t.Run("nil and empty input", func(t *testing.T) {
		if got := findDuplicates(nil); len(got) != 0 {
			t.Errorf("nil input: got %v", got)
		}
	})
}

func TestDupSummaryAndRows(t *testing.T) {
	groups := findDuplicates([]s3t.Object{
		dupObj("a", `"x"`, 1024, 0),
		dupObj("b", `"x"`, 1024, time.Hour),
		dupObj("c", `"x"`, 1024, 2*time.Hour),
	})

	t.Run("summary counts groups, copies and reclaimable bytes", func(t *testing.T) {
		got := dupSummary(groups)
		for _, want := range []string{"1 group(s)", "2 redundant copies", "2.0 KiB reclaimable"} {
			if !strings.Contains(got, want) {
				t.Errorf("summary missing %q: %q", want, got)
			}
		}
	})

	t.Run("a single redundant copy reads grammatically", func(t *testing.T) {
		one := findDuplicates([]s3t.Object{
			dupObj("a", `"y"`, 10, 0),
			dupObj("b", `"y"`, 10, 0),
		})
		if got := dupSummary(one); !strings.Contains(got, "1 redundant copy,") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("group row shows count, size, waste and the oldest key", func(t *testing.T) {
		primary, secondary := dupGroupRow(groups[0])
		for _, want := range []string{"3 ×", "1.0 KiB", "wastes 2.0 KiB", "a"} {
			if !strings.Contains(primary, want) {
				t.Errorf("row missing %q: %q", want, primary)
			}
		}
		if secondary != "x" {
			t.Errorf("secondary = %q, want the etag", secondary)
		}
	})

	t.Run("member rows mark only the oldest as original", func(t *testing.T) {
		oldest, _ := dupMemberRow(groups[0].Members[0], true)
		later, _ := dupMemberRow(groups[0].Members[1], false)
		if !strings.Contains(oldest, "*") {
			t.Errorf("oldest not marked: %q", oldest)
		}
		if strings.Contains(stripTags(later), "*") {
			t.Errorf("a later copy must not be marked: %q", later)
		}
		if !strings.Contains(later, "2026-08-08") {
			t.Errorf("member row missing its date: %q", later)
		}
	})

	t.Run("a member without a date degrades", func(t *testing.T) {
		primary, _ := dupMemberRow(dupMember{Key: "k"}, false)
		if !strings.Contains(primary, "unknown date") {
			t.Errorf("got %q", primary)
		}
	})
}

func TestDropDupMember(t *testing.T) {
	mk := func(etag string, keys ...string) dupGroup {
		g := dupGroup{ETag: etag, Size: 10}
		for _, k := range keys {
			g.Members = append(g.Members, dupMember{Key: k, Size: 10})
		}
		return g
	}

	t.Run("removing one of three keeps the group", func(t *testing.T) {
		groups := []dupGroup{mk("e1", "a", "b", "c"), mk("e2", "x", "y")}
		got, same := dropDupMember(groups, 0, "b")
		if !same {
			t.Fatal("group with two remaining copies must survive")
		}
		if len(got) != 2 || len(got[0].Members) != 2 {
			t.Fatalf("groups = %+v", got)
		}
		if got[0].Members[0].Key != "a" || got[0].Members[1].Key != "c" {
			t.Errorf("members = %+v", got[0].Members)
		}
	})

	t.Run("dissolving a middle group must NOT present the next group", func(t *testing.T) {
		// This pins the exact bug: after the dissolve, index gi denotes the
		// NEXT group (which always has >= 2 members), so a stay-in-group
		// branch keyed on groups[gi] would teleport the user into an
		// unrelated group's members under the same delete binding.
		groups := []dupGroup{mk("e1", "a", "b"), mk("e2", "x", "y")}
		got, same := dropDupMember(groups, 0, "a")
		if same {
			t.Fatal("a dissolved group must return to the group list")
		}
		if len(got) != 1 || got[0].ETag != "e2" {
			t.Fatalf("groups after dissolve = %+v", got)
		}
	})

	t.Run("dissolving the last group", func(t *testing.T) {
		groups := []dupGroup{mk("e1", "a", "b")}
		got, same := dropDupMember(groups, 0, "b")
		if same || len(got) != 0 {
			t.Fatalf("got %+v, same=%v", got, same)
		}
	})
}
