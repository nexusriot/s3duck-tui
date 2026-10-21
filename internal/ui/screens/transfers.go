// Package screens holds the app's self-contained overlay screens: the ones with
// their own widgets, their own key handling and no business logic of their own.
//
// They used to live on the controller, which meant their widget handles were
// controller fields — two more things for every other method to step around.
// A screen here owns its widgets for as long as it is open and nothing else
// can see them.
package screens

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/jobs"
	"github.com/nexusriot/s3duck-tui/internal/ui/flows"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

// transferRefresh is how often the open panel redraws. Fast enough that a
// percentage moves, slow enough to cost nothing.
const transferRefresh = 300 * time.Millisecond

// Transfers is the background-transfer panel: what is running, what finished,
// and the keys to cancel or clear.
type Transfers struct {
	shell *shell.Shell
	queue *jobs.Queue
	// modal wraps a primitive for display. Supplied by the caller because
	// sizing belongs to the view, not here.
	modal func(p tview.Primitive, width, height int) *shell.Layer
}

// NewTransfers wires the panel. modal is what puts a primitive on screen.
func NewTransfers(sh *shell.Shell, q *jobs.Queue, modal func(tview.Primitive, int, int) *shell.Layer) *Transfers {
	return &Transfers{shell: sh, queue: q, modal: modal}
}

// Show opens the panel, refreshing while it is up. d/Del cancels the selected
// job, c clears the finished ones, Esc closes.
//
// Must be called on the UI goroutine.
func (t *Transfers) Show() {
	// lay is assigned at the bottom; the handlers below run only after that.
	var lay *shell.Layer
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(" Transfers — d: cancel • c: clear finished • Esc: close ")
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	render := func() { t.render(list) }
	render()

	// done stops the ticker when the panel closes. Buffered-free and closed
	// exactly once, by closePanel.
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(transferRefresh)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				t.shell.Do(func() {
					// The layer may have closed between the tick and the
					// queued redraw; rendering into a detached list is wasted
					// work at best.
					if lay.IsOpen() {
						render()
					}
				})
			}
		}
	}()

	closed := false
	closePanel := func() {
		if closed {
			return
		}
		closed = true
		close(done)
		lay.Close()
	}

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			closePanel()
			return nil
		case tcell.KeyDelete:
			t.queue.CancelAt(list.GetCurrentItem())
			return nil
		case tcell.KeyRune:
			switch ev.Rune() {
			case 'd':
				t.queue.CancelAt(list.GetCurrentItem())
				return nil
			case 'c':
				t.queue.ClearFinished()
				render()
				return nil
			}
		}
		return ev
	})

	lay = t.modal(list, 100, 28)
	t.shell.Focus(list)
}

// render rebuilds the rows, keeping the cursor where it was.
func (t *Transfers) render(list *tview.List) {
	cur := list.GetCurrentItem()
	list.Clear()

	views := t.queue.Snapshot()
	if len(views) == 0 {
		list.AddItem("[gray](no transfers)[-]", "", 0, nil)
		return
	}
	for _, jv := range views {
		primary, secondary := TransferRow(jv, time.Since(jv.Start))
		list.AddItem(primary, secondary, 0, nil)
	}
	if cur >= 0 && cur < list.GetItemCount() {
		list.SetCurrentItem(cur)
	}
}

// TransferRow formats one job for the panel. elapsed is passed in rather than
// measured here so the function stays pure, and testable.
func TransferRow(jv jobs.View, elapsed time.Duration) (primary, secondary string) {
	pct := 0.0
	if jv.Total > 0 {
		pct = float64(jv.Done) / float64(jv.Total) * 100
	}
	// Escaped: the list renders tview tags, and "[failed]" is one — an unknown
	// colour name that swallowed the status entirely, so a failed or
	// cancelled transfer looked exactly like a finished one. The description
	// carries bucket and key names, which may hold brackets of their own.
	primary = tview.Escape(fmt.Sprintf("[%s] %s — %s", jv.Status, jv.Kind, jv.Desc))
	// Only a running job has a rate; every other state says what it is
	// rather than claiming "done" for a queued, failed or cancelled one.
	rate := jv.Status.String()
	if jv.Status == jobs.Running {
		rate = flows.ByteRateETA(jv.Done, jv.Total, elapsed)
	}
	secondary = fmt.Sprintf("%d/%d obj • %s / %s (%.0f%%) • %s",
		jv.DoneCount, jv.Count,
		widgets.HumanizeBytes(jv.Done), widgets.HumanizeBytes(jv.Total), pct, rate)
	return primary, secondary
}
