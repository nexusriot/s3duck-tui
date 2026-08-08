package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

func colObj(name string, ot model.FType) *model.Object {
	n := name
	return &model.Object{Key: &n, Ot: ot}
}

func colFile(name string, size int64, when *time.Time, class string) *model.Object {
	o := colObj(name, model.File)
	o.Size = &size
	o.LastModified = when
	if class != "" {
		c := class
		o.StorageClass = &c
	}
	return o
}

func colTime(t *testing.T) *time.Time {
	t.Helper()
	v := time.Date(2026, 7, 30, 14, 5, 0, 0, time.Local)
	return &v
}

func TestPlanColumns(t *testing.T) {
	t.Run("a wide pane fits every column", func(t *testing.T) {
		cols := planColumns(120)
		if !cols.size || !cols.date || !cols.class {
			t.Errorf("got %+v, want all columns", cols)
		}
		if cols.nameWidth < colMinName {
			t.Errorf("nameWidth = %d, want at least %d", cols.nameWidth, colMinName)
		}
	})

	t.Run("columns drop in priority order as the pane narrows", func(t *testing.T) {
		// Size is the most useful (it is what the sort keys order by) and must be
		// the last to go; class is the least and goes first.
		var lastDropped []string
		prev := planColumns(200)
		for w := 199; w >= 20; w-- {
			cur := planColumns(w)
			if prev.class && !cur.class {
				lastDropped = append(lastDropped, "class")
			}
			if prev.date && !cur.date {
				lastDropped = append(lastDropped, "date")
			}
			if prev.size && !cur.size {
				lastDropped = append(lastDropped, "size")
			}
			prev = cur
		}
		want := []string{"class", "date", "size"}
		if strings.Join(lastDropped, ",") != strings.Join(want, ",") {
			t.Errorf("drop order = %v, want %v", lastDropped, want)
		}
	})

	t.Run("the name never falls below the readable minimum while columns remain", func(t *testing.T) {
		for w := 20; w <= 200; w++ {
			cols := planColumns(w)
			if (cols.size || cols.date || cols.class) && cols.nameWidth < colMinName {
				t.Fatalf("width %d: nameWidth %d below minimum with columns %+v", w, cols.nameWidth, cols)
			}
		}
	})

	t.Run("a very narrow pane keeps only the name", func(t *testing.T) {
		cols := planColumns(24)
		if cols.size || cols.date || cols.class {
			t.Errorf("got %+v, want name only", cols)
		}
		if cols.nameWidth < 1 {
			t.Errorf("nameWidth = %d, want at least 1", cols.nameWidth)
		}
	})

	t.Run("an unmeasured widget falls back instead of collapsing", func(t *testing.T) {
		if planColumns(0) != planColumns(colFallbackWidth) {
			t.Error("width 0 should use the fallback width")
		}
		if planColumns(-5) != planColumns(colFallbackWidth) {
			t.Error("a negative width should use the fallback width")
		}
	})
}

func TestListRowAlignment(t *testing.T) {
	cols := planColumns(120)
	when := colTime(t)

	// Names deliberately contain no "-", so the dash a folder shows for size is
	// unambiguous when located below.
	rows := []struct {
		row  string
		size string
	}{
		{listRow(colFile("a.txt", 10, when, ""), false, cols), "10 B"},
		{listRow(colFile("archive.tar.gz", 1<<30, when, "GLACIER"), false, cols), "1.0 GiB"},
		{listRow(colObj("photos", model.Folder), false, cols), "-"},
		{listRow(colFile("selected.bin", 2048, when, ""), true, cols), "2.0 KiB"},
	}

	// The size column is right-aligned, so for every row its last cell must be
	// the same — that is what makes the magnitudes readable down the column.
	want := colIconWidth + cols.nameWidth + colGap + colSizeWidth
	for i, r := range rows {
		got := fieldEndCell(stripTags(r.row), r.size)
		if got != want {
			t.Errorf("row %d: size field ends at cell %d, want %d\n  %q", i, got, want, stripTags(r.row))
		}
	}
}

// stripTags removes tview colour tags so a test can reason about visible text.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fieldEndCell returns the display cell at which the first occurrence of field
// ends within plain (a row with colour tags already stripped). Measured in
// cells rather than bytes because the row starts with a double-width emoji.
func fieldEndCell(plain, field string) int {
	i := strings.Index(plain, field)
	if i < 0 {
		return -1
	}
	return runewidth.StringWidth(plain[:i+len(field)])
}

