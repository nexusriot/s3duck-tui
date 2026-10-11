//go:build integration

// Integration tests for the flows the controller composes out of the model:
// sync and safe delete.
//
// They live here rather than in internal/model because neither flow *is* a
// model call. Sync is a diff between two listings followed by a per-entry
// apply, and safe delete is a server-side move into a trash prefix — the
// model supplies the pieces, and the decisions that can lose data are the
// composition. internal/model's suite covers the pieces and cannot see the
// composition at all; the unit suite covers planSync's arithmetic against
// hand-built entry lists and never touches a server.
//
// That gap is not hypothetical: Empty Trash shipped as a silent no-op, because
// nothing exercised the trash flow end to end.
//
// Run with: make test-integration (or -tags integration against your own
// endpoint via S3DUCK_TEST_ENDPOINT).
package controller

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// itModel builds a client against the configured endpoint, skipping when none
// is set so a tagged run on a machine with no server is a no-op.
func itModel(t *testing.T) *model.Model {
	t.Helper()
	endpoint := os.Getenv("S3DUCK_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3DUCK_TEST_ENDPOINT not set; skipping integration tests")
	}
	region := itEnv("S3DUCK_TEST_REGION", "us-east-1")
	m, err := model.NewModel(model.NewConfig(endpoint,
		&region,
		itEnv("S3DUCK_TEST_ACCESS_KEY", "minioadmin"),
		itEnv("S3DUCK_TEST_SECRET_KEY", "minioadmin"),
		"", true, 0))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	return m
}

func itEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// itBucket creates an empty bucket, clearing anything a previous run left so
// the suite is re-runnable against a persistent server.
func itBucket(t *testing.T, m *model.Model, name string) *model.Object {
	t.Helper()
	ctx := context.Background()
	_ = m.CreateBucket(&name, false)
	bucket := &model.Object{Key: &name, Ot: model.Bucket}
	objs, err := m.ListObjects(ctx, "", bucket)
	if err != nil {
		t.Fatalf("listing %s: %v", name, err)
	}
	for _, o := range objs {
		if o.Key != nil {
			_ = m.DeleteKey(ctx, *o.Key, bucket)
		}
	}
	return bucket
}

// itPut writes one object through the same path an upload takes.
func itPut(t *testing.T, m *model.Model, bucket *model.Object, key, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadFile(context.Background(), p, key, bucket, nil); err != nil {
		t.Fatalf("upload %s: %v", key, err)
	}
}

// itKeys lists every key under prefix, sorted.
func itKeys(t *testing.T, m *model.Model, bucket *model.Object, prefix string) []string {
	t.Helper()
	objs, err := m.ListObjects(context.Background(), prefix, bucket)
	if err != nil {
		t.Fatalf("ListObjects(%q): %v", prefix, err)
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.Key != nil {
			out = append(out, *o.Key)
		}
	}
	sort.Strings(out)
	return out
}

// itTree writes a local file tree and returns its root.
func itTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runSyncPlan applies every operation in a reviewed plan, the way ops.Sync
// drives it: writes first, then deletes.
func runSyncPlan(t *testing.T, c *Controller, mdl *model.Model, spec syncSpec, plan []syncOp) {
	t.Helper()
	ctx := context.Background()
	writes, deletes := []syncOp{}, []syncOp{}
	for _, op := range plan {
		if op.Kind == syncDelete {
			deletes = append(deletes, op)
			continue
		}
		writes = append(writes, op)
	}
	for _, op := range append(writes, deletes...) {
		if err := c.applySyncOp(ctx, mdl, spec, op, func(int64) {}); err != nil {
			t.Fatalf("applySyncOp(%s %s): %v", op.Kind, op.Rel, err)
		}
	}
}

