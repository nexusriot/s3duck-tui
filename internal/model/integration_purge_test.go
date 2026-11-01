//go:build integration

package model_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// versionedBucket is a fresh bucket with versioning on and a unique name, so
// leftovers from an earlier run (versions survive the plain cleanup in
// freshBucket) cannot leak into this one.
func versionedBucket(t *testing.T, m *model.Model, base string) *model.Object {
	t.Helper()
	name := base + "-" + uniqueSuffix()
	if err := m.CreateBucket(&name, false); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	b := model.BucketObject(name)
	if err := m.SetBucketVersioning(context.Background(), b, true); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	t.Cleanup(func() {
		_ = m.EmptyBucket(context.Background(), b)
		_ = m.DeleteBucket(&name)
	})
	return b
}

func countVersions(t *testing.T, m *model.Model, b *model.Object, prefix string) (versions, markers int) {
	t.Helper()
	err := m.ListVersionsUnder(context.Background(), b, prefix, func(p []model.VersionRef) bool {
		for _, r := range p {
			if r.IsDeleteMark {
				markers++
			} else {
				versions++
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return versions, markers
}

// The whole versioned lifecycle on a real server: a deleted key is found,
// undelete brings back its newest version, an old-versions purge keeps what
// the key reads as now, an every-version purge leaves nothing.
func TestIntegrationDeletedUndeleteAndPurge(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	b := versionedBucket(t, m, "s3duck-it-purge")

	putObject(t, m, b, "docs/a.txt", "one")
	putObject(t, m, b, "docs/a.txt", "two")
	putObject(t, m, b, "docs/a.txt", "three!")
	putObject(t, m, b, "docs/keep.txt", "k")
	key := "docs/a.txt"
	if err := m.Delete(ctx, &key, b); err != nil { // an ordinary delete: a marker
		t.Fatal(err)
	}

	found, _, err := m.FindDeleted(ctx, b, "docs/", 0, nil)
	if err != nil || len(found) != 1 || found[0].Key != "docs/a.txt" || found[0].Versions != 3 || found[0].Size != 6 {
		t.Fatalf("FindDeleted = %+v, %v", found, err)
	}
	if err := m.Undelete(ctx, b, found[0]); err != nil {
		t.Fatal(err)
	}
	c, err := m.GetObjectContent(ctx, b, "docs/a.txt", 100)
	if err != nil || string(c.Data) != "three!" {
		t.Fatalf("after undelete: %q, %v", c.Data, err)
	}

	var refs []model.VersionRef
	_ = m.ListVersionsUnder(ctx, b, key, func(p []model.VersionRef) bool { refs = append(refs, p...); return true })
	n, failures, err := m.DeleteVersions(ctx, b, model.SelectPurge(refs, model.PurgeNoncurrent, key))
	if err != nil || len(failures) != 0 || n != 2 {
		t.Fatalf("old-versions purge: %d deleted, %v, %v", n, failures, err)
	}
	if v, mk := countVersions(t, m, b, key); v != 1 || mk != 0 {
		t.Errorf("after the old-versions purge: %d version(s), %d marker(s); want just the current", v, mk)
	}
	if c, err := m.GetObjectContent(ctx, b, key, 100); err != nil || string(c.Data) != "three!" {
		t.Errorf("the current version did not survive: %q, %v", c.Data, err)
	}

	refs = nil
	_ = m.ListVersionsUnder(ctx, b, "docs/", func(p []model.VersionRef) bool { refs = append(refs, p...); return true })
	if _, failures, err := m.DeleteVersions(ctx, b, model.SelectPurge(refs, model.PurgeAll, "")); err != nil || len(failures) != 0 {
		t.Fatalf("every-version purge: %v %v", failures, err)
	}
	if v, mk := countVersions(t, m, b, ""); v != 0 || mk != 0 {
		t.Errorf("after purging everything: %d version(s), %d marker(s)", v, mk)
	}
}

// A versioned bucket with history and markers can be deleted: emptying it
// purges by version ID. It used to write markers and leave it BucketNotEmpty.
func TestIntegrationDeleteVersionedBucket(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	name := "s3duck-it-delver-" + uniqueSuffix()
	if err := m.CreateBucket(&name, false); err != nil {
		t.Fatal(err)
	}
	b := model.BucketObject(name)
	if err := m.SetBucketVersioning(ctx, b, true); err != nil {
		t.Fatal(err)
	}
	putObject(t, m, b, "x", "1")
	putObject(t, m, b, "x", "2")
	k := "x"
	_ = m.Delete(ctx, &k, b)

	if err := m.EmptyBucket(ctx, b); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := m.DeleteBucket(&name); err != nil {
		t.Fatalf("delete a versioned bucket after emptying it: %v", err)
	}
}

// uniqueSuffix keeps bucket names from colliding across runs.
func uniqueSuffix() string { return strconv.FormatInt(time.Now().UnixNano()%1e9, 36) }
