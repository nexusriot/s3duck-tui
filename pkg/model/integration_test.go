//go:build integration

// Integration tests against a real S3-compatible endpoint.
//
// These are excluded from the default build by the `integration` tag, because
// they need a live server. Run them with:
//
//	make test-integration                      # starts MinIO in Docker
//	go test -tags integration ./pkg/model/...  # against an endpoint you provide
//
// Configure the target with S3DUCK_TEST_ENDPOINT / _ACCESS_KEY / _SECRET_KEY /
// _REGION. Without an endpoint the whole file skips, so a tagged run on a
// machine with no server is a no-op rather than a failure.
//
// They exist because the interesting behaviour of this package is *not* in its
// pure functions: that a restore adds a version rather than rewinding, that a
// storage-class change preserves metadata, that a prefix-like key is refused —
// none of that can be asserted without a server.
package model_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// testCreds resolves the target endpoint and credentials, skipping the calling
// test when no endpoint is configured.
func testCreds(t *testing.T) (endpoint, region, access, secret string) {
	t.Helper()

	endpoint = os.Getenv("S3DUCK_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3DUCK_TEST_ENDPOINT not set; skipping integration tests")
	}
	return endpoint,
		envOr("S3DUCK_TEST_REGION", "us-east-1"),
		envOr("S3DUCK_TEST_ACCESS_KEY", "minioadmin"),
		envOr("S3DUCK_TEST_SECRET_KEY", "minioadmin")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func testModel(t *testing.T) *model.Model {
	t.Helper()
	endpoint, region, access, secret := testCreds(t)
	return model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 0))
}

// freshBucket creates an empty bucket for one test, removing anything left by a
// previous run so the suite is re-runnable against a persistent server.
func freshBucket(t *testing.T, m *model.Model, name string) *model.Object {
	t.Helper()
	ctx := context.Background()

	_ = m.CreateBucket(&name, false)
	bucket := &model.Object{Key: &name, Ot: model.Bucket}

	objs, err := m.ListObjects("", bucket)
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

// putObject uploads body at key via the same path the app uses.
func putObject(t *testing.T, m *model.Model, bucket *model.Object, key, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadFile(context.Background(), p, key, bucket, nil); err != nil {
		t.Fatalf("upload %s: %v", key, err)
	}
}

func relNames(es []model.SyncEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Rel)
	}
	sort.Strings(out)
	return out
}

func TestIntegrationTransferRoundTrip(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-transfer")

	root := t.TempDir()
	for rel, body := range map[string]string{
		"a.txt":         "hello",
		"dir/b.txt":     "worldly",
		"dir/sub/c.bin": "deep content",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	local, err := model.WalkLocal(root)
	if err != nil {
		t.Fatalf("WalkLocal: %v", err)
	}
	if got := relNames(local); len(got) != 3 {
		t.Fatalf("walked %v, want 3 files (empty dirs excluded)", got)
	}

	prefix := model.NormalizePrefix("tree")
	for _, e := range local {
		if err := m.UploadFile(ctx, filepath.Join(root, filepath.FromSlash(e.Rel)), prefix+e.Rel, bucket, nil); err != nil {
			t.Fatalf("upload %s: %v", e.Rel, err)
		}
	}

	remote, err := m.ListRemoteEntries("tree", bucket)
	if err != nil {
		t.Fatalf("ListRemoteEntries: %v", err)
	}
	if got, want := relNames(remote), relNames(local); !slices.Equal(got, want) {
		t.Errorf("remote keys = %v, want them relative to the prefix: %v", got, want)
	}
	for _, e := range remote {
		if e.Mod.IsZero() {
			t.Errorf("%s has no modification time", e.Rel)
		}
	}

	t.Run("download reproduces the tree", func(t *testing.T) {
		dst := t.TempDir()
		for _, e := range remote {
			if _, err := m.DownloadTarget(ctx,
				model.DownloadTarget{Key: prefix + e.Rel, Size: e.Size},
				prefix, dst, bucket.Key, nil); err != nil {
				t.Fatalf("download %s: %v", e.Rel, err)
			}
		}
		got, err := os.ReadFile(filepath.Join(dst, "dir", "sub", "c.bin"))
		if err != nil || string(got) != "deep content" {
			t.Errorf("round-trip content = %q, %v", got, err)
		}
	})

	t.Run("download refuses to clobber", func(t *testing.T) {
		dst := t.TempDir()
		if err := os.WriteFile(filepath.Join(dst, "a.txt"), []byte("mine"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := m.DownloadTarget(ctx, model.DownloadTarget{Key: prefix + "a.txt", Size: 5}, prefix, dst, bucket.Key, nil)
		if !errors.Is(err, model.ErrFileExists) {
			t.Fatalf("err = %v, want ErrFileExists", err)
		}
		if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "mine" {
			t.Errorf("existing file was modified: %q", got)
		}
	})

	t.Run("DeleteKey removes one object and refuses prefixes", func(t *testing.T) {
		if err := m.DeleteKey(ctx, prefix+"a.txt", bucket); err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		after, _ := m.ListRemoteEntries("tree", bucket)
		if len(after) != 2 {
			t.Errorf("got %d objects, want 2", len(after))
		}

		if err := m.DeleteKey(ctx, prefix, bucket); err == nil {
			t.Error("a prefix-like key must be refused")
		}
		still, _ := m.ListRemoteEntries("tree", bucket)
		if len(still) != 2 {
			t.Errorf("the refused delete removed %d objects", 2-len(still))
		}
	})
}

func TestIntegrationSessionTokenReachesTheWire(t *testing.T) {
	// The only way to prove the session token is actually sent: a bogus one must
	// be rejected while the same credentials without one succeed.
	endpoint, region, access, secret := testCreds(t)

	good := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 0))
	if _, err := good.ListBuckets(); err != nil {
		t.Fatalf("baseline ListBuckets failed: %v", err)
	}

	bad := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "bogus-session-token", true, 0))
	if _, err := bad.ListBuckets(); err == nil {
		t.Error("a garbage session token was accepted; the token is not being sent")
	}
}

