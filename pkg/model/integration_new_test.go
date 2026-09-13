//go:build integration

// Integration coverage for the behaviour added alongside the content-type,
// checksum, listing and local-pane work. Same rule as integration_test.go:
// these assert the things no unit test can reach, because they are properties
// of what the server stores rather than of what the client computed.
package model_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// enableVersioning turns versioning on for a bucket, skipping the test when
// the endpoint does not support it.
func enableVersioning(t *testing.T, m *model.Model, bucket *model.Object) {
	t.Helper()
	_, err := m.Client.PutBucketVersioning(context.Background(), &s3.PutBucketVersioningInput{
		Bucket:                  bucket.Key,
		VersioningConfiguration: &s3t.VersioningConfiguration{Status: s3t.BucketVersioningStatusEnabled},
	})
	if err != nil {
		t.Skipf("cannot enable versioning on this endpoint: %v", err)
	}
}

// headInput is the raw HeadObject input for assertions the ObjectMeta wrapper
// does not carry (the encryption header).
func headInput(bucket *model.Object, key string) *s3.HeadObjectInput {
	return &s3.HeadObjectInput{Bucket: bucket.Key, Key: aws.String(key)}
}

// testModelWithWrites builds a model whose profile carries write options, the
// way opening a configured profile would.
func testModelWithWrites(t *testing.T, w model.WriteOptions) *model.Model {
	t.Helper()
	endpoint, region, access, secret := testCreds(t)
	cf := model.NewConfig(endpoint, &region, access, secret, "", true, 0)
	cf.Write = w
	m, err := model.NewModel(cf)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	return m
}

