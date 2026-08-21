package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

const (
	// overwriteRows is how many conflicting keys the confirmation lists
	// before it summarises the rest.
	overwriteRows = 8
	// overwritePage is the confirmation's own page name, so a background
	// toast (which lives on modal-msg) can't replace it mid-decision.
	overwritePage = "modal-overwrite"
)

// overwriteText renders the confirmation body: what is about to be replaced,
// and how much of the operation that is.
func overwriteText(title string, conflicts []string, candidates int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d of %d object(s) already exist at the destination\nand would be replaced.\n\n", title, len(conflicts), candidates)
	for i, k := range conflicts {
		if i == overwriteRows {
			fmt.Fprintf(&b, "  ...and %d more\n", len(conflicts)-overwriteRows)
			break
		}
		b.WriteString("  - " + k + "\n")
	}
	return b.String()
}

// confirmOverwrites is the guard every remote write goes through: it works out
// what the operation would land on, and when any of it already exists it says
// so before a byte moves. Without it a rename onto an existing name — or a
// copy into a folder holding same-named objects — destroys the old content
// with no warning at all, while the download path has always prompted.
//
// plan runs off the UI goroutine (it may list a folder) and returns the exact
// destination keys. proceed then runs on the UI goroutine with the keys to
// leave alone: nil means overwrite everything. Cancelling never calls proceed.
func (c *Controller) confirmOverwrites(
	mdl *model.Model,
	bucket *model.Object,
	title string,
	plan func() ([]string, error),
	proceed func(skip map[string]bool),
) {
	scanning := tview.NewModal().SetText("Checking the destination...")
	c.view.Pages.AddPage("progress", scanning, true, true)

	// fail reports and stops. A destination that can't be checked is not a
	// destination that may be silently overwritten, so there is no fallthrough
	// to the write.
	fail := func(err error) {
		c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
		c.error(title, err)
	}

	go func() {
		keys, err := plan()
		if err != nil {
			// A planning failure is about the operation itself (an impossible
			// folder copy, an unreadable source), not about the destination.
			fail(err)
			return
		}
		conflicts, err := mdl.Conflicts(context.Background(), bucket, keys)
		if err != nil {
			fail(fmt.Errorf("checking the destination: %w", err))
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress")
			if len(conflicts) == 0 {
				proceed(nil)
				return
			}
			c.askOverwriteRemote(title, conflicts, len(keys), func(skip map[string]bool, ok bool) {
				if ok {
					proceed(skip)
				}
			})
		})
	}()
}

// askOverwriteRemote presents the conflict list. "Skip existing" is offered only
// when it would leave something to do — skipping every candidate is just
// Cancel with extra steps.
//
// decide is called exactly once, on every exit including Esc: a caller may be
// a goroutine blocked on the answer, and a path that returns without deciding
// would hang it forever.
func (c *Controller) askOverwriteRemote(title string, conflicts []string, candidates int, decide func(skip map[string]bool, ok bool)) {
	modal := tview.NewModal().SetText(overwriteText(title, conflicts, candidates))

	buttons := []string{"Overwrite"}
	if len(conflicts) < candidates {
		buttons = append(buttons, "Skip existing")
	}
	buttons = append(buttons, "Cancel")

	modal.AddButtons(buttons).SetDoneFunc(func(_ int, label string) {
		c.view.Pages.RemovePage(overwritePage)
		switch label {
		case "Overwrite":
			decide(nil, true)
		case "Skip existing":
			skip := make(map[string]bool, len(conflicts))
			for _, k := range conflicts {
				skip[k] = true
			}
			decide(skip, true)
		default:
			decide(nil, false)
		}
	})
	modal.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage(overwritePage)
			decide(nil, false)
			return nil
		}
		return event
	})

	c.view.Pages.AddPage(overwritePage, modal, true, true)
	c.view.App.SetFocus(modal)
}

// askOverwriteBlocking is the same question asked from a flow that is already
// running in a goroutine: it blocks on the answer instead of continuing
// through a callback. ok is false when the user cancelled.
func (c *Controller) askOverwriteBlocking(title string, conflicts []string, candidates int) (map[string]bool, bool) {
	type answer struct {
		skip map[string]bool
		ok   bool
	}
	ch := make(chan answer, 1)

	c.view.App.QueueUpdateDraw(func() {
		c.askOverwriteRemote(title, conflicts, candidates, func(skip map[string]bool, ok bool) {
			ch <- answer{skip: skip, ok: ok}
		})
	})

	a := <-ch
	return a.skip, a.ok
}

// plannedCopyKeys is the plan function for a copy/move/rename: the exact
// destination keys of every item, folders expanded at the source.
func plannedCopyKeys(mdl *model.Model, srcBucket, dstBucket *model.Object, items []copyMoveItem, dstPrefix string) func() ([]string, error) {
	return func() ([]string, error) {
		var keys []string
		for _, it := range items {
			dstKey := dstPrefix + it.shortName
			if it.isFolder {
				dstKey += "/"
			}
			planned, err := mdl.PlannedCopyKeys(srcBucket, dstBucket, it.srcKey, dstKey, it.isFolder)
			if err != nil {
				return nil, err
			}
			keys = append(keys, planned...)
		}
		return keys, nil
	}
}
