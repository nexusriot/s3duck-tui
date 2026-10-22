package controller

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// Marking by pattern, Midnight Commander's + / - / *: mark every visible item
// whose name matches, unmark them, or flip every mark. The pattern is the
// filter's own query syntax (text, *.glob, re:regex), matched against the
// name shown in the listing, and "visible" means what the filter currently
// lets through — the same scope Ctrl+S (mark everything visible) uses, so
// marks never land on rows the user cannot see.

// markScope is the set of rows a pattern or an invert acts on.
func markScope(objs []*model.Object, filter string, key sortKey, desc bool) []*model.Object {
	var out []*model.Object
	for _, o := range filterSortObjects(objs, filter, key, desc) {
		if o.Ot == model.File || o.Ot == model.Folder {
			out = append(out, o)
		}
	}
	return out
}

// namesMatching returns the mark keys of the rows whose displayed name
// matches m. filesOnly leaves folders alone — the default, because a folder
// mark is a recursive one, and "*" then Delete should not take a tree with it.
func namesMatching(scope []*model.Object, m matcher, filesOnly bool) []string {
	var out []string
	for _, o := range scope {
		if o.Key == nil || (filesOnly && o.Ot != model.File) {
			continue
		}
		if m.match(*o.Key) {
			out = append(out, objKey(o))
		}
	}
	return out
}

// MarkByPattern asks for a pattern and marks (mark=true) or unmarks every
// visible item that matches it.
func (c *Controller) MarkByPattern(mark bool) {
	if !c.canMark() {
		return
	}
	verb, title := "Mark", " Mark matching — text, *.glob or re:regex "
	if !mark {
		verb, title = "Unmark", " Unmark matching — text, *.glob or re:regex "
	}
	pattern := c.lastMarkPattern
	if pattern == "" {
		pattern = "*"
	}

	// lay is assigned below; the buttons fire only after that.
	var lay *shell.Layer
	form := c.view.NewInputForm(title, "Pattern", pattern)
	form.AddCheckbox("Include folders", c.markFolders, nil)
	form.AddButton(verb, func() {
		q := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		folders := form.GetFormItem(1).(*tview.Checkbox).IsChecked()
		lay.Close()
		if q == "" {
			return
		}
		m, err := newMatcher(q)
		if err != nil {
			c.error(verb+" by pattern", fmt.Errorf("%q: %w", q, err))
			return
		}
		c.lastMarkPattern, c.markFolders = q, folders

		pane := c.pane()
		sk, sd := c.getSort()
		names := namesMatching(markScope(pane.Snapshot(), pane.Filter(), sk, sd), m, !folders)
		if mark {
			pane.SelectNames(names)
		} else {
			for _, n := range names {
				pane.Unselect(n)
			}
		}
		go c.renderList()
		c.queue.SetNotice(fmt.Sprintf(" %sed %d item(s) matching %s ", strings.ToLower(verb), len(names), tview.Escape(q)))
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 70, 9)
}

// InvertMarks flips the mark on every visible file and folder.
func (c *Controller) InvertMarks() {
	if !c.canMark() {
		return
	}
	pane := c.pane()
	sk, sd := c.getSort()
	for _, o := range markScope(pane.Snapshot(), pane.Filter(), sk, sd) {
		pane.ToggleSelected(objKey(o))
	}
	go c.renderList()
}
