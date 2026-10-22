package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/klauspost/compress/zstd"
	"github.com/mattn/go-runewidth"
)

// The smarter half of the preview: what the head of an object *is*, decided
// from its name, its Content-Type and its first bytes, and rendered the way a
// person would want to read it. Everything here is a pure function of the bytes
// already read — no second request — so it is cheap, testable, and cannot make
// a preview slower than the raw one it replaces.

// previewCompressedBytes is the read window for an object whose name says it
// is compressed. Larger than previewBytes because a compressor's unit of output
// is a block, not a line: zstd blocks run to 128 KiB, and a window that holds
// no whole block decodes to nothing at all.
const previewCompressedBytes = 256 << 10

// previewDecodedMax caps what a compressed head may expand to. A highly
// compressible log grows 10–50×, and the preview is for reading the top of a
// file, not for holding megabytes of it in a TextView.
const previewDecodedMax = 256 << 10

// prettyJSONMax caps the re-indented output. Minified JSON of many small values
// can triple in size once every value gets its own indented line.
const prettyJSONMax = 1 << 20

// csvMaxColWidth and csvMaxRows bound the table view. A free-text column would
// otherwise push every other column off screen, and the head of a large CSV
// holds thousands of rows nobody scrolls through in a preview.
const (
	csvMaxColWidth = 32
	csvMaxRows     = 2000
)

// previewInput is everything the formatter decides from.
type previewInput struct {
	key   string
	data  []byte
	ctype string
	// whole is true when data is the entire object, so a parser that stops
	// early can tell a truncated read from a malformed file.
	whole bool
}

// previewOutput is the rendered preview. raw is the unformatted text of the
// same (decoded) bytes for the overlay's r toggle; empty when nothing was
// reformatted, so there is nothing to toggle to.
type previewOutput struct {
	body  string
	label string
	raw   string
}

// compressionByName reports the codec a key's extension names, if any, and
// the key with that extension removed (".tgz" becomes ".tar"), so the inner
// format can still be recognised by name.
func compressionByName(key string) (codec, inner string) {
	lower := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lower, ".tgz"):
		return "gzip", key[:len(key)-4] + ".tar"
	case strings.HasSuffix(lower, ".gz"):
		return "gzip", key[:len(key)-3]
	case strings.HasSuffix(lower, ".gzip"):
		return "gzip", key[:len(key)-5]
	case strings.HasSuffix(lower, ".zst"):
		return "zstd", key[:len(key)-4]
	case strings.HasSuffix(lower, ".zstd"):
		return "zstd", key[:len(key)-5]
	}
	return "", key
}

// compressionByMagic identifies a compressed stream from its first bytes. The
// magic wins over the name: a ".gz" that is not gzip is previewed as what it
// is, and an extensionless gzip is still decoded.
func compressionByMagic(data []byte) string {
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		return "gzip"
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return "zstd"
	}
	return ""
}

// decompressHead decodes as much of a compressed head as it holds. A head is
// a truncated stream by construction, so the unexpected-EOF every decoder
// reports at the cut is the normal outcome, not an error; only a stream that
// yields nothing at all fails. complete reports that the stream ended cleanly
// within the cap — the decoded bytes are then the whole content.
func decompressHead(codec string, data []byte, max int) (out []byte, complete bool, err error) {
	var r io.Reader
	switch codec {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false, err
		}
		defer zr.Close()
		r = zr
	case "zstd":
		// One goroutine and a bounded window: the defaults decode in the
		// background on every core and accept windows of gigabytes, which is
		// the wrong trade for a 256 KiB look at the top of a file.
		zr, err := zstd.NewReader(bytes.NewReader(data),
			zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(64<<20))
		if err != nil {
			return nil, false, err
		}
		defer zr.Close()
		r = zr
	default:
		return nil, false, fmt.Errorf("unknown codec %q", codec)
	}

	out, rerr := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if len(out) > max {
		return out[:max], false, nil
	}
	if rerr == nil {
		return out, true, nil
	}
	if len(out) == 0 {
		return nil, false, rerr
	}
	return out, false, nil
}

