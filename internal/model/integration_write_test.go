//go:build integration

package model_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// headInput is the raw HeadObject input for assertions the ObjectMeta wrapper
// does not carry (the encryption header).
func headInput(bucket *model.Object, key string) *s3.HeadObjectInput {
	return &s3.HeadObjectInput{Bucket: bucket.Key, Key: aws.String(key)}
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

// UploadTargets sends exactly the list it is given: no walk of any directory.
// The local pane's upload used to hand the model the directory its one marked
// file sat in, and the model uploaded everything in it.
func TestIntegrationUploadTargetsSendsExactlyTheList(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-targets")

	dir := t.TempDir()
	for name, body := range map[string]string{"a.txt": "send me", "b.txt": "leave me"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0700); err != nil {
		t.Fatal(err)
	}

	targets := []model.UploadTarget{
		{LocalPath: filepath.Join(dir, "a.txt"), RemotePath: "pre/a.txt", Size: 7},
		{LocalPath: filepath.Join(dir, "empty"), RemotePath: "pre/empty/"},
	}
	if err := m.UploadTargets(ctx, bucket, targets, nil); err != nil {
		t.Fatalf("UploadTargets: %v", err)
	}

	objs, err := m.ListObjects(ctx, "", bucket)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range objs {
		got[*o.Key] = true
	}
	if len(got) != 2 || !got["pre/a.txt"] || !got["pre/empty/"] {
		t.Errorf("bucket holds %v, want exactly pre/a.txt and the pre/empty/ marker", got)
	}
}