func TestIntegrationMetadataTagsAndStorageClass(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-meta")
	key := "meta/obj.txt"
	putObject(t, m, bucket, key, "hello")

	want := model.ObjectMeta{
		ContentType:  "text/markdown",
		CacheControl: "max-age=99",
		UserMetadata: map[string]string{"owner": "vlad", "purpose": "integration"},
	}
	if err := m.PutObjectMeta(ctx, bucket, key, want); err != nil {
		t.Fatalf("PutObjectMeta: %v", err)
	}

	got, err := m.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if got.ContentType != want.ContentType || got.CacheControl != want.CacheControl {
		t.Errorf("headers not persisted: %+v", got)
	}
	if got.UserMetadata["owner"] != "vlad" || got.UserMetadata["purpose"] != "integration" {
		t.Errorf("x-amz-meta round-trip = %v", got.UserMetadata)
	}

	t.Run("tags round-trip and clear", func(t *testing.T) {
		if err := m.PutObjectTags(ctx, bucket, key, map[string]string{"env": "test", "team": "infra"}); err != nil {
			t.Fatalf("PutObjectTags: %v", err)
		}
		tags, err := m.ObjectTags(ctx, bucket, key)
		if err != nil || tags["env"] != "test" || tags["team"] != "infra" {
			t.Fatalf("tags = %v, err = %v", tags, err)
		}
		if err := m.PutObjectTags(ctx, bucket, key, map[string]string{}); err != nil {
			t.Fatalf("clearing tags: %v", err)
		}
		if tags, _ := m.ObjectTags(ctx, bucket, key); len(tags) != 0 {
			t.Errorf("tags after clear = %v", tags)
		}
	})

	t.Run("a storage-class change preserves metadata", func(t *testing.T) {
		if err := m.SetStorageClass(ctx, bucket, key, "REDUCED_REDUNDANCY"); err != nil {
			t.Fatalf("SetStorageClass: %v", err)
		}
		after, err := m.HeadObject(ctx, bucket, key)
		if err != nil {
			t.Fatal(err)
		}
		if after.StorageClass != "REDUCED_REDUNDANCY" {
			t.Errorf("StorageClass = %q", after.StorageClass)
		}
		// The reason SetStorageClass leaves MetadataDirective at COPY.
		if after.ContentType != want.ContentType {
			t.Errorf("metadata was lost by the class change: ContentType = %q", after.ContentType)
		}
	})
}

