//go:build integration

package model_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// A profile with checksum_algo must still be able to copy. A multipart upload
// that declares a checksum needs one with every part, which UploadPartCopy
// cannot send, so every copy above the threshold used to fail; the single
// request path must accept the algorithm too.
func TestIntegrationMultipartCopyWithChecksum(t *testing.T) {
	m := testModelWithWrites(t, model.WriteOptions{ChecksumAlgorithm: "CRC32C"})
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mpcopysum")

	threshold, part := model.MultipartCopyThreshold, model.MultipartCopyPartSize
	t.Cleanup(func() {
		model.MultipartCopyThreshold, model.MultipartCopyPartSize = threshold, part
	})
	model.MultipartCopyThreshold = 5 << 20
	model.MultipartCopyPartSize = 5 << 20

	const size = 6 << 20
	putObject(t, m, bucket, "src.bin", strings.Repeat("s3duck!!", size/8))
	if err := m.CopyObjectSized(ctx, bucket, bucket, "src.bin", "dst.bin", size); err != nil {
		t.Fatalf("multipart copy with a checksum algorithm: %v", err)
	}
	if got, err := m.HeadObject(ctx, bucket, "dst.bin"); err != nil || got.Size != size {
		t.Errorf("copy = %+v, %v", got, err)
	}

	putObject(t, m, bucket, "small.txt", "small")
	if err := m.CopyObject(ctx, bucket, bucket, "small.txt", "small-copy.txt"); err != nil {
		t.Fatalf("single-request copy with a checksum algorithm: %v", err)
	}
}

// A copy handed a stale size — from a listing taken before the object was
// rewritten larger — must still copy the whole object. The ranges used to be
// planned from that size, and the completed copy (and the move that deleted
// its source) silently kept only the prefix.
func TestIntegrationMultipartCopyWithAStaleSize(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mpstale")

	threshold, part := model.MultipartCopyThreshold, model.MultipartCopyPartSize
	t.Cleanup(func() {
		model.MultipartCopyThreshold, model.MultipartCopyPartSize = threshold, part
	})
	model.MultipartCopyThreshold = 5 << 20
	model.MultipartCopyPartSize = 5 << 20

	const listed, actual = 6 << 20, 12 << 20
	body := strings.Repeat("0123456789abcdef", actual/16)
	putObject(t, m, bucket, "grew.bin", body)
	if err := m.CopyObjectSized(ctx, bucket, bucket, "grew.bin", "copy.bin", listed); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, err := m.HeadObject(ctx, bucket, "copy.bin")
	if err != nil || got.Size != actual {
		t.Fatalf("copy is %d bytes (%v), want the whole %d", got.Size, err, actual)
	}
	content, err := m.GetObjectContent(ctx, bucket, "copy.bin", actual+1)
	if err != nil || string(content.Data) != body {
		t.Errorf("copied content differs (%v)", err)
	}
}
