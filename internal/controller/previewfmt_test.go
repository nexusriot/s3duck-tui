package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/rivo/tview"
)

// rendered is what a TextView would actually show for a body: tags resolved,
// escapes removed. Assertions go through it so an escaping mistake cannot
// hide behind a string comparison.
func rendered(body string) string {
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetText(body)
	return tv.GetText(true)
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstded(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

// logLines is incompressible enough to need several deflate blocks.
func logLines(n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "2026-10-22T10:%02d:%02d level=info req=%08x msg=\"request served\"\n", i/60%60, i%60, i*2654435761)
	}
	return b.Bytes()
}

func TestPreviewDecodesATruncatedGzipHead(t *testing.T) {
	plain := logLines(20000)
	z := gzipped(t, plain)
	if len(z) < 40<<10 {
		t.Fatalf("fixture too small to truncate meaningfully: %d bytes", len(z))
	}
	head := z[:32<<10] // what a ranged GET of a big .gz returns

	out := renderPreview(previewInput{key: "logs/app.log.gz", data: head})
	if !strings.HasPrefix(out.label, "gzip → ") {
		t.Errorf("label = %q, want the decode step named", out.label)
	}
	got := rendered(out.body)
	if !strings.HasPrefix(got, string(plain[:200])) {
		t.Errorf("decoded text does not start like the original:\n%q", got[:200])
	}
}

func TestPreviewDecodesZstdAndRecognisesTheInnerFormat(t *testing.T) {
	doc := []byte(`{"name":"duck","tags":["a","b"],"nested":{"ok":true,"empty":{}}}`)
	out := renderPreview(previewInput{key: "data/doc.json.zst", data: zstded(t, doc), whole: true})
	if !strings.Contains(out.label, "zstd → ") || !strings.Contains(out.label, "JSON, pretty-printed") {
		t.Errorf("label = %q, want zstd decode then strict JSON", out.label)
	}
	got := rendered(out.body)
	if !strings.Contains(got, "\n  \"tags\": [\n    \"a\",") {
		t.Errorf("not indented:\n%s", got)
	}
	if !strings.Contains(got, `"empty": {}`) {
		t.Errorf("an empty object should stay on one line:\n%s", got)
	}
	if out.raw == "" {
		t.Error("a reformatted preview must offer the raw text")
	}
}

func TestPreviewTrustsMagicOverName(t *testing.T) {
	// No extension at all, but gzip bytes: decoded.
	out := renderPreview(previewInput{key: "dump", data: gzipped(t, []byte("hello\n")), whole: true})
	if !strings.HasPrefix(out.label, "gzip") || !strings.Contains(rendered(out.body), "hello") {
		t.Errorf("extensionless gzip not decoded: %q / %q", out.label, out.body)
	}
	// A ".gz" that is really text: shown as text, not reported undecodable.
	out = renderPreview(previewInput{key: "notes.gz", data: []byte("just text\n"), whole: true})
	if strings.Contains(out.label, "gzip") || !strings.Contains(rendered(out.body), "just text") {
		t.Errorf("mislabelled .gz should preview as what it is: %q", out.label)
	}
}

func TestPreviewReportsAGzipItCannotDecode(t *testing.T) {
	bad := append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte{0xff}, 64)...)
	out := renderPreview(previewInput{key: "x.gz", data: bad, whole: true})
	if !strings.Contains(out.label, "undecodable") {
		t.Errorf("label = %q, want the failure said", out.label)
	}
}

func TestReindentJSONHandlesATruncatedHead(t *testing.T) {
	head := []byte(`{"items":[{"id":1,"note":"a, \"quoted\" {brace}"},{"id":2,"v":[1,2`)
	out, strict := prettyJSON(head)
	if strict {
		t.Fatal("a truncated document cannot be strict")
	}
	want := "{\n  \"items\": [\n    {\n      \"id\": 1,\n      \"note\": \"a, \\\"quoted\\\" {brace}\"\n    },\n    {\n      \"id\": 2,\n      \"v\": [\n        1,\n        2"
	if out != want {
		t.Errorf("reindent:\n%s\nwant:\n%s", out, want)
	}
}

func TestReindentJSONSeparatesTopLevelValues(t *testing.T) {
	out := reindentJSON([]byte(`{"a":1}{"b":[]} [3]`), prettyJSONMax)
	if strings.Count(out, "\n{") != 1 || !strings.Contains(out, "\"b\": []") || !strings.HasSuffix(out, "]") {
		t.Errorf("top-level values run together:\n%s", out)
	}
}

