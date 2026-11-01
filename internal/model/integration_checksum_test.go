//go:build integration

package model_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

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

// A multipart object's stored checksum is a checksum-of-parts, and
// GetObjectAttributes reports it without the "-N" suffix, so it must be
// recognised from the ETag. Reading it as a whole-object checksum reported
// every intact multipart download as a MISMATCH.
func TestIntegrationMultipartChecksumIsNotAMismatch(t *testing.T) {
	m := testModelWithWrites(t, model.WriteOptions{ChecksumAlgorithm: "CRC32C"})
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mpsum")

	local := filepath.Join(t.TempDir(), "big.bin")
	// Over the uploader's 5 MiB part size, so the upload goes multipart.
	if err := os.WriteFile(local, []byte(strings.Repeat("multipart", (11<<20)/9)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadFile(ctx, local, "big.bin", bucket, nil); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	res, err := m.VerifyLocalFile(ctx, bucket, "big.bin", local)
	if err != nil {
		t.Fatalf("VerifyLocalFile: %v", err)
	}
	if res.Mismatch {
		t.Errorf("an untouched multipart upload verified as a mismatch: %s", res)
	}
}
