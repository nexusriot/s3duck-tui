package controller

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// previewBytes is how much of an object the preview reads. Enough for a
// header, a log tail-sniff or a config file in full; small enough to be
// instant on any link.
const previewBytes = 64 << 10

// hexdumpBytes is how much of a binary object is rendered. A hexdump is four
// times as wide as its input, so the whole 64 KiB would be a wall.
const hexdumpBytes = 2 << 10

// hexdump renders data in the classic 16-bytes-per-line form: offset, hex,
// and the printable ASCII gutter that makes embedded strings readable.
func hexdump(data []byte) string {
	var b strings.Builder
	for off := 0; off < len(data); off += 16 {
		end := off + 16
		if end > len(data) {
			end = len(data)
		}
		row := data[off:end]
		fmt.Fprintf(&b, "%08x  ", off)
		for i := 0; i < 16; i++ {
			if i < len(row) {
				fmt.Fprintf(&b, "%02x ", row[i])
			} else {
				b.WriteString("   ")
			}
			if i == 7 {
				b.WriteByte(' ')
			}
		}
		b.WriteString(" |")
		for _, ch := range row {
			if ch >= 0x20 && ch < 0x7f {
				b.WriteByte(ch)
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteString("|\n")
	}
	return b.String()
}

// escapeForTextView neutralises tview's colour tags in object content. Without
// this a log line containing "[red]" would be swallowed as markup — and a
// preview that silently drops parts of the text is worse than no preview.
//
// tview.Escape is the escape this tview understands (a tag is escaped by an
// "[" before its closing "]"). Doubling every "[" is not one: "[default]"
// rendered as "[", "[1, 2, 3]" as "[[1, 2, 3]", and "[red]" still coloured.
func escapeForTextView(s string) string {
	return tview.Escape(s)
}

// sanitizeText makes arbitrary bytes safe to render as text: tabs become
// spaces, other control characters become dots, and invalid UTF-8 is replaced
// by the standard rune rather than corrupting the terminal.
func sanitizeText(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	for _, r := range string(data) {
		switch {
		case r == '\n' || r == '\r':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r == unicode.ReplacementChar:
			b.WriteRune(r)
		case unicode.IsControl(r):
			b.WriteByte('.')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// previewBody renders what was read: text as text, binary as a hexdump. The
// returned label names which decision was made, since "why am I looking at
// hex?" needs an answer on screen.
func previewBody(data []byte) (body, label string) {
	if isProbablyBinary(data) {
		clip := data
		if len(clip) > hexdumpBytes {
			clip = clip[:hexdumpBytes]
		}
		// The gutter shows printable bytes verbatim, so it needs the same
		// escaping as text: "ab[red]cd" would otherwise lose its tag.
		return escapeForTextView(hexdump(clip)), fmt.Sprintf("binary — hexdump of the first %s", humanize.IBytes(uint64(len(clip))))
	}
	return escapeForTextView(sanitizeText(data)), fmt.Sprintf("text — first %s", humanize.IBytes(uint64(len(data))))
}

// previewTitle names the object and how much of it is shown.
func previewTitle(key string, shown int64, size *int64, label string) string {
	total := "unknown size"
	if size != nil {
		total = humanize.IBytes(uint64(*size))
	}
	truncated := ""
	if size != nil && shown < *size {
		truncated = " · truncated"
	}
	return fmt.Sprintf(" %s — %s of %s (%s%s) ", key, humanize.IBytes(uint64(shown)), total, label, truncated)
}

// Preview shows the head of the highlighted object read-only. Unlike the
// editor it has no size cap and no binary refusal: it reads a fixed window
// with a ranged GET, so cost is independent of the object's size.
func (c *Controller) Preview() {
	if c.remoteOnly("Preview") {
		return
	}
	if c.pane().Bucket() == nil {
		return
	}
	_, obj, ok := c.currentObject()
	if !ok || obj == nil || obj.Ot != model.File || obj.FullPath == nil {
		c.error("Preview", fmt.Errorf("highlight a file"))
		return
	}
	key := *obj.FullPath
	size := obj.Size
	bucket := c.pane().Bucket()
	mdl := c.model

	wait, _, ctx, cancel := c.cancellableWait(fmt.Sprintf("Reading %s ...", key))
	go func() {
		defer cancel()
		data, ctype, err := mdl.GetObjectHead(ctx, bucket, key, previewBytes)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Preview failed", err)
			}
			return
		}
		body, label := previewBody(data)
		if ctype != "" {
			label += " · " + ctype
		}
		title := previewTitle(key, int64(len(data)), size, label)
		c.showTextOverlay(title, body,
			"[gray]↑↓/PgUp/PgDn[white] scroll · [gray]e[white] edit · [gray]Esc/q[white] close",
			func(ev *tcell.EventKey, lay *shell.Layer) bool {
				if ev.Key() == tcell.KeyRune && ev.Rune() == 'e' {
					lay.Close()
					c.EditObject()
					return true
				}
				return false
			})
	}()
}

// showTextOverlay is the shared read-only text viewer: a scrollable TextView
// with a one-line footer, closed with Esc or q. extra lets a caller add its
// own keys (the preview's "e" to open the editor).
//
// Scrolling is the reason this is not a tview.Modal: a Modal's text cannot be
// scrolled, and a preview that shows only what fits is not a preview. Sized
// through ModalClamped so it can never ask for more rows than the terminal
// has — the mistake that made the hotkey panel's tail unreachable.
func (c *Controller) showTextOverlay(title, body, footer string, extra func(*tcell.EventKey, *shell.Layer) bool) {
	c.shell.Do(func() {
		// lay is assigned below; every closure here runs after that.
		var lay *shell.Layer
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
		tv.SetText(body)
		tv.SetBorder(true).SetTitle(title)

		foot := tview.NewTextView().SetDynamicColors(true)
		foot.SetText(footer)

		layout := tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(tv, 0, 1, true).
			AddItem(foot, 1, 0, false)

		close := func() { lay.Close() }
		tv.SetDoneFunc(func(key tcell.Key) {
			switch key {
			case tcell.KeyEsc, tcell.KeyEnter, tcell.KeyTab:
				close()
			}
		})
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if extra != nil && extra(ev, lay) {
				return nil
			}
			if ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q') {
				close()
				return nil
			}
			return ev
		})

		lay = c.pushModalClamped(layout, 120, 40)
		c.view.App.SetFocus(tv)
	})
}

