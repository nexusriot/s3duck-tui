package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// The deleted-objects view: keys under the current folder whose latest entry
// is a delete marker. An ordinary listing cannot show them — that is what a
// delete marker is for — and the version browser opens only on a listed
// object, so undelete existed but could not be reached. This view is a list
// of its own rather than rows mixed into the browser: nothing here exists as
// an object, and every browser action pointed at one (download, copy,
// preview) would only fail.

// deletedCap bounds the view. The scan is a recursive version listing, so it
// is cancellable and reports progress; a prefix with more deleted keys than
// this is narrowed by opening a deeper folder.
const deletedCap = 5000

// deletedRow renders one deleted object: its key relative to the folder the
// view was opened in, when it was deleted, and what undelete would bring back.
func deletedRow(d model.DeletedObject, prefix string, marked bool) string {
	mark := "  "
	if marked {
		mark = "[green]●[-] "
	}
	when := "?"
	if d.DeletedAt != nil {
		when = d.DeletedAt.Local().Format("2006-01-02 15:04")
	}
	back := "[gray]nothing to restore (no version beneath)[-]"
	if d.Versions > 0 {
		back = fmt.Sprintf("[gray]restores %s, %d version(s) kept[-]", humanize.IBytes(uint64(d.Size)), d.Versions)
	}
	return fmt.Sprintf("%s%s  [red]deleted %s[-]  %s", mark, tview.Escape(strings.TrimPrefix(d.Key, prefix)), when, back)
}

// deletedTitle heads the view.
func deletedTitle(bucket, prefix string, n int, truncated bool) string {
	more := ""
	if truncated {
		more = fmt.Sprintf(", first %d", n)
	}
	return fmt.Sprintf(" Deleted under %s/%s — %d%s (Space mark, u undelete, v versions, Esc close) ",
		tview.Escape(bucket), tview.Escape(prefix), n, more)
}

// undeleteTargets is the marked rows, or the highlighted one when none are.
func undeleteTargets(all []model.DeletedObject, marked map[string]bool, current int) []model.DeletedObject {
	var out []model.DeletedObject
	for _, d := range all {
		if marked[d.Key] {
			out = append(out, d)
		}
	}
	if len(out) == 0 && current >= 0 && current < len(all) {
		out = append(out, all[current])
	}
	return out
}

// ShowDeleted scans for deleted keys under the current folder and lists them.
// Bound to leader D.
func (c *Controller) ShowDeleted() {
	if c.remoteOnly("Deleted objects") {
		return
	}
	bucket := c.pane().Bucket()
	if bucket == nil || bucket.Key == nil {
		c.error("Deleted objects", fmt.Errorf("open a bucket first"))
		return
	}
	prefix := c.pane().Path()
	mdl := c.model
	wait, modal, ctx, cancel := c.cancellableWait("Looking for deleted objects...")
	go func() {
		defer cancel()
		found, truncated, err := mdl.FindDeleted(ctx, bucket, prefix, deletedCap, func(scanned, n int) {
			c.shell.Do(func() {
				modal.SetText(fmt.Sprintf("Looking for deleted objects...\n\n%s version(s) scanned, %d deleted key(s)", humanCount(scanned), n))
			})
		})
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Deleted objects", err)
			}
			return
		}
		if len(found) == 0 {
			c.success(fmt.Sprintf("Nothing deleted under %s/%s (or versioning was never on: without it a delete leaves nothing behind)", *bucket.Key, prefix))
			return
		}
		c.shell.Do(func() { c.presentDeleted(mdl, bucket, prefix, found, truncated) })
	}()
}

// presentDeleted shows the list. UI goroutine.
func (c *Controller) presentDeleted(mdl *model.Model, bucket *model.Object, prefix string, found []model.DeletedObject, truncated bool) {
	var lay *shell.Layer
	marked := map[string]bool{}
	list := tview.NewList().ShowSecondaryText(false).SetHighlightFullLine(true)
	list.SetBorder(true).SetTitle(deletedTitle(*bucket.Key, prefix, len(found), truncated))
	redraw := func() {
		cur := list.GetCurrentItem()
		list.Clear()
		for _, d := range found {
			list.AddItem(deletedRow(d, prefix, marked[d.Key]), "", 0, nil)
		}
		if cur >= 0 && cur < list.GetItemCount() {
			list.SetCurrentItem(cur)
		}
	}
	redraw()

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		i := list.GetCurrentItem()
		switch {
		case ev.Key() == tcell.KeyEsc || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q'):
			lay.Close()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
			if i >= 0 && i < len(found) {
				marked[found[i].Key] = !marked[found[i].Key]
				redraw()
				if i+1 < list.GetItemCount() {
					list.SetCurrentItem(i + 1)
				}
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'v':
			if i >= 0 && i < len(found) {
				d := found[i]
				lay.Close()
				c.openVersions(bucket, d.Key)
			}
			return nil
		case ev.Key() == tcell.KeyEnter || (ev.Key() == tcell.KeyRune && ev.Rune() == 'u'):
			targets := undeleteTargets(found, marked, i)
			if len(targets) == 0 {
				return nil
			}
			if c.readOnlyBlocked("undelete") {
				return nil
			}
			c.confirmThen(undeleteConfirmText(targets), func() {
				lay.Close()
				c.runUndelete(mdl, bucket, targets)
			})
			return nil
		}
		return ev
	})
	lay = c.pushModalClamped(list, 120, 30)
	c.view.App.SetFocus(list)
}

// undeleteConfirmText says what undelete does — removes the marker, nothing
// else — so it reads as the reversible step it is.
func undeleteConfirmText(ts []model.DeletedObject) string {
	var b strings.Builder
	if len(ts) == 1 {
		fmt.Fprintf(&b, "Undelete %s?\n\n", ts[0].Key)
	} else {
		fmt.Fprintf(&b, "Undelete %d objects?\n\n", len(ts))
	}
	b.WriteString("Removes the delete marker on top, so the version beneath is current again. No other version changes.")
	return b.String()
}

// runUndelete removes each marker and reports. Off the UI goroutine.
func (c *Controller) runUndelete(mdl *model.Model, bucket *model.Object, ts []model.DeletedObject) {
	go func() {
		res := newOpResult("Undelete")
		for _, d := range ts {
			if err := mdl.Undelete(context.Background(), bucket, d); err != nil {
				res.fail(retryItem{label: d.Key}, err)
				continue
			}
			res.ok()
		}
		c.logActivity("Undelete in %s: %d restored, %d failed", *bucket.Key, res.okCount, len(res.failures))
		if len(res.failures) > 0 {
			c.error("Undelete", fmt.Errorf("%d of %d failed; first: %s: %v",
				len(res.failures), len(ts), res.failures[0].item.label, res.failures[0].err))
		} else {
			c.success(fmt.Sprintf("Undeleted %d object(s)", res.okCount))
		}
		c.updateList()
	}()
}