func TestDetectFormatSniffing(t *testing.T) {
	cases := []struct {
		name, ctype, data string
		want              previewFormat
	}{
		{"a.json", "", "not even json", formatJSON},
		{"blob", "application/json", "{}", formatJSON},
		{"events.jsonl", "", `{"a":1}`, formatText},
		{"noext", "", ` {"a": 1, "b": `, formatJSON},
		{"noext", "", `["x", "y"`, formatJSON},
		// Log lines that open with a bracket are not arrays.
		{"app.log", "", "[2024-01-01 12:00] started", formatText},
		{"app.log", "", "[INFO] started", formatText},
		{"t.csv", "", "a,b\n", formatCSV},
		{"t", "text/csv; charset=utf-8", "a,b\n", formatCSV},
		{"t.tsv", "", "a\tb\n", formatTSV},
		{"x.tar", "", "", formatTar},
		{"readme.md", "", "{\"a\":1}", formatText},
	}
	for _, c := range cases {
		if got := detectFormat(c.name, c.ctype, []byte(c.data)); got != c.want {
			t.Errorf("detectFormat(%q, %q, %q) = %v, want %v", c.name, c.ctype, c.data, got, c.want)
		}
	}
}

func TestCSVTableAlignsAndMarksTheHeader(t *testing.T) {
	data := []byte("id,name,city\n1,Ann,\"Paris, FR\"\n22,Bartholomew,Oslo\n")
	body, rows, ok := csvTable(data, ',', true)
	if !ok || rows != 2 {
		t.Fatalf("ok=%v rows=%d, want a 2-row table", ok, rows)
	}
	if !strings.Contains(body, "[yellow::b]") {
		t.Error("the header row should be highlighted")
	}
	lines := strings.Split(strings.TrimRight(rendered(body), "\n"), "\n")
	if len(lines) != 4 { // header, rule, two rows
		t.Fatalf("got %d lines:\n%s", len(lines), rendered(body))
	}
	// Columns line up: the second separator sits at the same cell in every row.
	col := func(s string) int { return strings.Index(s, "│") }
	if col(lines[0]) != col(lines[2]) || col(lines[2]) != col(lines[3]) {
		t.Errorf("columns are not aligned:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[2], "Paris, FR") {
		t.Errorf("a quoted comma must stay inside its cell: %q", lines[2])
	}
}

func TestCSVTableDropsTheRowTheReadCut(t *testing.T) {
	data := []byte("a,b\n1,2\n3,4\n5,")
	_, rows, ok := csvTable(data, ',', false)
	if !ok || rows != 2 {
		t.Errorf("rows=%d ok=%v, want the cut row dropped (2 rows)", rows, ok)
	}
	// The whole object really does end that way: keep it.
	if _, rows, _ = csvTable(data, ',', true); rows != 3 {
		t.Errorf("rows=%d for a whole file, want 3", rows)
	}
}

func TestCSVTableRefusesSingleColumnText(t *testing.T) {
	if _, _, ok := csvTable([]byte("just\nlines\nof text\n"), ',', true); ok {
		t.Error("text with no delimiter is not a table")
	}
}

func TestCSVCellCannotBreakItsRow(t *testing.T) {
	body, _, ok := csvTable([]byte("a,b\n\"line1\nline2\",x\n"), ',', true)
	if !ok {
		t.Fatal("not parsed")
	}
	if got := rendered(body); !strings.Contains(got, "line1↵line2") {
		t.Errorf("embedded newline should be shown, not obeyed:\n%s", got)
	}
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := []string{"docs/", "docs/a.txt", "docs/b.txt"}
	for _, name := range names {
		body, ok := files[name]
		if !ok {
			continue
		}
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1700000000, 0)}
		if strings.HasSuffix(name, "/") {
			h.Typeflag = tar.TypeDir
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPreviewListsATarGz(t *testing.T) {
	tb := tarball(t, map[string]string{"docs/": "", "docs/a.txt": "alpha", "docs/b.txt": strings.Repeat("b", 3000)})
	out := renderPreview(previewInput{key: "backup.tgz", data: gzipped(t, tb), whole: true})
	if !strings.Contains(out.label, "tar listing, 3 entries") {
		t.Fatalf("label = %q", out.label)
	}
	got := rendered(out.body)
	for _, want := range []string{"d ", "docs/a.txt", "2.9 KiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stops where") {
		t.Error("a whole archive must not claim to be cut short")
	}
}

func TestTarListingOfATruncatedHeadSaysSo(t *testing.T) {
	tb := tarball(t, map[string]string{"docs/a.txt": strings.Repeat("a", 2000), "docs/b.txt": "b"})
	body, n, ok := tarListing(tb[:1024], false)
	if !ok || n != 1 {
		t.Fatalf("ok=%v n=%d, want the first entry only", ok, n)
	}
	if !strings.Contains(rendered(body), "stops where the preview's read ended") {
		t.Errorf("a cut listing should say so:\n%s", rendered(body))
	}
}

func TestPlainTextHasNothingToToggle(t *testing.T) {
	out := renderPreview(previewInput{key: "readme.txt", data: []byte("hello\n"), whole: true})
	if out.raw != "" {
		t.Error("an unformatted preview has no raw view to switch to")
	}
	if out.label != "text — first 6 B" {
		t.Errorf("label = %q, want the plain text label unchanged", out.label)
	}
}

func TestBinaryWithATextNameIsStillAHexdump(t *testing.T) {
	out := renderPreview(previewInput{key: "data.json", data: []byte{0x00, 0x01, 'P', 'K'}, whole: true})
	if !strings.Contains(out.label, "hexdump") {
		t.Errorf("label = %q, want a hexdump for binary bytes", out.label)
	}
}
