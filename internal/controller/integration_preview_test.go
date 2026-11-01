//go:build integration

package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// previewOf reads an object exactly as Preview does — same window, same
// whole-object test — and renders it.
func previewOf(t *testing.T, m *model.Model, b *model.Object, key string, size int64) previewOutput {
	t.Helper()
	window := int64(previewBytes)
	if codec, _ := compressionByName(key); codec != "" {
		window = previewCompressedBytes
	}
	data, ctype, err := m.GetObjectHead(context.Background(), b, key, window)
	if err != nil {
		t.Fatalf("GetObjectHead(%s): %v", key, err)
	}
	return renderPreview(previewInput{key: key, data: data, ctype: ctype, whole: int64(len(data)) >= size})
}

func gzipBytes(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestIntegrationPreviewCornerCases(t *testing.T) {
	m := itModel(t)
	b := itBucket(t, m, "s3duck-it-preview-corner")

	// A zero-byte object has no satisfiable range at all, so S3 answers the
	// preview's ranged GET with 416. That must read as an empty file, not as
	// an error — and not as a hexdump of nothing.
	t.Run("zero-byte object", func(t *testing.T) {
		itPut(t, m, b, "empty.txt", "")
		out := previewOf(t, m, b, "empty.txt", 0)
		if strings.Contains(out.label, "hexdump") {
			t.Errorf("an empty object should not be binary: %q", out.label)
		}
		if strings.TrimSpace(rendered(out.body)) != "" {
			t.Errorf("body = %q, want nothing", out.body)
		}
	})

	// The head of a gzip larger than the window: the decoder meets the cut
	// mid-stream, which is its normal case here, and must keep what decoded.
	t.Run("gzip larger than the window", func(t *testing.T) {
		var plain strings.Builder
		for i := 0; i < 60000; i++ {
			fmt.Fprintf(&plain, "2026-10-22T10:%02d:%02d level=info req=%08x msg=\"served\"\n", i/60%60, i%60, i*2654435761)
		}
		gz := gzipBytes(t, plain.String())
		if len(gz) <= previewCompressedBytes {
			t.Fatalf("fixture compressed to %d bytes, not larger than the %d window", len(gz), previewCompressedBytes)
		}
		itPut(t, m, b, "big.log.gz", gz)

		out := previewOf(t, m, b, "big.log.gz", int64(len(gz)))
		if !strings.HasPrefix(out.label, "gzip → ") {
			t.Fatalf("label = %q, want the decode step named", out.label)
		}
		got := rendered(out.body)
		if !strings.HasPrefix(got, plain.String()[:120]) {
			t.Errorf("decoded head does not start like the original:\n%.160q", got)
		}
		if len(got) < 10000 {
			t.Errorf("only %d bytes decoded from a %d-byte window; the head should yield far more", len(got), previewCompressedBytes)
		}
	})

	// Minified JSON past the window cannot be parsed, so it takes the
	// token-level re-indenter — the path no complete document ever reaches.
	t.Run("JSON larger than the window", func(t *testing.T) {
		var doc strings.Builder
		doc.WriteString(`[`)
		for i := 0; i < 4000; i++ {
			if i > 0 {
				doc.WriteString(",")
			}
			fmt.Fprintf(&doc, `{"id":%d,"name":"row-%d","note":"a, \"quoted\" value"}`, i, i)
		}
		doc.WriteString(`]`)
		if doc.Len() <= previewBytes {
			t.Fatalf("fixture is %d bytes, not larger than the %d window", doc.Len(), previewBytes)
		}
		itPut(t, m, b, "big.json", doc.String())

		out := previewOf(t, m, b, "big.json", int64(doc.Len()))
		if !strings.Contains(out.label, "not validated") {
			t.Errorf("label = %q, want the truncated-head wording", out.label)
		}
		got := rendered(out.body)
		if !strings.Contains(got, "\n    \"id\": 0,") {
			t.Errorf("not re-indented:\n%.300q", got)
		}
		if !strings.Contains(got, `"note": "a, \"quoted\" value"`) {
			t.Errorf("escapes inside a string were not copied verbatim:\n%.300q", got)
		}
	})

	// Real CSV, with the two things that break a naive table: a quoted field
	// holding the delimiter and a newline, and double-width runes.
	t.Run("CSV with quoted newlines and wide runes", func(t *testing.T) {
		itPut(t, m, b, "people.csv", "id,name,city\n1,\"Ann\nB\",\"Paris, FR\"\n2,日本語テスト,Oslo\n3,🦆duck,Rome\n")
		out := previewOf(t, m, b, "people.csv", -1)
		if !strings.Contains(out.label, "CSV table") {
			t.Fatalf("label = %q", out.label)
		}
		got := rendered(out.body)
		if !strings.Contains(got, "Ann↵B") {
			t.Errorf("an embedded newline must be shown, not obeyed:\n%s", got)
		}
		if !strings.Contains(got, "Paris, FR") {
			t.Errorf("a quoted delimiter must stay in its cell:\n%s", got)
		}
		lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
		if len(lines) != 5 { // header, rule, three rows
			t.Fatalf("got %d lines, want 5:\n%s", len(lines), got)
		}
		// Every row's second separator must sit in the same column, or the
		// wide runes have spilled past their cell.
		col := func(s string) int { return strings.Index(s, "│") }
		for i, l := range lines[2:] {
			if col(l) != col(lines[0]) {
				t.Errorf("row %d is misaligned (%d vs %d):\n%s", i, col(l), col(lines[0]), got)
			}
		}
	})

	// The name says JSON and the bytes say otherwise. The bytes win.
	t.Run("name lies about the content", func(t *testing.T) {
		itPut(t, m, b, "actually-binary.json", "\x00\x01\x02PK\x03\x04binary")
		out := previewOf(t, m, b, "actually-binary.json", -1)
		if !strings.Contains(out.label, "hexdump") {
			t.Errorf("label = %q, want a hexdump", out.label)
		}
	})

	// A log line that opens with a bracket is not a JSON array.
	t.Run("bracketed log lines are not JSON", func(t *testing.T) {
		itPut(t, m, b, "app.log", "[2026-10-22 10:00:00] started\n[INFO] ready\n")
		out := previewOf(t, m, b, "app.log", -1)
		if strings.Contains(out.label, "JSON") {
			t.Errorf("label = %q, want plain text", out.label)
		}
	})
}
