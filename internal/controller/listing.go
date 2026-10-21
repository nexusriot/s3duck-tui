package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// listCap bounds one prefix level. Listings stream — each page enters the
// object map as it arrives, the count is shown live and Esc cancels — and a
// cancelled or capped listing keeps what arrived, saying so in the title: a
// browsable first slice of a huge prefix beats an error. The cap exists
// because a tview.List holding a hundred thousand rows renders slower than
// the listing took to fetch.
const listCap = 20000

// listProgressEvery throttles the "listing… N" title updates. The UI queue is
// shared with everything else that draws; a redraw per page would flood it on
// a fast endpoint.
const listProgressEvery = 250 * time.Millisecond

// listingState tracks the in-flight listing so Esc can cancel it. One at a
// time is enough: updateList already serialises on refreshMu.
type listingState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	// gen identifies the registered listing. end quotes the generation it was
	// handed, so a listing that finishes late cannot retire — or cancel — the
	// one that has registered since.
	gen uint64
}

// begin registers a new listing, cancelling any predecessor that is somehow
// still running, and returns its context plus the token end must quote.
func (l *listingState) begin() (context.Context, uint64) {
	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
	}
	l.gen++
	gen := l.gen
	l.cancel = cancel
	l.mu.Unlock()
	return ctx, gen
}

// end retires the listing identified by gen, releasing its context. A stale
// token is a no-op: clearing l.cancel unconditionally used to drop a newer
// listing's registration, leaving Esc with nothing to cancel.
func (l *listingState) end(gen uint64) {
	l.mu.Lock()
	if l.gen == gen {
		if l.cancel != nil {
			l.cancel()
		}
		l.cancel = nil
	}
	l.mu.Unlock()
}

// stop cancels the in-flight listing, if any, and reports whether there was
// one — so the Esc handler knows whether it consumed the key.
func (l *listingState) stop() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel == nil {
		return false
	}
	l.cancel()
	return true
}

// CancelListing stops an in-flight navigation listing. Bound to Esc, which
// otherwise does nothing in the browser.
func (c *Controller) CancelListing() bool { return c.listing.stop() }

// makeObjectMap fetches the current location into the object map, streaming
// page by page. It reports whether the listing is *partial* — cancelled by the
// user or stopped at listCap — which the title then discloses.
//
// A cancelled listing is deliberately not an error: whatever arrived is kept
// and rendered. The alternative (an error and an empty pane) throws away work
// the user already waited for.
//
// pane is the pane the listing was started for, captured by the caller before
// the fetch: a Tab during a slow listing must not store one pane's objects in
// the other, where a delete would resolve them against the wrong bucket.
func (c *Controller) makeObjectMap(ctx context.Context, pane *session.Pane, onProgress func(n int)) (partial bool, err error) {
	dirs := make(map[string]*model.Object)

	bucket, prefix := pane.Bucket(), pane.Path()
	if bucket == nil {
		list, err := c.model.ListBuckets(ctx)
		if err != nil {
			return false, err
		}
		for _, obj := range list {
			dirs[objKey(obj)] = obj
		}
		pane.SetObjs(dirs)
		return false, nil
	}

	capped := false
	err = c.model.ListStream(ctx, prefix, bucket, func(page []*model.Object, total int) bool {
		for _, obj := range page {
			dirs[objKey(obj)] = obj
		}
		if onProgress != nil {
			onProgress(len(dirs))
		}
		if total >= listCap {
			capped = true
			return false
		}
		return true
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			pane.SetObjs(dirs)
			return true, nil
		}
		return false, err
	}
	pane.SetObjs(dirs)
	return capped, nil
}

// listProgressReporter builds the throttled progress callback. It writes to
// the list widget captured at call time rather than to c.view.List: a Tab
// mid-listing swaps the active pane, and the count belongs to the pane the
// listing was started for.
func (c *Controller) listProgressReporter(target *tview.List, base string) func(int) {
	var last time.Time
	return func(n int) {
		if time.Since(last) < listProgressEvery {
			return
		}
		last = time.Now()
		c.shell.Do(func() {
			target.SetTitle(fmt.Sprintf("%s  [yellow]listing… %s", base, humanCount(n)))
		})
	}
}

// humanCount groups a count with thin separators, so "127000" reads as a
// number rather than as noise while it ticks upward.
func humanCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, ch := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, ch)
	}
	return string(out)
}

// partialNote is the title suffix disclosing an incomplete listing. Empty for
// a complete one.
func partialNote(partial bool, shown int) string {
	if !partial {
		return ""
	}
	if shown >= listCap {
		return fmt.Sprintf("  [red]partial: first %s (Ctrl+F to search the rest)", humanCount(shown))
	}
	return fmt.Sprintf("  [red]partial: %s listed (r to retry)", humanCount(shown))
}

// wait puts an uncancellable "please hold" modal on screen and returns its
// handle. For anything that can take long enough to want an Esc, use
// cancellableWait instead.
//
// Must be called on the UI goroutine.
func (c *Controller) wait(text string) *shell.Layer {
	return c.pushRaw(tview.NewModal().SetText(text))
}

// cancellableWait puts a wait modal on screen with a Cancel button and returns
// its layer handle, the modal itself (callers update its text), and a context
// the button cancels. Closing the layer is the caller's job — and it is now the
// only way to take this particular overlay down, so one flow's Close cannot
// remove another's wait modal the way a shared "progress" page name did.
//
// Must be called on the UI goroutine.
func (c *Controller) cancellableWait(text string) (*shell.Layer, *tview.Modal, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	m := tview.NewModal().SetText(text).AddButtons([]string{"Cancel"})
	m.SetDoneFunc(func(_ int, _ string) { cancel() })
	// Esc should cancel too — a wait modal with a button the user has to
	// tab to is the kind of thing that gets stabbed at with Esc first.
	m.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			cancel()
			return nil
		}
		return ev
	})
	lay := c.pushRaw(m)
	c.view.App.SetFocus(m)
	return lay, m, ctx, cancel
}
