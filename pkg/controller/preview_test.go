package controller

import (
	"strings"
	"testing"
)

func TestHexdumpLayout(t *testing.T) {
	got := hexdump([]byte("ABC"))
	if !strings.HasPrefix(got, "00000000  41 42 43 ") {
		t.Errorf("hexdump = %q, want offset and hex bytes", got)
	}
	if !strings.Contains(got, "|ABC|") {
		t.Errorf("hexdump = %q, want the printable gutter", got)
	}
	// A full line plus one: two rows, second offset 0x10.
	got = hexdump([]byte(strings.Repeat("x", 17)))
	if lines := strings.Split(strings.TrimRight(got, "\n"), "\n"); len(lines) != 2 {
		t.Fatalf("17 bytes produced %d rows, want 2", len(lines))
	}
	if !strings.Contains(got, "00000010  78 ") {
		t.Errorf("second row offset missing: %q", got)
	}
	// Non-printable bytes become dots, never raw control characters.
	got = hexdump([]byte{0x00, 0x1b, 0x7f})
	if strings.ContainsRune(got, 0x1b) {
		t.Error("hexdump leaked an escape byte into the terminal")
	}
	if !strings.Contains(got, "|...|") {
		t.Errorf("control bytes should render as dots: %q", got)
	}
	if hexdump(nil) != "" {
		t.Error("no data should render nothing")
	}
}

func TestEscapeForTextView(t *testing.T) {
	// A log line containing a colour tag must survive verbatim, not be eaten
	// as markup by the viewer.
	if got := escapeForTextView("level=[red] msg"); got != "level=[[red] msg" {
		t.Errorf("escapeForTextView = %q", got)
	}
}

func TestSanitizeText(t *testing.T) {
	got := sanitizeText([]byte("a\tb\nc\x00d\x1b"))
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0) {
		t.Errorf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "a    b") {
		t.Errorf("tab should expand: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("newlines must be preserved: %q", got)
	}
}

func TestPreviewBodyPicksTextOrHex(t *testing.T) {
	body, label := previewBody([]byte("hello\nworld\n"))
	if !strings.Contains(body, "hello") || !strings.HasPrefix(label, "text") {
		t.Errorf("text body = %q / %q", body, label)
	}
	// git's heuristic: a NUL byte means binary.
	body, label = previewBody([]byte{'P', 'K', 0x03, 0x04, 0x00, 0x01})
	if !strings.HasPrefix(label, "binary") {
		t.Errorf("label = %q, want binary", label)
	}
	if !strings.Contains(body, "00000000") {
		t.Errorf("binary body should be a hexdump: %q", body)
	}
}

func TestPreviewTitleReportsTruncation(t *testing.T) {
	size := int64(1 << 20)
	got := previewTitle("k", previewBytes, &size, "text")
	if !strings.Contains(got, "truncated") {
		t.Errorf("a partial read must say so: %q", got)
	}
	small := int64(10)
	if got := previewTitle("k", 10, &small, "text"); strings.Contains(got, "truncated") {
		t.Errorf("a complete read must not claim truncation: %q", got)
	}
	if got := previewTitle("k", 10, nil, "text"); !strings.Contains(got, "unknown size") {
		t.Errorf("unknown size = %q", got)
	}
}

func TestSplitLines(t *testing.T) {
	if got := splitLines("a\nb\n"); len(got) != 2 {
		t.Errorf("trailing newline should not add a line: %v", got)
	}
	if got := splitLines("a\r\nb"); len(got) != 2 || got[0] != "a" {
		t.Errorf("CRLF should normalise: %v", got)
	}
	if splitLines("") != nil {
		t.Error("empty text has no lines")
	}
}

func TestUnifiedDiff(t *testing.T) {
	// Identical content produces no diff at all, which is what lets the
	// caller say "identical" instead of showing an empty window.
	if got := unifiedDiff("a\nb\n", "a\nb\n", "old", "new"); got != "" {
		t.Errorf("identical inputs produced a diff: %q", got)
	}

	got := unifiedDiff("keep\nold\ntail\n", "keep\nnew\ntail\n", "v1", "current")
	if !strings.Contains(got, "- old") || !strings.Contains(got, "+ new") {
		t.Errorf("diff missing the change: %q", got)
	}
	if !strings.Contains(got, "  keep") || !strings.Contains(got, "  tail") {
		t.Errorf("context lines should be shown: %q", got)
	}
	if !strings.Contains(got, "--- v1") || !strings.Contains(got, "+++ current") {
		t.Errorf("diff header missing labels: %q", got)
	}

	// Pure addition and pure removal.
	if got := unifiedDiff("", "added\n", "v1", "current"); !strings.Contains(got, "+ added") {
		t.Errorf("addition diff = %q", got)
	}
	if got := unifiedDiff("gone\n", "", "v1", "current"); !strings.Contains(got, "- gone") {
		t.Errorf("removal diff = %q", got)
	}

	// Tags inside content must be escaped, or the viewer would eat them.
	if got := unifiedDiff("[red]\n", "[blue]\n", "v1", "current"); !strings.Contains(got, "[[red]") {
		t.Errorf("content tags not escaped: %q", got)
	}
}