// maxDiffCells bounds the LCS table. The walk is quadratic in LINES, not in
// bytes, so the editMaxSize cap on the bodies bounds nothing useful: a 1 MiB
// CSV of 40-byte rows is ~26k lines a side, and a table that size is ~5 GiB of
// int — the process dies before it can draw anything. 4M cells is ~32 MiB and
// covers two 2000-line files, which is every config file a version diff is
// actually for; past it the diff declines rather than taking the app with it.
const maxDiffCells = 4 << 20

// errDiffTooLarge is what unifiedDiff reports instead of allocating a table
// that would exhaust memory. It is a sentinel so the caller can word the
// message; errors.Is identifies it.
var errDiffTooLarge = errors.New("too many lines to diff")

// unifiedDiff renders a minimal line diff between two texts. It is a plain
// longest-common-subsequence walk rather than a full Myers implementation:
// within maxDiffCells the quadratic table costs nothing worth optimising, and
// the result is the same.
//
// Context is unlimited — every line is shown, changed or not — because a
// version diff is usually a config file where the surrounding lines are the
// point. Empty output with a nil error means the two versions are identical.
func unifiedDiff(a, b string, labelA, labelB string) (string, error) {
	linesA := splitLines(a)
	linesB := splitLines(b)

	if cells := int64(len(linesA)+1) * int64(len(linesB)+1); cells > maxDiffCells {
		return "", fmt.Errorf("%w: %d and %d lines", errDiffTooLarge, len(linesA), len(linesB))
	}

	// lcs[i][j] = length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(linesA)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(linesB)+1)
	}
	for i := len(linesA) - 1; i >= 0; i-- {
		for j := len(linesB) - 1; j >= 0; j-- {
			if linesA[i] == linesB[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var b2 strings.Builder
	fmt.Fprintf(&b2, "[gray]--- %s\n+++ %s[white]\n", labelA, labelB)
	changed := false
	i, j := 0, 0
	for i < len(linesA) && j < len(linesB) {
		switch {
		case linesA[i] == linesB[j]:
			fmt.Fprintf(&b2, "  %s\n", escapeForTextView(linesA[i]))
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&b2, "[red]- %s[white]\n", escapeForTextView(linesA[i]))
			i++
			changed = true
		default:
			fmt.Fprintf(&b2, "[green]+ %s[white]\n", escapeForTextView(linesB[j]))
			j++
			changed = true
		}
	}
	for ; i < len(linesA); i++ {
		fmt.Fprintf(&b2, "[red]- %s[white]\n", escapeForTextView(linesA[i]))
		changed = true
	}
	for ; j < len(linesB); j++ {
		fmt.Fprintf(&b2, "[green]+ %s[white]\n", escapeForTextView(linesB[j]))
		changed = true
	}
	if !changed {
		return "", nil
	}
	return b2.String(), nil
}

// splitLines splits on newlines without producing a trailing empty line for
// text that ends in one.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// diffVersions shows a unified diff between one version of an object and the
// current one. Both bodies are capped like the editor's, and a binary body is
// refused — a hexdump diff would be noise.
func (c *Controller) diffVersions(bucket *model.Object, key string, v model.ObjectVersion) {
	mdl := c.model
	wait, _, ctx, cancel := c.cancellableWait("Fetching both versions...")
	go func() {
		defer cancel()
		older, err := mdl.GetVersionContent(ctx, bucket, key, v.VersionID, editMaxSize)
		if err != nil {
			c.shell.Do(wait.Close)
			if ctx.Err() == nil {
				c.error("Diff: reading the old version failed", err)
			}
			return
		}
		current, err := mdl.GetVersionContent(ctx, bucket, key, "", editMaxSize)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Diff: reading the current version failed", err)
			}
			return
		}
		if isProbablyBinary(older) || isProbablyBinary(current) {
			c.error("Diff", fmt.Errorf("%s looks binary; a line diff would be meaningless", key))
			return
		}

		short := v.VersionID
		if len(short) > 8 {
			short = short[:8]
		}
		body, err := unifiedDiff(string(older), string(current), "version "+short, "current")
		if err != nil {
			c.error("Diff", fmt.Errorf("%s: %w — download both versions and diff them locally", key, err))
			return
		}
		if body == "" {
			c.success(fmt.Sprintf("%s: version %s is identical to the current one", key, short))
			return
		}
		c.showTextOverlay(
			fmt.Sprintf(" %s — version %s vs current ", key, short),
			body,
			"[gray]↑↓/PgUp/PgDn[white] scroll · [gray]Esc/q[white] close",
			nil)
	}()
}