// previewFormat names how decoded bytes are rendered.
type previewFormat int

const (
	formatText previewFormat = iota
	formatJSON
	formatCSV
	formatTSV
	formatTar
)

// detectFormat picks a rendering from the name, the Content-Type and the
// bytes. Order matters: a name is the strongest hint, a Content-Type next (it
// is often the generic application/octet-stream), and sniffing last.
func detectFormat(name, ctype string, data []byte) previewFormat {
	ext := strings.ToLower(path.Ext(name))
	ct := strings.ToLower(ctype)
	switch {
	case ext == ".tar" || isTarHeader(data):
		return formatTar
	case ext == ".csv" || strings.Contains(ct, "text/csv"):
		return formatCSV
	case ext == ".tsv" || ext == ".tab" || strings.Contains(ct, "tab-separated-values"):
		return formatTSV
	// JSON Lines are already one value per line, which is the readable form;
	// pretty-printing them would turn each record into a screenful.
	case ext == ".jsonl" || ext == ".ndjson" || strings.Contains(ct, "ndjson") || strings.Contains(ct, "jsonl"):
		return formatText
	case ext == ".json" || ext == ".geojson" || strings.Contains(ct, "json"):
		return formatJSON
	}
	if ext == "" || ext == ".txt" || ext == ".log" {
		if t := bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), " \t\r\n"); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			// "[2024-01-01 12:00] started" is a log line, not an array:
			// only call it JSON when the strict parser agrees, or when a
			// truncated head at least opens like a JSON document.
			if json.Valid(t) || looksLikeJSONStart(t) {
				return formatJSON
			}
		}
	}
	return formatText
}

// looksLikeJSONStart is the sniff for a head too truncated to validate: an
// object opening with a quoted key, or an array opening with an object, an
// array or a string. A bare number or word after "[" is left alone — that is
// how "[2024-01-01 12:00]" and "[INFO]" log lines start.
func looksLikeJSONStart(t []byte) bool {
	rest := bytes.TrimLeft(t[1:], " \t\r\n")
	if len(rest) == 0 {
		return false
	}
	if t[0] == '{' {
		return rest[0] == '"' || rest[0] == '}'
	}
	return strings.IndexByte(`{["]`, rest[0]) >= 0
}

// isTarHeader reports a POSIX/GNU tar header: "ustar" at offset 257.
func isTarHeader(data []byte) bool {
	return len(data) >= 262 && string(data[257:262]) == "ustar"
}

// renderPreview turns the head of an object into what the overlay shows.
func renderPreview(in previewInput) previewOutput {
	data, name, whole := in.data, in.key, in.whole
	var steps []string

	codec := compressionByMagic(data)
	if _, inner := compressionByName(name); codec != "" {
		name = inner
	}
	if codec != "" {
		decoded, complete, err := decompressHead(codec, data, previewDecodedMax)
		if err != nil {
			body, label := previewBody(data)
			return previewOutput{body: body, label: fmt.Sprintf("%s, undecodable (%v) — %s", codec, err, label)}
		}
		steps = append(steps, fmt.Sprintf("%s → %s", codec, humanize.IBytes(uint64(len(decoded)))))
		data = decoded
		whole = whole && complete
	}

	label := func(what string) string {
		return strings.Join(append(steps, what), " · ")
	}
	raw, rawLabel := previewBody(data)
	format := detectFormat(name, in.ctype, data)

	// A tar header is full of NULs, so the binary sniff would claim every
	// archive; anything else binary is a hexdump whatever its name says.
	if isProbablyBinary(data) && format != formatTar {
		return previewOutput{body: raw, label: label(rawLabel)}
	}

	switch format {
	case formatTar:
		if body, n, ok := tarListing(data, whole); ok {
			return previewOutput{body: body, label: label(fmt.Sprintf("tar listing, %d entr%s", n, pluralYIes(n))), raw: raw}
		}
	case formatJSON:
		if body, strict := prettyJSON(data); body != "" {
			what := "JSON, pretty-printed"
			if !strict {
				what = "JSON, re-indented (head only, not validated)"
			}
			return previewOutput{body: escapeForTextView(sanitizeText([]byte(body))), label: label(what), raw: raw}
		}
	case formatCSV, formatTSV:
		comma, kind := ',', "CSV"
		if format == formatTSV {
			comma, kind = '\t', "TSV"
		}
		if body, rows, ok := csvTable(data, comma, whole); ok {
			return previewOutput{body: body, label: label(fmt.Sprintf("%s table, %d row(s)", kind, rows)), raw: raw}
		}
	}
	if len(steps) > 0 {
		return previewOutput{body: raw, label: label(rawLabel)}
	}
	return previewOutput{body: raw, label: rawLabel}
}

