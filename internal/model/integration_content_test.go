//go:build integration

package model_test

import (
	"context"
	"strings"
	"testing"
)

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
