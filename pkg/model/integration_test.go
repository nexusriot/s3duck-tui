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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
	m, err := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 0))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	return m
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
				prefix, dst, bucket.Key, false, nil); err != nil {
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
		_, err := m.DownloadTarget(ctx, model.DownloadTarget{Key: prefix + "a.txt", Size: 5}, prefix, dst, bucket.Key, false, nil)
		if !errors.Is(err, model.ErrFileExists) {
			t.Fatalf("err = %v, want ErrFileExists", err)
		}
		if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "mine" {
			t.Errorf("existing file was modified: %q", got)
		}
	})

	t.Run("overwrite replaces the file only after a full download", func(t *testing.T) {
		dst := t.TempDir()
		if err := os.WriteFile(filepath.Join(dst, "a.txt"), []byte("stale local"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := m.DownloadTarget(ctx, model.DownloadTarget{Key: prefix + "a.txt", Size: 5}, prefix, dst, bucket.Key, true, nil); err != nil {
			t.Fatalf("overwrite download: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
		if err != nil || string(got) != "hello" {
			t.Errorf("overwritten content = %q, %v", got, err)
		}
		// The atomic swap must not leave its temp file behind.
		if _, err := os.Stat(filepath.Join(dst, "a.txt.s3duck-part")); !os.IsNotExist(err) {
			t.Errorf("temp part file left behind (stat err = %v)", err)
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

	good, err := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 0))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	if _, err := good.ListBuckets(); err != nil {
		t.Fatalf("baseline ListBuckets failed: %v", err)
	}

	bad, err := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "bogus-session-token", true, 0))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
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
	if err := m.Upload(ctx, root, "pre", bucket, nil, nil); err != nil {
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
	if err := m.Upload(ctx, empty, "", bucket, nil, nil); err != nil {
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

func TestIntegrationDuplicateETags(t *testing.T) {
	// The duplicate finder groups by (size, ETag). Its load-bearing assumption:
	// identical single-part uploads share an ETag (it is the body MD5), while
	// different content differs. Assert both against a real endpoint.
	m := testModel(t)
	bucket := freshBucket(t, m, "s3duck-it-dups")

	putObject(t, m, bucket, "a/copy1.bin", "identical duplicate content")
	putObject(t, m, bucket, "b/copy2.bin", "identical duplicate content")
	putObject(t, m, bucket, "c/other.bin", "completely different content!")

	objs, err := m.ListObjects("", bucket)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}

	etags := map[string]string{}
	for _, o := range objs {
		etags[aws.ToString(o.Key)] = aws.ToString(o.ETag)
	}
	if etags["a/copy1.bin"] == "" {
		t.Fatal("endpoint returned no ETags; the duplicate finder cannot work here")
	}
	if etags["a/copy1.bin"] != etags["b/copy2.bin"] {
		t.Errorf("identical uploads have different ETags: %q vs %q — grouping assumption broken",
			etags["a/copy1.bin"], etags["b/copy2.bin"])
	}
	if etags["a/copy1.bin"] == etags["c/other.bin"] {
		t.Errorf("different content shares an ETag: %q — false-positive risk", etags["a/copy1.bin"])
	}
}

func TestIntegrationObjectContent(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-content")
	putObject(t, m, bucket, "cfg/app.yaml", "key: value\n")

	t.Run("round-trip preserves content-type and metadata", func(t *testing.T) {
		if err := m.PutObjectMeta(ctx, bucket, "cfg/app.yaml", model.ObjectMeta{
			ContentType:  "application/yaml",
			UserMetadata: map[string]string{"owner": "vlad"},
		}); err != nil {
			t.Fatalf("PutObjectMeta: %v", err)
		}

		if err := m.PutObjectTags(ctx, bucket, "cfg/app.yaml", map[string]string{"env": "prod"}); err != nil {
			t.Fatalf("PutObjectTags: %v", err)
		}

		content, err := m.GetObjectContent(ctx, bucket, "cfg/app.yaml", 1<<20)
		if err != nil {
			t.Fatalf("GetObjectContent: %v", err)
		}
		if string(content.Data) != "key: value\n" {
			t.Errorf("data = %q", content.Data)
		}
		if content.ContentType != "application/yaml" || content.UserMetadata["owner"] != "vlad" {
			t.Errorf("attributes not carried: %+v", content)
		}
		if content.ETag == "" {
			t.Error("no ETag captured — the edit conflict guard cannot work")
		}
		if content.Tagging != "env=prod" {
			t.Errorf("tags not carried: %q", content.Tagging)
		}

		// The edited write-back must carry every attribute forward.
		if err := m.PutBytes(ctx, bucket, "cfg/app.yaml", []byte("key: edited\n"), content); err != nil {
			t.Fatalf("PutBytes: %v", err)
		}
		after, err := m.HeadObject(ctx, bucket, "cfg/app.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if after.ContentType != "application/yaml" || after.UserMetadata["owner"] != "vlad" {
			t.Errorf("an edit stripped attributes: %+v", after)
		}
		afterTags, err := m.ObjectTags(ctx, bucket, "cfg/app.yaml")
		if err != nil || afterTags["env"] != "prod" {
			t.Errorf("an edit stripped tags: %v, %v", afterTags, err)
		}
		roundTrip, _ := m.GetObjectContent(ctx, bucket, "cfg/app.yaml", 1<<20)
		if string(roundTrip.Data) != "key: edited\n" {
			t.Errorf("edited body = %q", roundTrip.Data)
		}

		// The rewrite changed the body, so the live ETag must differ from the
		// captured one — this is exactly the signal the lost-update guard uses.
		cur, err := m.CurrentETag(ctx, bucket, "cfg/app.yaml")
		if err != nil {
			t.Fatalf("CurrentETag: %v", err)
		}
		if cur == "" || cur == content.ETag {
			t.Errorf("CurrentETag = %q, want a fresh non-empty ETag different from %q", cur, content.ETag)
		}
	})

	t.Run("the size cap refuses oversized objects", func(t *testing.T) {
		putObject(t, m, bucket, "big.bin", strings.Repeat("x", 4096))
		if _, err := m.GetObjectContent(ctx, bucket, "big.bin", 1024); err == nil {
			t.Error("a 4 KiB object must be refused by a 1 KiB cap")
		}
	})
}

func TestIntegrationCrossCopy(t *testing.T) {
	// Two independent Model instances — exactly the cross-profile code path;
	// that they happen to share an endpoint here changes nothing in the code
	// under test, which never lets one client see the other.
	endpoint, region, access, secret := testCreds(t)
	src, err := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 0))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	dst, err := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 0))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}

	ctx := context.Background()
	srcBucket := freshBucket(t, src, "s3duck-it-xsrc")
	dstBucket := freshBucket(t, dst, "s3duck-it-xdst")

	putObject(t, src, srcBucket, "data/report.pdf", "cross-profile payload")
	if err := src.PutObjectMeta(ctx, srcBucket, "data/report.pdf", model.ObjectMeta{
		ContentType:  "application/pdf",
		UserMetadata: map[string]string{"origin": "src-profile"},
	}); err != nil {
		t.Fatalf("seeding metadata: %v", err)
	}
	if err := src.PutObjectTags(ctx, srcBucket, "data/report.pdf", map[string]string{"tier": "gold"}); err != nil {
		t.Fatalf("seeding tags: %v", err)
	}

	var sawProgress bool
	err = model.CrossCopy(ctx, src, srcBucket, "data/report.pdf", dst, dstBucket, "restore/report.pdf",
		func(written, total int64) { sawProgress = written > 0 && total > 0 })
	if err != nil {
		t.Fatalf("CrossCopy: %v", err)
	}
	if !sawProgress {
		t.Error("no progress was reported")
	}

	got, err := dst.GetObjectContent(ctx, dstBucket, "restore/report.pdf", 1<<20)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}
	if string(got.Data) != "cross-profile payload" {
		t.Errorf("copied body = %q", got.Data)
	}
	if got.ContentType != "application/pdf" || got.UserMetadata["origin"] != "src-profile" {
		t.Errorf("attributes did not cross: %+v", got)
	}
	if crossed, err := dst.ObjectTags(ctx, dstBucket, "restore/report.pdf"); err != nil || crossed["tier"] != "gold" {
		t.Errorf("tags did not cross: %v, %v", crossed, err)
	}

	t.Run("the source is untouched", func(t *testing.T) {
		orig, err := src.GetObjectContent(ctx, srcBucket, "data/report.pdf", 1<<20)
		if err != nil || string(orig.Data) != "cross-profile payload" {
			t.Errorf("source changed: %q, %v", orig.Data, err)
		}
	})

	t.Run("a prefix-like destination key is refused", func(t *testing.T) {
		if err := model.CrossCopy(ctx, src, srcBucket, "data/report.pdf", dst, dstBucket, "bad/", nil); err == nil {
			t.Error("want an error for a prefix-like destination")
		}
	})
}