// prettyJSON indents JSON for reading. A complete document goes through the
// standard library and is reported strict; a truncated head — the usual case
// for anything over 64 KiB — cannot be parsed, so it is re-indented token by
// token instead, which needs no closing brackets to have arrived.
func prettyJSON(data []byte) (string, bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if json.Valid(data) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, data, "", "  "); err == nil && buf.Len() <= prettyJSONMax {
			return buf.String(), true
		}
	}
	return reindentJSON(data, prettyJSONMax), false
}

// reindentJSON lays JSON out one value per line without validating it. Strings
// are copied verbatim (escapes included), whitespace between tokens is dropped
// and re-made, and an empty {} or [] stays on one line. Several top-level
// values in a row are put on lines of their own.
func reindentJSON(data []byte, max int) string {
	var b strings.Builder
	depth := 0
	inString, escaped := false, false
	newline := func() {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", depth))
	}
	// nextToken returns the first non-whitespace byte after i, or 0.
	nextToken := func(i int) byte {
		for j := i + 1; j < len(data); j++ {
			switch data[j] {
			case ' ', '\t', '\r', '\n':
				continue
			}
			return data[j]
		}
		return 0
	}

	for i := 0; i < len(data); i++ {
		if b.Len() > max {
			b.WriteString("\n…")
			break
		}
		ch := data[i]
		if inString {
			b.WriteByte(ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case ' ', '\t', '\r', '\n':
			// Insignificant outside strings; layout is rebuilt below.
		case '"':
			inString = true
			b.WriteByte(ch)
		case '{', '[':
			b.WriteByte(ch)
			if next := nextToken(i); (ch == '{' && next == '}') || (ch == '[' && next == ']') {
				// Empty container: emit the close now and skip to it.
				for i++; i < len(data) && data[i] != next; i++ {
				}
				b.WriteByte(next)
				if depth == 0 && nextToken(i) != 0 {
					b.WriteByte('\n')
				}
				continue
			}
			depth++
			newline()
		case '}', ']':
			if depth > 0 {
				depth--
			}
			newline()
			b.WriteByte(ch)
			if depth == 0 && nextToken(i) != 0 && nextToken(i) != ',' {
				b.WriteByte('\n')
			}
		case ',':
			b.WriteByte(ch)
			newline()
		case ':':
			b.WriteString(": ")
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// csvCell makes one field safe for a single table row: embedded newlines (legal
// inside a quoted CSV field) are shown as a return glyph, and control
// characters as dots, so a cell can never break the row it sits in.
func csvCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "↵")
	s = strings.ReplaceAll(s, "\n", "↵")
	s = strings.ReplaceAll(s, "\r", "↵")
	return strings.TrimRight(sanitizeText([]byte(s)), "\n")
}

// csvTable renders delimited text as aligned columns, the first row as the
// header. A read that stopped mid-file almost certainly cut its last record in
// half, so that record is dropped unless the data ends on a line break. ok is
// false when the data does not parse as delimited text at all.
func csvTable(data []byte, comma rune, whole bool) (body string, rows int, ok bool) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.Comma = comma
	r.LazyQuotes = true
	r.FieldsPerRecord = -1

	var records [][]string
	reachedEnd := false
	for len(records) < csvMaxRows+1 {
		rec, err := r.Read()
		if err != nil {
			// io.EOF, or a quote the cut left open, which makes the rest
			// unparseable; what was read before it is still a table.
			reachedEnd = true
			break
		}
		records = append(records, rec)
	}
	// Only the record that ran into the end of the read can be a fragment; one
	// the row cap stopped at is followed by more data.
	if reachedEnd && !whole && len(records) > 1 && !bytes.HasSuffix(data, []byte("\n")) {
		records = records[:len(records)-1]
	}
	if len(records) == 0 {
		return "", 0, false
	}
	// A single column is not a table — it is text the delimiter never split,
	// and boxing it would only add noise.
	maxCols := 0
	for _, rec := range records {
		if len(rec) > maxCols {
			maxCols = len(rec)
		}
	}
	if maxCols < 2 {
		return "", 0, false
	}

	shown := records
	if len(shown) > csvMaxRows {
		shown = shown[:csvMaxRows]
	}
	widths := make([]int, maxCols)
	cells := make([][]string, len(shown))
	for i, rec := range shown {
		cells[i] = make([]string, len(rec))
		for j, f := range rec {
			c := truncateDisplay(csvCell(f), csvMaxColWidth)
			cells[i][j] = c
			if w := runewidth.StringWidth(c); w > widths[j] {
				widths[j] = w
			}
		}
	}

	var b strings.Builder
	sep := " [gray]│[-] "
	for i, row := range cells {
		for j := 0; j < maxCols; j++ {
			if j > 0 {
				b.WriteString(sep)
			}
			c := ""
			if j < len(row) {
				c = row[j]
			}
			pad := widths[j] - runewidth.StringWidth(c)
			c = escapeForTextView(c)
			if i == 0 {
				c = "[yellow::b]" + c + "[-::-]"
			}
			b.WriteString(c)
			if j < maxCols-1 && pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteByte('\n')
		if i == 0 {
			for j, w := range widths {
				if j > 0 {
					b.WriteString("[gray]─┼─[-]")
				}
				b.WriteString("[gray]" + strings.Repeat("─", w) + "[-]")
			}
			b.WriteByte('\n')
		}
	}
	if len(records) > csvMaxRows {
		fmt.Fprintf(&b, "[gray]… more rows than the preview shows (%d)[-]\n", csvMaxRows)
	}
	// The header is not a data row.
	return b.String(), len(shown) - 1, true
}

// tarListing lists the entries in the head of a tar stream: type, size, date
// and name, like `tar tv`. A head ends mid-archive, so the listing stops at the
// first entry whose header or body the read did not reach — said in the last
// line, so a short listing is not mistaken for a small archive.
func tarListing(data []byte, whole bool) (body string, entries int, ok bool) {
	tr := tar.NewReader(bytes.NewReader(data))
	var b strings.Builder
	var stopErr error
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				stopErr = err
			}
			break
		}
		entries++
		kind := "-"
		switch h.Typeflag {
		case tar.TypeDir:
			kind = "d"
		case tar.TypeSymlink:
			kind = "l"
		case tar.TypeLink:
			kind = "h"
		}
		name := h.Name
		if h.Linkname != "" {
			name += " -> " + h.Linkname
		}
		fmt.Fprintf(&b, "%s %10s  %s  %s\n", kind, humanize.IBytes(uint64(h.Size)),
			h.ModTime.Local().Format(colDateLayout), escapeForTextView(csvCell(name)))
	}
	if entries == 0 {
		return "", 0, false
	}
	if stopErr != nil || !whole {
		b.WriteString("[gray]… the listing stops where the preview's read ended[-]\n")
	}
	return b.String(), entries, true
}