func TestIntegrationVersioning(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-versions")

	if _, err := m.Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(*bucket.Key),
		VersioningConfiguration: &s3t.VersioningConfiguration{Status: s3t.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Skipf("endpoint does not support bucket versioning: %v", err)
	}

	key := "docs/doc.txt"

	// freshBucket only removes CURRENT objects — on a versioned bucket old
	// versions and delete markers survive (the exact semantics this test
	// verifies), so a rerun against a persistent server would inherit them.
	for _, k := range []string{key, key + ".bak"} {
		vs, _ := m.ListVersions(ctx, bucket, k)
		for _, v := range vs {
			if err := m.DeleteVersion(ctx, bucket, k, v.VersionID); err != nil {
				t.Fatalf("purging leftover version %s of %s: %v", v.VersionID, k, err)
			}
		}
	}

	for _, body := range []string{"version one", "version two!!", "version three"} {
		putObject(t, m, bucket, key, body)
	}

	versions, err := m.ListVersions(ctx, bucket, key)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}
	if !versions[0].IsLatest {
		t.Error("versions are not newest-first")
	}
	if versions[0].Size != 13 || versions[2].Size != 11 {
		t.Errorf("sizes = %d..%d, want newest 13 and oldest 11", versions[0].Size, versions[2].Size)
	}

	t.Run("a sibling key does not leak into the history", func(t *testing.T) {
		putObject(t, m, bucket, key+".bak", "x")
		vs, _ := m.ListVersions(ctx, bucket, key)
		if len(vs) != 3 {
			t.Errorf("got %d versions, want 3 — ListObjectVersions is prefix-based and must be filtered to the exact key", len(vs))
		}
	})

	oldest := versions[2]

	t.Run("download fetches that version, not the latest", func(t *testing.T) {
		dst := t.TempDir()
		name := model.VersionFileName(key, oldest.VersionID)
		if _, err := m.DownloadVersion(ctx, bucket, key, oldest.VersionID, dst, name); err != nil {
			t.Fatalf("DownloadVersion: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(dst, name))
		if string(got) != "version one" {
			t.Errorf("content = %q, want the oldest version", got)
		}
	})

	t.Run("restore adds a version rather than rewinding", func(t *testing.T) {
		if err := m.RestoreVersion(ctx, bucket, key, oldest.VersionID, oldest.StorageClass); err != nil {
			t.Fatalf("RestoreVersion: %v", err)
		}
		vs, _ := m.ListVersions(ctx, bucket, key)
		if len(vs) != 4 {
			t.Errorf("got %d versions, want 4 — restore must not discard history", len(vs))
		}
		head, err := m.HeadObject(ctx, bucket, key)
		if err != nil || head.Size != 11 {
			t.Errorf("current size = %d (%v), want the restored 11", head.Size, err)
		}
	})

	t.Run("deleting a version leaves no delete marker", func(t *testing.T) {
		before, _ := m.ListVersions(ctx, bucket, key)
		if err := m.DeleteVersion(ctx, bucket, key, oldest.VersionID); err != nil {
			t.Fatalf("DeleteVersion: %v", err)
		}
		after, _ := m.ListVersions(ctx, bucket, key)
		if len(after) != len(before)-1 {
			t.Errorf("got %d versions, want %d", len(after), len(before)-1)
		}
		for _, v := range after {
			if v.IsDeleteMark {
				t.Error("a version delete must not add a delete marker")
			}
		}
	})

	t.Run("an ordinary delete does add one", func(t *testing.T) {
		if err := m.DeleteKey(ctx, key, bucket); err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		vs, _ := m.ListVersions(ctx, bucket, key)
		markers := 0
		for _, v := range vs {
			if v.IsDeleteMark {
				markers++
			}
		}
		if markers != 1 {
			t.Errorf("got %d delete markers, want 1", markers)
		}
	})
}

func TestIntegrationRemoteToRemote(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	src := freshBucket(t, m, "s3duck-it-rsrc")
	dst := freshBucket(t, m, "s3duck-it-rdst")

	srcPrefix := model.NormalizePrefix("data")
	dstPrefix := model.NormalizePrefix("mirror")

	putObject(t, m, src, srcPrefix+"same.txt", "identical")
	putObject(t, m, src, srcPrefix+"changed.txt", "new content long")
	putObject(t, m, src, srcPrefix+"nested/deep.txt", "deep")
	putObject(t, m, dst, dstPrefix+"same.txt", "identical")
	putObject(t, m, dst, dstPrefix+"changed.txt", "old")
	putObject(t, m, dst, dstPrefix+"extra.txt", "should go")

	srcEntries, err := m.ListRemoteEntries(srcPrefix, src)
	if err != nil {
		t.Fatal(err)
	}
	dstEntries, err := m.ListRemoteEntries(dstPrefix, dst)
	if err != nil {
		t.Fatal(err)
	}

	// The controller's planSync is unexported; this mirrors its rules so the
	// server-side half (CopyObject / DeleteKey) is what's under test here.
	dstBy := map[string]model.SyncEntry{}
	for _, e := range dstEntries {
		dstBy[e.Rel] = e
	}
	seen := map[string]bool{}
	for _, e := range srcEntries {
		seen[e.Rel] = true
		if x, ok := dstBy[e.Rel]; !ok || x.Size != e.Size {
			if err := m.CopyObject(ctx, src, dst, srcPrefix+e.Rel, dstPrefix+e.Rel); err != nil {
				t.Fatalf("CopyObject %s: %v", e.Rel, err)
			}
		}
	}
	for _, e := range dstEntries {
		if !seen[e.Rel] {
			if err := m.DeleteKey(ctx, dstPrefix+e.Rel, dst); err != nil {
				t.Fatalf("DeleteKey %s: %v", e.Rel, err)
			}
		}
	}

	after, _ := m.ListRemoteEntries(dstPrefix, dst)
	if got, want := relNames(after), relNames(srcEntries); !slices.Equal(got, want) {
		t.Errorf("destination = %v, want it to match the source %v", got, want)
	}
	if sizeOfRel(after, "changed.txt") != sizeOfRel(srcEntries, "changed.txt") {
		t.Error("the stale object was not refreshed")
	}
	if sizeOfRel(after, "nested/deep.txt") != 4 {
		t.Error("the nested key lost its path")
	}
	if sizeOfRel(after, "extra.txt") != -1 {
		t.Error("the extraneous object was not removed")
	}

	t.Run("the source is untouched", func(t *testing.T) {
		again, _ := m.ListRemoteEntries(srcPrefix, src)
		if len(again) != 3 {
			t.Errorf("source now has %d objects, want its original 3", len(again))
		}
	})
}

func sizeOfRel(es []model.SyncEntry, rel string) int64 {
	for _, e := range es {
		if e.Rel == rel {
			return e.Size
		}
	}
	return -1
}

func TestIntegrationPlusInKeys(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-plus")
	key := "dir/report+final v2.pdf"
	putObject(t, m, bucket, key, "plus content")

	t.Run("server-side copy resolves the exact key", func(t *testing.T) {
		if err := m.CopyObject(ctx, bucket, bucket, key, "dir/copy+of it.pdf"); err != nil {
			t.Fatalf("CopyObject with + in key: %v", err)
		}
	})

	t.Run("metadata rewrite (copy-onto-self) resolves the exact key", func(t *testing.T) {
		if err := m.PutObjectMeta(ctx, bucket, key, model.ObjectMeta{ContentType: "application/pdf"}); err != nil {
			t.Fatalf("PutObjectMeta with + in key: %v", err)
		}
		meta, err := m.HeadObject(ctx, bucket, key)
		if err != nil || meta.ContentType != "application/pdf" {
			t.Errorf("meta = %+v, err = %v", meta, err)
		}
	})
}

func TestIntegrationDeleteNonEmptyBucket(t *testing.T) {
	m := testModel(t)
	bucket := freshBucket(t, m, "s3duck-it-delbucket")
	putObject(t, m, bucket, "keep/a.txt", "x")
	putObject(t, m, bucket, "b.txt", "y")

	// The delete flow promises the objects go too: EmptyBucket then
	// DeleteBucket must succeed on a non-empty (unversioned) bucket.
	if err := m.EmptyBucket(bucket); err != nil {
		t.Fatalf("EmptyBucket: %v", err)
	}
	if err := m.DeleteBucket(bucket.Key); err != nil {
		t.Fatalf("DeleteBucket after emptying: %v", err)
	}
}

func TestIntegrationUploadMatchesPreparedKeys(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-prep")

	root := filepath.Join(t.TempDir(), "mytree")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"a.txt": "aa", "sub/b.txt": "bbbb"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}

	targets, _, err := m.PrepareUpload(root, "pre", bucket)
	if err != nil {
		t.Fatalf("PrepareUpload: %v", err)
	}
	if err := m.Upload(ctx, root, "pre", bucket, nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Every key the preview promised must exist exactly as promised — this is
	// the divergence that used to make the modal lie about destinations.
	for _, tg := range targets {
		if _, err := m.HeadObject(ctx, bucket, tg.RemotePath); err != nil {
			t.Errorf("promised key %q was not uploaded: %v", tg.RemotePath, err)
		}
	}
}

func TestIntegrationEmptyDirUpload(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-emptydir")

	empty := filepath.Join(t.TempDir(), "vacant")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	if err := m.Upload(ctx, empty, "", bucket, nil); err != nil {
		t.Fatalf("Upload of an empty dir: %v", err)
	}
	objs, err := m.ListObjects("", bucket)
	if err != nil || len(objs) != 1 {
		t.Fatalf("objs = %d, err = %v; want exactly the folder marker", len(objs), err)
	}
	if got := *objs[0].Key; got != "vacant/" {
		t.Errorf("marker key = %q, want %q", got, "vacant/")
	}
}