// TestIntegrationMultipartCopy exercises the >5 GiB copy path without moving
// 5 GiB: the thresholds are variables precisely so this test can lower them.
// The proof that the multipart path really ran is the destination's ETag —
// a multipart object's ETag carries a "-<partcount>" suffix, which a single
// CopyObject could never produce.
func TestIntegrationMultipartCopy(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mpcopy")

	threshold, part := model.MultipartCopyThreshold, model.MultipartCopyPartSize
	t.Cleanup(func() {
		model.MultipartCopyThreshold, model.MultipartCopyPartSize = threshold, part
	})
	// 5 MiB is the smallest part S3 accepts, so a 6 MiB source splits into
	// one full part plus a remainder — the smallest real multipart copy.
	model.MultipartCopyThreshold = 5 << 20
	model.MultipartCopyPartSize = 5 << 20

	const size = 6 << 20
	body := strings.Repeat("s3duck!!", size/8)
	putObject(t, m, bucket, "big/source.bin", body)
	if err := m.PutObjectMeta(ctx, bucket, "big/source.bin", model.ObjectMeta{
		ContentType:  "application/octet-stream",
		CacheControl: "max-age=31536000",
		UserMetadata: map[string]string{"origin": "multipart-test"},
		StorageClass: "STANDARD",
		Size:         size,
	}); err != nil {
		t.Fatalf("seeding metadata: %v", err)
	}
	if err := m.PutObjectTags(ctx, bucket, "big/source.bin", map[string]string{"tier": "gold"}); err != nil {
		t.Fatalf("seeding tags: %v", err)
	}

	dstBucket := freshBucket(t, m, "s3duck-it-mpcopy-dst")
	if err := m.CopyObjectSized(ctx, bucket, dstBucket, "big/source.bin", "copied.bin", size); err != nil {
		t.Fatalf("CopyObjectSized over the threshold: %v", err)
	}

	got, err := m.HeadObject(ctx, dstBucket, "copied.bin")
	if err != nil {
		t.Fatalf("HeadObject on the copy: %v", err)
	}
	if got.Size != size {
		t.Errorf("copy is %d bytes, want %d", got.Size, size)
	}
	if !strings.HasSuffix(got.ETag, "-2") {
		t.Errorf("ETag = %q, want a multipart ETag ending in -2; the copy did not go through UploadPartCopy", got.ETag)
	}
	// CreateMultipartUpload starts a bare object: everything a single-request
	// copy would have inherited has to be re-applied, and this is what proves
	// it was.
	if got.ContentType != "application/octet-stream" {
		t.Errorf("content type = %q", got.ContentType)
	}
	if got.CacheControl != "max-age=31536000" {
		t.Errorf("cache control = %q", got.CacheControl)
	}
	if got.UserMetadata["origin"] != "multipart-test" {
		t.Errorf("user metadata = %v", got.UserMetadata)
	}
	if tags, err := m.ObjectTags(ctx, dstBucket, "copied.bin"); err != nil || tags["tier"] != "gold" {
		t.Errorf("tags did not survive the multipart copy: %v, %v", tags, err)
	}

	t.Run("the bytes are identical", func(t *testing.T) {
		content, err := m.GetObjectContent(ctx, dstBucket, "copied.bin", 8<<20)
		if err != nil {
			t.Fatalf("reading the copy: %v", err)
		}
		if string(content.Data) != body {
			t.Errorf("copied body differs from the source (%d vs %d bytes)", len(content.Data), len(body))
		}
	})

	t.Run("a metadata save over the threshold keeps the tags", func(t *testing.T) {
		// The single-request path preserves tags for free (TaggingDirective
		// defaults to COPY even under MetadataDirective=REPLACE); a fresh
		// multipart upload starts with none, so they must be re-applied.
		if err := m.PutObjectMeta(ctx, bucket, "big/source.bin", model.ObjectMeta{
			ContentType:  "text/plain",
			UserMetadata: map[string]string{"origin": "meta-save"},
			StorageClass: "STANDARD",
			Size:         size,
		}); err != nil {
			t.Fatalf("PutObjectMeta over the threshold: %v", err)
		}
		head, err := m.HeadObject(ctx, bucket, "big/source.bin")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(head.ETag, "-2") {
			t.Errorf("ETag = %q, want the metadata save to have taken the multipart path", head.ETag)
		}
		if head.ContentType != "text/plain" || head.UserMetadata["origin"] != "meta-save" {
			t.Errorf("metadata not applied: %+v", head)
		}
		tags, err := m.ObjectTags(ctx, bucket, "big/source.bin")
		if err != nil || tags["tier"] != "gold" {
			t.Errorf("a large metadata save dropped the tags: %v, %v", tags, err)
		}
	})

	t.Run("a size under the threshold still uses the single-request path", func(t *testing.T) {
		putObject(t, m, bucket, "small.txt", "tiny")
		if err := m.CopyObjectSized(ctx, bucket, dstBucket, "small.txt", "small-copy.txt", 4); err != nil {
			t.Fatalf("small copy: %v", err)
		}
		head, err := m.HeadObject(ctx, dstBucket, "small-copy.txt")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(head.ETag, "-") {
			t.Errorf("ETag = %q, want a single-part ETag", head.ETag)
		}
	})

	t.Run("an unknown size is resolved rather than guessed", func(t *testing.T) {
		// CopyObject has no size to go on, so it must HeadObject and still
		// pick the multipart path for the 6 MiB source.
		if err := m.CopyObject(ctx, bucket, dstBucket, "big/source.bin", "unsized.bin"); err != nil {
			t.Fatalf("CopyObject: %v", err)
		}
		head, err := m.HeadObject(ctx, dstBucket, "unsized.bin")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(head.ETag, "-2") {
			t.Errorf("ETag = %q, want the multipart path to have been chosen from the resolved size", head.ETag)
		}
	})
}

