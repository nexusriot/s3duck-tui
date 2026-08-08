package controller

import (
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/mattn/go-runewidth"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// Listing row layout. The browser is a tview.List, whose rows are a single
// string, so "columns" are produced by padding rather than by a real table
// widget — switching to tview.Table would mean rewriting every cursor,
// selection and reveal call site that currently speaks List.
//
// Widths are display cells, not bytes: the icons are double-width emoji (📄 is
// four bytes and two cells). Padding uses tview.TaggedStringWidth, which also
// discounts colour tags; truncation uses runewidth and is therefore only ever
// applied to untagged text — hence listRow truncates a name *before* colouring
// it.
const (
	colIconWidth  = 3  // emoji (2 cells) + separating space
	colSizeWidth  = 10 // "1023.9 KiB"
	colDateWidth  = 16 // "2006-01-02 15:04"
	colClassWidth = 12 // "INTELLIGENT_" truncated; longest shown is DEEP_ARCHIVE
	colGap        = 2
	colMinName    = 16 // below this a row is unreadable, so drop a column instead

	// colFallbackWidth is used before the first draw, when the widget has no
	// size yet.
	colFallbackWidth = 80

	colDateLayout = "2006-01-02 15:04"
)

// colGapStr is the inter-column separator, derived from colGap so the two can
// never disagree, and built once instead of on every column of every row.
var colGapStr = strings.Repeat(" ", colGap)

// listColumns is the resolved layout for one render: how wide the name may be
// and which optional columns fit alongside it.
type listColumns struct {
	nameWidth int
	size      bool
	date      bool
	class     bool
}

// planColumns decides which columns fit in the given total width.
//
// Priority is size → date → class, because size is the column the sort keys
// most often order by and the one a name alone can never imply. Each column is
// only taken if what remains still leaves a readable name, so a narrow
// dual-pane degrades gracefully to just names instead of truncating everything
// into uselessness.
func planColumns(width int) listColumns {
	if width <= 0 {
		width = colFallbackWidth
	}

	cols := listColumns{}
	avail := width - colIconWidth

	// Stop at the first column that doesn't fit rather than skipping it: with
	// `continue`, dropping the date would free enough room for the (lower
	// priority) class to reappear, so shrinking the pane could *add* a column.
	for _, c := range []struct {
		enable *bool
		width  int
	}{
		{&cols.size, colSizeWidth},
		{&cols.date, colDateWidth},
		{&cols.class, colClassWidth},
	} {
		if avail-(c.width+colGap) < colMinName {
			break
		}
		*c.enable = true
		avail -= c.width + colGap
	}

	cols.nameWidth = avail
	if cols.nameWidth < 1 {
		cols.nameWidth = 1
	}
	return cols
}

// truncateDisplay shortens s to at most width display cells, marking the cut
// with an ellipsis. Width is counted in cells so double-width runes can't
// overflow the column.
func truncateDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return runewidth.Truncate(s, width-1, "") + "…"
}

// padDisplay right-pads s with spaces to width display cells, ignoring colour
// tags. A string already at or over the width is returned unchanged.
func padDisplay(s string, width int) string {
	if pad := width - tview.TaggedStringWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// rowIcon returns the type marker and the colour the name is drawn in.
func rowIcon(ot model.FType, selected bool) (icon, color string) {
	switch ot {
	case model.Folder:
		if selected {
			return "📁", "[green]"
		}
		return "[cyan]📁[-]", ""
	case model.File:
		if selected {
			return "📄", "[green]"
		}
		return "📄", ""
	case model.Bucket:
		return "[yellow]●[-]", ""
	default:
		return " ", ""
	}
}

// rowSize renders the size column. Folders and buckets have no size of their
// own — S3 does not track one — so they show a dash rather than a misleading 0.
func rowSize(o *model.Object) string {
	if o.Ot != model.File || o.Size == nil {
		return "-"
	}
	return humanize.IBytes(uint64(*o.Size))
}

// rowDate renders the date column. For a bucket this is its creation date,
// which is what ListBuckets reports.
func rowDate(o *model.Object) string {
	if o.LastModified == nil || o.LastModified.IsZero() {
		return "-"
	}
	return o.LastModified.In(time.Local).Format(colDateLayout)
}

// rowClass renders the storage-class column. STANDARD is the default and is
// left blank: showing it on every row would bury the handful of objects whose
// class actually differs, which is the only reason to have the column.
func rowClass(o *model.Object) string {
	if o.Ot != model.File || o.StorageClass == nil {
		return ""
	}
	if strings.EqualFold(*o.StorageClass, "STANDARD") {
		return ""
	}
	return *o.StorageClass
}

// listRow renders one listing line: icon, padded name, then whichever of
// size / date / storage class fit. Size is right-aligned so magnitudes line up
// down the column.
func listRow(o *model.Object, selected bool, cols listColumns) string {
	if o == nil || o.Key == nil {
		return ""
	}

	name := *o.Key
	if o.Ot == model.Folder {
		name += "/"
	}
	icon, color := rowIcon(o.Ot, selected)

	// Truncate the bare name, then colour it: colour tags are zero-width but
	// would confuse a byte- or rune-based truncation.
	name = truncateDisplay(name, cols.nameWidth)
	if color != "" {
		name = color + name + "[-]"
	}

	var b strings.Builder
	b.WriteString(padDisplay(icon, colIconWidth))
	b.WriteString(padDisplay(name, cols.nameWidth))

	if cols.size {
		b.WriteString(colGapStr)
		size := truncateDisplay(rowSize(o), colSizeWidth)
		// Right-align by padding in front.
		if pad := colSizeWidth - runewidth.StringWidth(size); pad > 0 {
			size = strings.Repeat(" ", pad) + size
		}
		b.WriteString("[gray]" + size + "[-]")
	}
	if cols.date {
		b.WriteString(colGapStr)
		b.WriteString("[gray]" + padDisplay(truncateDisplay(rowDate(o), colDateWidth), colDateWidth) + "[-]")
	}
	if cols.class {
		b.WriteString(colGapStr)
		class := truncateDisplay(rowClass(o), colClassWidth)
		if class != "" {
			class = "[yellow]" + class + "[-]"
		}
		b.WriteString(class)
	}

	return strings.TrimRight(b.String(), " ")
}

// listHeader renders the caption line drawn in the per-pane header TextView
// above the list, so the columns are identifiable without guessing. The caller
// indents it to match the list's inner rect.
func listHeader(cols listColumns) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", colIconWidth))
	b.WriteString(padDisplay("NAME", cols.nameWidth))
	if cols.size {
		b.WriteString(colGapStr)
		b.WriteString(padDisplay(strings.Repeat(" ", colSizeWidth-4)+"SIZE", colSizeWidth))
	}
	if cols.date {
		b.WriteString(colGapStr)
		b.WriteString(padDisplay("MODIFIED", colDateWidth))
	}
	if cols.class {
		b.WriteString(colGapStr)
		b.WriteString("CLASS")
	}
	return strings.TrimRight(b.String(), " ")
}