func TestListRowContent(t *testing.T) {
	cols := planColumns(120)
	when := colTime(t)

	t.Run("a file shows its size, date and non-standard class", func(t *testing.T) {
		row := stripTags(listRow(colFile("report.pdf", 2048, when, "GLACIER"), false, cols))
		for _, want := range []string{"report.pdf", "2.0 KiB", "2026-07-30 14:05", "GLACIER"} {
			if !strings.Contains(row, want) {
				t.Errorf("row missing %q: %q", want, row)
			}
		}
	})

	t.Run("STANDARD is left blank so exceptions stand out", func(t *testing.T) {
		row := stripTags(listRow(colFile("a.txt", 1, when, "STANDARD"), false, cols))
		if strings.Contains(row, "STANDARD") {
			t.Errorf("the default class should not be printed: %q", row)
		}
		// A lowercase spelling from an S3-compatible backend must be caught too.
		row = stripTags(listRow(colFile("a.txt", 1, when, "standard"), false, cols))
		if strings.Contains(strings.ToUpper(row), "STANDARD") {
			t.Errorf("class comparison should be case-insensitive: %q", row)
		}
	})

	t.Run("folders show a trailing slash and no size", func(t *testing.T) {
		row := stripTags(listRow(colObj("photos", model.Folder), false, cols))
		if !strings.Contains(row, "photos/") {
			t.Errorf("row = %q", row)
		}
		if strings.Contains(row, "0 B") {
			t.Errorf("a folder must not claim a size: %q", row)
		}
		if !strings.Contains(row, "-") {
			t.Errorf("a folder should show a dash for size: %q", row)
		}
	})

	t.Run("a bucket shows its creation date", func(t *testing.T) {
		b := colObj("my-bucket", model.Bucket)
		b.LastModified = when
		row := stripTags(listRow(b, false, cols))
		if !strings.Contains(row, "my-bucket") || !strings.Contains(row, "2026-07-30") {
			t.Errorf("row = %q", row)
		}
	})

	t.Run("a missing date degrades to a dash", func(t *testing.T) {
		row := stripTags(listRow(colFile("a.txt", 1, nil, ""), false, cols))
		if !strings.Contains(row, "-") {
			t.Errorf("row = %q", row)
		}
	})

	t.Run("selection is still colourised", func(t *testing.T) {
		row := listRow(colFile("a.txt", 1, when, ""), true, cols)
		if !strings.Contains(row, "[green]") {
			t.Errorf("a marked row must stay green: %q", row)
		}
	})

	t.Run("nil objects are handled", func(t *testing.T) {
		if listRow(nil, false, cols) != "" {
			t.Error("nil object should render as empty")
		}
		if listRow(&model.Object{Ot: model.File}, false, cols) != "" {
			t.Error("keyless object should render as empty")
		}
	})
}

func TestListRowTruncation(t *testing.T) {
	cols := planColumns(60)
	long := strings.Repeat("very-long-name-", 12) + ".txt"
	row := listRow(colFile(long, 1, colTime(t), ""), false, cols)

	if w := tview.TaggedStringWidth(row); w > 60 {
		t.Errorf("row is %d cells wide, want <= 60:\n%q", w, row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("a truncated name should be marked with an ellipsis: %q", row)
	}
}

func TestTruncateDisplay(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"truncate me", 8, "truncat…"},
		{"x", 1, "x"},
		{"xy", 1, "…"},
		{"anything", 0, ""},
		{"anything", -1, ""},
	}
	for _, c := range cases {
		if got := truncateDisplay(c.in, c.width); got != c.want {
			t.Errorf("truncateDisplay(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

func TestPadDisplayIgnoresTags(t *testing.T) {
	// The padding must be computed on visible cells; a colour tag is zero-width.
	got := padDisplay("[green]ab[-]", 5)
	if w := tview.TaggedStringWidth(got); w != 5 {
		t.Errorf("padded width = %d, want 5 (%q)", w, got)
	}

	// Emoji are double-width and must count as two cells.
	got = padDisplay("📄", 5)
	if w := tview.TaggedStringWidth(got); w != 5 {
		t.Errorf("padded emoji width = %d, want 5 (%q)", w, got)
	}

	// Already at or over the width: unchanged.
	if got := padDisplay("abcdef", 3); got != "abcdef" {
		t.Errorf("got %q, want the input unchanged", got)
	}
}

func TestListHeaderMatchesColumns(t *testing.T) {
	t.Run("captions appear only for the columns that fit", func(t *testing.T) {
		wide := listHeader(planColumns(120))
		for _, want := range []string{"NAME", "SIZE", "MODIFIED", "CLASS"} {
			if !strings.Contains(wide, want) {
				t.Errorf("wide header missing %q: %q", want, wide)
			}
		}

		narrow := listHeader(planColumns(24))
		if !strings.Contains(narrow, "NAME") {
			t.Errorf("narrow header should still name the column: %q", narrow)
		}
		for _, unwanted := range []string{"SIZE", "MODIFIED", "CLASS"} {
			if strings.Contains(narrow, unwanted) {
				t.Errorf("narrow header should not claim %q: %q", unwanted, narrow)
			}
		}
	})

	t.Run("the SIZE caption sits over the size column", func(t *testing.T) {
		cols := planColumns(120)
		header := listHeader(cols)
		row := stripTags(listRow(colFile("a.txt", 2048, colTime(t), ""), false, cols))

		// Both end their size field at the same cell, since size is right-aligned.
		// Measured in display cells, not bytes: the row's leading emoji is four
		// bytes but two cells, so a byte comparison is off by two.
		hEnd := fieldEndCell(header, "SIZE")
		rEnd := fieldEndCell(row, "2.0 KiB")
		if hEnd != rEnd {
			t.Errorf("SIZE caption ends at %d but the value ends at %d\n  %q\n  %q", hEnd, rEnd, header, row)
		}
	})
}