// TestIntegrationConflicts covers both branches of the conflict scan — the
// per-key HeadObject probe for a handful of candidates and the single listing
// for a batch — since the two could easily disagree.
// TestIntegrationUploadSkip proves the skip set reaches the wire: the kept
// object must be untouched, and the progress accounting must measure only what
// was actually sent (the walk counts every file, so the totals have to be
// recomputed after filtering or a skipped upload finishes below 100%).
func TestIntegrationUploadSkip(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-upskip")

	root := t.TempDir()
	tree := filepath.Join(root, "proj")
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"keep.txt": "LOCAL new", "send.txt": "LOCAL send"} {
		if err := os.WriteFile(filepath.Join(tree, rel), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	putObject(t, m, bucket, "proj/keep.txt", "REMOTE original, must survive")

	var lastSent, lastTotal int64
	var lastCount int
	skip := map[string]bool{"proj/keep.txt": true}
	if err := m.Upload(ctx, tree, "", bucket, skip, func(n, total int64, i, count int, _, _ string) {
		lastSent, lastTotal, lastCount = n, total, count
	}); err != nil {
		t.Fatalf("Upload with a skip: %v", err)
	}

	kept, err := m.GetObjectContent(ctx, bucket, "proj/keep.txt", 1<<20)
	if err != nil || string(kept.Data) != "REMOTE original, must survive" {
		t.Errorf("skipped object = %q, %v — it was overwritten", kept.Data, err)
	}
	sent, err := m.GetObjectContent(ctx, bucket, "proj/send.txt", 1<<20)
	if err != nil || string(sent.Data) != "LOCAL send" {
		t.Errorf("unskipped object = %q, %v", sent.Data, err)
	}

	if lastCount != 1 {
		t.Errorf("progress counted %d file(s), want only the 1 actually sent", lastCount)
	}
	if lastTotal != int64(len("LOCAL send")) {
		t.Errorf("progress total = %d bytes, want %d — skipped bytes are still in the total",
			lastTotal, len("LOCAL send"))
	}
	if lastSent != lastTotal {
		t.Errorf("progress finished at %d/%d, want a completed transfer", lastSent, lastTotal)
	}
}

func TestIntegrationConflicts(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-conflicts")

	putObject(t, m, bucket, "dst/a.txt", "existing a")
	putObject(t, m, bucket, "dst/c.txt", "existing c")
	putObject(t, m, bucket, "dst/folder/inner.txt", "existing inner")

	t.Run("few keys are probed directly", func(t *testing.T) {
		got, err := m.Conflicts(ctx, bucket, []string{"dst/a.txt", "dst/b.txt", "dst/c.txt"})
		if err != nil {
			t.Fatalf("Conflicts: %v", err)
		}
		if !slices.Equal(got, []string{"dst/a.txt", "dst/c.txt"}) {
			t.Errorf("conflicts = %v, want the two that exist", got)
		}
	})

	t.Run("nothing exists yet", func(t *testing.T) {
		got, err := m.Conflicts(ctx, bucket, []string{"dst/x.txt", "dst/y.txt"})
		if err != nil {
			t.Fatalf("Conflicts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("conflicts = %v, want none", got)
		}
	})

	t.Run("a batch past the probe limit is answered by one listing", func(t *testing.T) {
		// Deliberately over conflictHeadLimit so this takes the listing
		// branch rather than the per-key probes; both must agree.
		var keys []string
		for i := 0; i < 40; i++ {
			keys = append(keys, fmt.Sprintf("dst/gen%02d.txt", i))
		}
		keys = append(keys, "dst/a.txt", "dst/c.txt")
		got, err := m.Conflicts(ctx, bucket, keys)
		if err != nil {
			t.Fatalf("Conflicts: %v", err)
		}
		if !slices.Equal(got, []string{"dst/a.txt", "dst/c.txt"}) {
			t.Errorf("conflicts = %v, want the two that exist", got)
		}
	})

	t.Run("a folder candidate conflicts when anything is under it", func(t *testing.T) {
		got, err := m.Conflicts(ctx, bucket, []string{"dst/folder/", "dst/empty/"})
		if err != nil {
			t.Fatalf("Conflicts: %v", err)
		}
		if !slices.Equal(got, []string{"dst/folder/"}) {
			t.Errorf("conflicts = %v, want only the populated folder", got)
		}
	})

	t.Run("planned keys match what a copy would write", func(t *testing.T) {
		putObject(t, m, bucket, "src/tree/one.txt", "1")
		putObject(t, m, bucket, "src/tree/sub/two.txt", "2")

		planned, err := m.PlannedCopyKeys(bucket, bucket, "src/tree/", "dst/tree/", true)
		if err != nil {
			t.Fatalf("PlannedCopyKeys: %v", err)
		}
		sort.Strings(planned)
		if !slices.Equal(planned, []string{"dst/tree/one.txt", "dst/tree/sub/two.txt"}) {
			t.Errorf("planned = %v", planned)
		}

		// Copy for real and check the plan was the truth.
		if _, err := m.CopyKeys(ctx, bucket, bucket, "src/tree/", "dst/tree/", true, nil, nil); err != nil {
			t.Fatalf("CopyKeys: %v", err)
		}
		after, err := m.Conflicts(ctx, bucket, planned)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(after, planned) {
			t.Errorf("after the copy, %v exist; planned %v", after, planned)
		}
	})

	t.Run("a skipped key is not written and its source survives a move", func(t *testing.T) {
		putObject(t, m, bucket, "mv/keep.txt", "new content")
		putObject(t, m, bucket, "target/keep.txt", "old content, must survive")

		skip := map[string]bool{"target/keep.txt": true}
		n, err := m.MoveKeys(ctx, bucket, bucket, "mv/keep.txt", "target/keep.txt", false, skip, nil)
		if err != nil {
			t.Fatalf("MoveKeys with a skip: %v", err)
		}
		if n != 0 {
			t.Errorf("moved %d object(s), want 0", n)
		}

		dst, err := m.GetObjectContent(ctx, bucket, "target/keep.txt", 1<<20)
		if err != nil || string(dst.Data) != "old content, must survive" {
			t.Errorf("destination = %q, %v — a skipped destination was overwritten", dst.Data, err)
		}
		// A move that skipped its destination must NOT delete the source:
		// nothing was written, so deleting would simply destroy the object.
		src, err := m.GetObjectContent(ctx, bucket, "mv/keep.txt", 1<<20)
		if err != nil || string(src.Data) != "new content" {
			t.Errorf("source = %q, %v — a skipped move deleted its source", src.Data, err)
		}
	})
}