// TestIntegrationSyncUploadRoundTrip walks the full local→remote sync: scan
// both sides, diff them, apply, and confirm the destination ends up mirroring
// the source. Delete-extraneous is the half that can destroy data, so it is
// asserted in both directions: an object the source no longer has must go, and
// one outside the synced prefix must not.
func TestIntegrationSyncUploadRoundTrip(t *testing.T) {
	m := itModel(t)
	c := &Controller{}
	ctx := context.Background()
	bucket := itBucket(t, m, "s3duck-it-sync-up")

	root := itTree(t, map[string]string{
		"a.txt":       "alpha",
		"dir/b.txt":   "bravo",
		"dir/c/d.txt": "delta",
	})

	// Pre-existing remote state: one object the source no longer has (must be
	// deleted), one that differs (must be updated), and one outside the synced
	// prefix (must survive).
	itPut(t, m, bucket, "mirror/stale.txt", "to be removed")
	itPut(t, m, bucket, "mirror/a.txt", "old content, different size")
	itPut(t, m, bucket, "untouched/keep.txt", "not part of this sync")

	spec := syncSpec{
		dir:       syncUpload,
		localDir:  root,
		dstBucket: bucket,
		dstPrefix: "mirror/",
		del:       true,
	}

	src, dst, err := c.collectSides(ctx, m, spec)
	if err != nil {
		t.Fatalf("collectSides: %v", err)
	}
	plan := planSync(src, dst, spec.del)

	st := summarizeSync(plan)
	if st.Creates != 2 || st.Updates != 1 || st.Deletes != 1 {
		t.Fatalf("plan = %d creates, %d updates, %d deletes; want 2/1/1 (plan: %v)",
			st.Creates, st.Updates, st.Deletes, plan)
	}

	runSyncPlan(t, c, m, spec, plan)

	want := []string{"mirror/a.txt", "mirror/dir/b.txt", "mirror/dir/c/d.txt"}
	if got := itKeys(t, m, bucket, "mirror/"); !equalStrings(got, want) {
		t.Errorf("after sync the destination holds %v, want %v", got, want)
	}
	if got := itKeys(t, m, bucket, "untouched/"); len(got) != 1 {
		t.Errorf("sync reached outside its prefix: %v", got)
	}

	// The updated object must carry the source's bytes, not the old ones.
	content, err := m.GetObjectContent(ctx, bucket, "mirror/a.txt", 1<<20)
	if err != nil {
		t.Fatalf("GetObjectContent: %v", err)
	}
	if string(content.Data) != "alpha" {
		t.Errorf("mirror/a.txt = %q, want %q", content.Data, "alpha")
	}

	// A second run over an already-mirrored tree must plan nothing at all;
	// otherwise sync re-uploads (and re-deletes) on every invocation.
	src, dst, err = c.collectSides(ctx, m, spec)
	if err != nil {
		t.Fatalf("collectSides (second pass): %v", err)
	}
	if again := planSync(src, dst, spec.del); len(again) != 0 {
		t.Errorf("a second sync of an unchanged tree planned %d operation(s): %v", len(again), again)
	}
}

// TestIntegrationSyncDownloadRoundTrip is the mirror image: remote is the
// source of truth and the local tree is made to match, including removing a
// local file the bucket no longer has.
func TestIntegrationSyncDownloadRoundTrip(t *testing.T) {
	m := itModel(t)
	c := &Controller{}
	ctx := context.Background()
	bucket := itBucket(t, m, "s3duck-it-sync-down")

	itPut(t, m, bucket, "src/a.txt", "alpha")
	itPut(t, m, bucket, "src/dir/b.txt", "bravo")

	root := itTree(t, map[string]string{
		"stale.txt": "no longer upstream",
		"a.txt":     "stale and a different size entirely",
	})

	spec := syncSpec{
		dir:       syncDownload,
		localDir:  root,
		srcBucket: bucket,
		srcPrefix: "src/",
		del:       true,
	}

	src, dst, err := c.collectSides(ctx, m, spec)
	if err != nil {
		t.Fatalf("collectSides: %v", err)
	}
	plan := planSync(src, dst, spec.del)
	runSyncPlan(t, c, m, spec, plan)

	local, err := model.WalkLocal(root)
	if err != nil {
		t.Fatalf("WalkLocal: %v", err)
	}
	var got []string
	for _, e := range local {
		got = append(got, e.Rel)
	}
	sort.Strings(got)
	if want := []string{"a.txt", "dir/b.txt"}; !equalStrings(got, want) {
		t.Errorf("local tree = %v, want %v", got, want)
	}

	body, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("read back a.txt: %v", err)
	}
	if string(body) != "alpha" {
		t.Errorf("a.txt = %q, want %q", body, "alpha")
	}
}

