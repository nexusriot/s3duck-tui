package controller

import (
	"errors"
	"github.com/rivo/tview"
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
	// Text containing tag-like brackets must render verbatim, not be eaten as
	// markup — asserted through a real TextView, because a string transform
	// can look right and still render wrong (doubling "[" did exactly that).
	for _, in := range []string{"level=[red] msg", "[default]", "[1, 2, 3]", "a[b]c[]", "|ab[red]cd|"} {
		tv := tview.NewTextView().SetDynamicColors(true)
		tv.SetText(escapeForTextView(in))
		if got := strings.TrimRight(tv.GetText(true), "\n"); got != in {
			t.Errorf("%q renders as %q", in, got)
		}
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
	diff := func(a, b, la, lb string) string {
		t.Helper()
		got, err := unifiedDiff(a, b, la, lb)
		if err != nil {
			t.Fatalf("unifiedDiff(%q, %q) errored: %v", a, b, err)
		}
		return got
	}

	// Identical content produces no diff at all, which is what lets the
	// caller say "identical" instead of showing an empty window.
	if got := diff("a\nb\n", "a\nb\n", "old", "new"); got != "" {
		t.Errorf("identical inputs produced a diff: %q", got)
	}

	got := diff("keep\nold\ntail\n", "keep\nnew\ntail\n", "v1", "current")
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
	if got := diff("", "added\n", "v1", "current"); !strings.Contains(got, "+ added") {
		t.Errorf("addition diff = %q", got)
	}
	if got := diff("gone\n", "", "v1", "current"); !strings.Contains(got, "- gone") {
		t.Errorf("removal diff = %q", got)
	}

	// Tags inside content must be escaped, or the viewer would eat them.
	got = diff("[red]\n", "[blue]\n", "v1", "current")
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetText(got)
	if shown := tv.GetText(true); !strings.Contains(shown, "- [red]") || !strings.Contains(shown, "+ [blue]") {
		t.Errorf("content tags not escaped: %q renders as %q", got, shown)
	}
}

// The LCS table is quadratic in LINES, so the byte cap on the bodies bounds
// nothing: two 1 MiB files of short lines would ask for gigabytes. The guard
// has to refuse before it allocates, not after.
func TestUnifiedDiffRefusesAnOversizedTable(t *testing.T) {
	big := strings.Repeat("x\n", 4096) // 4096 lines a side -> ~16.8M cells
	_, err := unifiedDiff(big, big+"y\n", "v1", "current")
	if !errors.Is(err, errDiffTooLarge) {
		t.Fatalf("want errDiffTooLarge, got %v", err)
	}

	// Just under the cap still diffs.
	small := strings.Repeat("x\n", 64)
	if _, err := unifiedDiff(small, small+"y\n", "v1", "current"); err != nil {
		t.Errorf("a small diff was refused: %v", err)
	}
}