func TestIntegrationUploadSetsContentType(t *testing.T) {
	// The point of the fix: an object's Content-Type is decided at the moment
	// it is created, and only the server can confirm what was stored.
	m := testModelWithWrites(t, model.DefaultWriteOptions())
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mime")

	root := t.TempDir()
	files := map[string]string{
		"index.html": "<!doctype html><p>hi",
		"data.json":  `{"a":1}`,
		"README":     "plain prose, no extension\n",
		"blob.bin":   "\x00\x01\x02binary",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.Upload(ctx, root, "", bucket, nil, nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	base := filepath.Base(root)
	want := map[string]string{
		base + "/index.html": "text/html",
		base + "/data.json":  "application/json",
		base + "/README":     "text/plain", // sniffed, having no extension
	}
	for key, prefix := range want {
		meta, err := m.HeadObject(ctx, bucket, key)
		if err != nil {
			t.Fatalf("HeadObject %s: %v", key, err)
		}
		if !strings.HasPrefix(meta.ContentType, prefix) {
			t.Errorf("%s: Content-Type = %q, want %s...", key, meta.ContentType, prefix)
		}
	}
	// An unrecognisable body with an unknown-to-us extension must not have a
	// type invented for it; the server's own default applies.
	if meta, err := m.HeadObject(ctx, bucket, base+"/blob.bin"); err == nil {
		if strings.HasPrefix(meta.ContentType, "text/") {
			t.Errorf("blob.bin was mislabelled as %q", meta.ContentType)
		}
	}

	// Detection off means nothing is claimed — the escape hatch for buckets
	// whose types are managed elsewhere.
	plain := testModelWithWrites(t, model.WriteOptions{})
	if err := plain.UploadFile(ctx, filepath.Join(root, "index.html"), "raw.html", bucket, nil); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	meta, err := plain.HeadObject(ctx, bucket, "raw.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(meta.ContentType, "text/html") {
		t.Errorf("detection was off but the type was still set: %q", meta.ContentType)
	}
}

func TestIntegrationChecksumOnWriteAndVerify(t *testing.T) {
	// Two claims that only a server can settle: that the checksum we ask for
	// is actually stored, and that a locally computed one matches it — which
	// is what makes the post-transfer verify meaningful for multipart
	// objects, where the ETag cannot be compared at all.
	m := testModelWithWrites(t, model.WriteOptions{DetectMime: true, ChecksumAlgorithm: "CRC32C"})
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-checksum")

	dir := t.TempDir()
	local := filepath.Join(dir, "payload.txt")
	body := strings.Repeat("checksum me\n", 500)
	if err := os.WriteFile(local, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadFile(ctx, local, "payload.txt", bucket, nil); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	ci, err := m.ObjectChecksum(ctx, bucket, "payload.txt")
	if err != nil {
		t.Fatalf("ObjectChecksum: %v", err)
	}
	if ci.Algorithm != "CRC32C" || ci.Value == "" {
		t.Fatalf("stored checksum = %+v, want a CRC32C value", ci)
	}
	if want, err := model.LocalChecksum(local, "CRC32C"); err != nil {
		t.Fatal(err)
	} else if ci.Value != want {
		t.Errorf("server checksum %q != locally computed %q", ci.Value, want)
	}

	res, err := m.VerifyLocalFile(ctx, bucket, "payload.txt", local)
	if err != nil {
		t.Fatalf("VerifyLocalFile: %v", err)
	}
	if !res.Verified || res.Mismatch {
		t.Errorf("verify of an untouched file = %+v, want verified", res)
	}

	// A corrupted local copy must be reported as a mismatch, not as verified.
	if err := os.WriteFile(local, []byte(body+"tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err = m.VerifyLocalFile(ctx, bucket, "payload.txt", local)
	if err != nil {
		t.Fatalf("VerifyLocalFile (tampered): %v", err)
	}
	if !res.Mismatch {
		t.Errorf("a modified file was not detected: %+v", res)
	}
}

func TestIntegrationVerifyFallsBackToETag(t *testing.T) {
	// With no additional checksum, a single-part object is still verifiable
	// through its ETag — the case that covers most of what a TUI downloads.
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-etagverify")

	dir := t.TempDir()
	local := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(local, []byte("small body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadFile(ctx, local, "small.txt", bucket, nil); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	res, err := m.VerifyLocalFile(ctx, bucket, "small.txt", local)
	if err != nil {
		t.Fatalf("VerifyLocalFile: %v", err)
	}
	if !res.Verified || !strings.Contains(res.Algorithm, "ETag") {
		t.Errorf("verify = %+v, want a verified ETag comparison", res)
	}
}

func TestIntegrationRangedPreviewReadsOnlyTheHead(t *testing.T) {
	// The preview's whole economy: one small request whatever the object's
	// size. The proof is that the returned slice is the requested length and
	// holds the file's first bytes.
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-preview")

	body := strings.Repeat("0123456789", 5000) // 50 KB
	putObject(t, m, bucket, "big.txt", body)

	data, ctype, err := m.GetObjectHead(ctx, bucket, "big.txt", 100)
	if err != nil {
		t.Fatalf("GetObjectHead: %v", err)
	}
	if len(data) != 100 {
		t.Errorf("read %d bytes, want exactly the requested 100", len(data))
	}
	if string(data) != body[:100] {
		t.Errorf("head = %q, want the first 100 bytes", data)
	}
	_ = ctype

	// Asking for more than the object holds returns the object, not an error.
	data, _, err = m.GetObjectHead(ctx, bucket, "big.txt", int64(len(body)+1000))
	if err != nil {
		t.Fatalf("GetObjectHead (over-long range): %v", err)
	}
	if len(data) != len(body) {
		t.Errorf("read %d bytes, want the whole %d-byte object", len(data), len(body))
	}

	// A zero-byte object has no satisfiable range at all: the server answers
	// 416, and the preview has to read that as an empty object rather than
	// showing the user an InvalidRange error.
	putObject(t, m, bucket, "empty.txt", "")
	data, _, err = m.GetObjectHead(ctx, bucket, "empty.txt", 100)
	if err != nil {
		t.Fatalf("GetObjectHead on a zero-byte object: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("read %d bytes from a zero-byte object, want 0", len(data))
	}
}

func TestIntegrationVersionContentForDiff(t *testing.T) {
	// The version diff needs the *old* body, which is only reachable with a
	// VersionId — and only on a versioned bucket.
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-vdiff")
	enableVersioning(t, m, bucket)

	putObject(t, m, bucket, "conf.yaml", "a: 1\nb: 2\n")
	putObject(t, m, bucket, "conf.yaml", "a: 1\nb: 3\n")

	versions, err := m.ListVersions(ctx, bucket, "conf.yaml")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) < 2 {
		t.Skipf("endpoint reported %d versions; versioning may be unsupported", len(versions))
	}

	current, err := m.GetVersionContent(ctx, bucket, "conf.yaml", "", 1<<20)
	if err != nil {
		t.Fatalf("GetVersionContent (current): %v", err)
	}
	if string(current) != "a: 1\nb: 3\n" {
		t.Errorf("current body = %q", current)
	}

	// The older version is the one that is not latest.
	var older string
	for _, v := range versions {
		if !v.IsLatest && !v.IsDeleteMark {
			older = v.VersionID
			break
		}
	}
	if older == "" {
		t.Skip("no non-latest version to read")
	}
	old, err := m.GetVersionContent(ctx, bucket, "conf.yaml", older, 1<<20)
	if err != nil {
		t.Fatalf("GetVersionContent (old): %v", err)
	}
	if string(old) != "a: 1\nb: 2\n" {
		t.Errorf("old body = %q, want the first write", old)
	}
}

func TestIntegrationListStreamPagesIncrementally(t *testing.T) {
	// The incremental listing's contract: pages arrive one at a time with a
	// running total, and returning false stops the paging. Asserted against a
	// server because the paginator is the thing being driven.
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-liststream")

	for i := 0; i < 12; i++ {
		putObject(t, m, bucket, fmt.Sprintf("k%02d.txt", i), "x")
	}

	pages, seen := 0, 0
	if err := m.ListObjectsStream(ctx, "", bucket, func(page []s3t.Object, total int) bool {
		pages++
		seen = total
		return true
	}); err != nil {
		t.Fatalf("ListObjectsStream: %v", err)
	}
	if pages == 0 || seen != 12 {
		t.Errorf("pages = %d, total = %d, want at least one page holding 12 keys", pages, seen)
	}

	// Stopping early must not keep paging: with one page requested, exactly
	// one callback happens.
	calls := 0
	if err := m.ListObjectsStream(ctx, "", bucket, func([]s3t.Object, int) bool {
		calls++
		return false
	}); err != nil {
		t.Fatalf("ListObjectsStream (early stop): %v", err)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times after asking to stop, want 1", calls)
	}

	// A cancelled context stops the listing rather than running to the end.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.ListObjectsStream(cancelled, "", bucket, func([]s3t.Object, int) bool { return true }); err == nil {
		t.Error("a cancelled listing should report the cancellation")
	}
}

func TestIntegrationSSEOnWrite(t *testing.T) {
	// SSE-S3 is the one form of server-side encryption MinIO supports without
	// a KMS, so it is what can be asserted here.
	m := testModelWithWrites(t, model.WriteOptions{DetectMime: true, SSE: "AES256"})
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-sse")

	dir := t.TempDir()
	local := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(local, []byte("classified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadFile(ctx, local, "secret.txt", bucket, nil); err != nil {
		t.Skipf("endpoint refused an SSE upload: %v", err)
	}

	out, err := m.Client.HeadObject(ctx, headInput(bucket, "secret.txt"))
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if out.ServerSideEncryption != s3t.ServerSideEncryptionAes256 {
		t.Errorf("ServerSideEncryption = %q, want AES256", out.ServerSideEncryption)
	}
}