// TestIntegrationSyncChecksumSeesInPlaceEdit covers the comparison mode that
// exists precisely because size+mtime cannot see it: a file rewritten to the
// same length with its timestamp preserved. The remote sum comes from the
// object, the local one is computed with the algorithm the remote reported, so
// this only works against a real endpoint.
func TestIntegrationSyncChecksumSeesInPlaceEdit(t *testing.T) {
	m := itModel(t)
	c := &Controller{}
	ctx := context.Background()
	bucket := itBucket(t, m, "s3duck-it-sync-sum")

	root := itTree(t, map[string]string{"note.txt": "aaaaa"})
	local := filepath.Join(root, "note.txt")

	spec := syncSpec{
		dir: syncUpload, localDir: root,
		dstBucket: bucket, dstPrefix: "m/", checksum: true,
	}

	// First pass uploads it.
	src, dst, err := c.collectSides(ctx, m, spec)
	if err != nil {
		t.Fatalf("collectSides: %v", err)
	}
	runSyncPlan(t, c, m, spec, planSync(src, dst, false))

	// Rewrite to the same length and restore the old mtime, so size+mtime
	// cannot tell the two apart.
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("bbbbb"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(local, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	// Without content comparison the edit is invisible — that is the
	// documented hole this mode closes, so pin it rather than assume it.
	plain := spec
	plain.checksum = false
	src, dst, err = c.collectSides(ctx, m, plain)
	if err != nil {
		t.Fatalf("collectSides (size+mtime): %v", err)
	}
	if blind := planSync(src, dst, false); len(blind) != 0 {
		t.Errorf("size+mtime unexpectedly saw the edit (%v); the checksum assertion below proves nothing", blind)
	}

	src, dst, err = c.collectSides(ctx, m, spec)
	if err != nil {
		t.Fatalf("collectSides (checksum): %v", err)
	}
	plan := planSync(src, dst, false)
	if len(plan) != 1 || plan[0].Kind != syncUpdate || plan[0].Rel != "note.txt" {
		t.Fatalf("checksum sync planned %v, want one update of note.txt", plan)
	}
	if !strings.Contains(plan[0].Reason, "content differs") {
		t.Errorf("update reason = %q, want it to name the content comparison", plan[0].Reason)
	}

	runSyncPlan(t, c, m, spec, plan)
	content, err := m.GetObjectContent(ctx, bucket, "m/note.txt", 1<<20)
	if err != nil {
		t.Fatalf("GetObjectContent: %v", err)
	}
	if string(content.Data) != "bbbbb" {
		t.Errorf("remote content = %q, want the edited %q", content.Data, "bbbbb")
	}
}

// TestIntegrationSyncExcludesAreInvisibleToDeletes pins the safety property
// applyExcludes exists for: an excluded path must not be seen by the planner
// at all, or a delete-extraneous run would decide the destination's copy is
// extraneous and remove it.
func TestIntegrationSyncExcludesProtectTheDestination(t *testing.T) {
	m := itModel(t)
	c := &Controller{}
	ctx := context.Background()
	bucket := itBucket(t, m, "s3duck-it-sync-excl")

	root := itTree(t, map[string]string{
		"keep.txt":          "kept",
		"node_modules/x.js": "vendored",
	})
	// The destination already holds the excluded tree.
	itPut(t, m, bucket, "m/node_modules/x.js", "vendored")

	spec := syncSpec{
		dir: syncUpload, localDir: root,
		dstBucket: bucket, dstPrefix: "m/",
		del: true, exclude: []string{"node_modules"},
	}

	src, dst, err := c.collectSides(ctx, m, spec)
	if err != nil {
		t.Fatalf("collectSides: %v", err)
	}
	plan := planSync(src, dst, spec.del)
	for _, op := range plan {
		if strings.Contains(op.Rel, "node_modules") {
			t.Fatalf("an excluded path reached the plan: %s %s", op.Kind, op.Rel)
		}
	}

	runSyncPlan(t, c, m, spec, plan)

	if got := itKeys(t, m, bucket, "m/node_modules/"); len(got) != 1 {
		t.Errorf("the excluded destination tree was touched: %v", got)
	}
}

// TestIntegrationTrashRoundTrip covers safe delete end to end: a file and a
// folder move under the trash prefix instead of being removed, the key they
// came from is recoverable, and deleting something already in the trash
// removes it for real — which is what makes Empty Trash terminate instead of
// nesting the trash inside itself.
func TestIntegrationTrashRoundTrip(t *testing.T) {
	m := itModel(t)
	c := &Controller{}
	ctx := context.Background()
	bucket := itBucket(t, m, "s3duck-it-trash")
	const trash = ".s3duck-trash/"

	itPut(t, m, bucket, "docs/report.pdf", "report body")
	itPut(t, m, bucket, "docs/sub/notes.txt", "notes")
	itPut(t, m, bucket, "loose.txt", "loose")

	// A file goes to the trash, not away.
	if err := c.trashTarget(ctx, m, bucket, deleteTarget{name: "loose.txt", key: "loose.txt"}, trash); err != nil {
		t.Fatalf("trashTarget(file): %v", err)
	}
	if got := itKeys(t, m, bucket, "loose.txt"); len(got) != 0 {
		t.Errorf("the original survived a trash delete: %v", got)
	}
	trashed := itKeys(t, m, bucket, trash)
	if len(trashed) != 1 || !strings.HasSuffix(trashed[0], "/loose.txt") {
		t.Fatalf("trash holds %v, want one entry ending in /loose.txt", trashed)
	}

	// The key it came from is recoverable from where it landed.
	if got := restoreKeyFrom(trash, trashed[0]); got != "loose.txt" {
		t.Fatalf("restoreKeyFrom(%q) = %q, want loose.txt", trashed[0], got)
	}

	// A folder goes whole, keeping its shape underneath.
	if err := c.trashTarget(ctx, m, bucket, deleteTarget{name: "docs/", key: "docs/", isFolder: true}, trash); err != nil {
		t.Fatalf("trashTarget(folder): %v", err)
	}
	if got := itKeys(t, m, bucket, "docs/"); len(got) != 0 {
		t.Errorf("the original folder survived a trash delete: %v", got)
	}
	var restoredReport string
	for _, k := range itKeys(t, m, bucket, trash) {
		if strings.HasSuffix(k, "/docs/report.pdf") {
			restoredReport = k
		}
	}
	if restoredReport == "" {
		t.Fatalf("the folder's contents are not under the trash: %v", itKeys(t, m, bucket, trash))
	}
	if got := restoreKeyFrom(trash, restoredReport); got != "docs/report.pdf" {
		t.Errorf("restoreKeyFrom(%q) = %q, want docs/report.pdf", restoredReport, got)
	}

	// Restoring puts it back where it was.
	if _, err := m.MoveKeys(ctx, bucket, bucket, restoredReport, "docs/report.pdf", false, nil, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	content, err := m.GetObjectContent(ctx, bucket, "docs/report.pdf", 1<<20)
	if err != nil {
		t.Fatalf("the restored object is not readable: %v", err)
	}
	if string(content.Data) != "report body" {
		t.Errorf("restored content = %q, want %q", content.Data, "report body")
	}

	// Deleting inside the trash is a real delete, not another move: this is
	// what Empty Trash relies on, and what stops the trash nesting in itself.
	before := itKeys(t, m, bucket, trash)
	if len(before) == 0 {
		t.Fatal("nothing left in the trash to empty")
	}
	if err := c.trashTarget(ctx, m, bucket, deleteTarget{name: trash, key: trash, isFolder: true}, trash); err != nil {
		t.Fatalf("emptying the trash: %v", err)
	}
	if after := itKeys(t, m, bucket, trash); len(after) != 0 {
		t.Errorf("the trash still holds %v after being emptied", after)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
