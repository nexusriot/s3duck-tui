// Package shell owns every overlay the app puts on screen and every update it
// makes to a tview widget from a background goroutine.
//
// It exists to retire two whole classes of bug that the hand-rolled page
// handling could not avoid:
//
//   - Overlays were addressed by magic string. tview's Pages.AddPage REPLACES a
//     page of the same name, so two flows that both used "progress" (there were
//     twelve) could destroy each other's modal, and a RemovePage issued by the
//     wrong flow left the survivor focused on a detached primitive — a soft
//     lock. Here an overlay is a *Layer: Push hands back a handle with a name
//     nobody else can hold, and Close pops exactly that layer and refocuses
//     whatever was underneath.
//
//   - Updating a widget from a background goroutine meant App.QueueUpdateDraw,
//     which in this tview version BLOCKS until the event loop runs the closure.
//     Calling it from the UI goroutine therefore deadlocks the app outright, so
//     every caller had to know which goroutine it was on (`go c.error(...)` from
//     a button handler, bare `c.error(...)` from a worker). Shell.Do is safe
//     from anywhere: it appends to a pending list and returns, and a pump
//     goroutine — never the UI goroutine, so QueueUpdateDraw can always
//     complete — forwards batches to tview.
//
// Everything that mutates a widget off the UI goroutine goes through Do, so the
// single FIFO also preserves the ordering that the old blocking calls got for
// free.
package shell

import (
	"fmt"
	"sync"

	"github.com/rivo/tview"
)

// Shell is the overlay stack and the UI update queue for one application.
type Shell struct {
	app   *tview.Application
	pages *tview.Pages

	// wake signals the pump that pending has work. Buffered by one: a signal
	// already in flight covers any number of appends, because the pump drains
	// pending to empty before it waits again.
	wake chan struct{}
	stop chan struct{}

	mu      sync.Mutex
	pending []func()
	stack   []*Layer
	seq     int
	// toast is the transient message layer, kept apart from the stack so a new
	// toast replaces the old one instead of burying it. This is what the old
	// dedicated "modal-msg" page name did.
	toast *Layer

	// baseFocus returns what to focus when the last layer closes. Set by the
	// controller, which is the only thing that knows which pane is active.
	baseFocus func() tview.Primitive
}

// New builds a Shell over an application's page container. The caller must call
// Start before any Do reaches the screen.
func New(app *tview.Application, pages *tview.Pages) *Shell {
	return &Shell{
		app:   app,
		pages: pages,
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
	}
}

// SetBaseFocus installs the fallback focus target used when the overlay stack
// empties. A nil return from f means "leave focus alone".
func (s *Shell) SetBaseFocus(f func() tview.Primitive) { s.baseFocus = f }

// Start launches the pump. Safe to call before App.Run: queued work simply
// waits for the event loop to exist.
func (s *Shell) Start() { go s.pump() }

// Stop retires the pump. The process normally exits instead of calling this;
// it exists so tests can shut a Shell down.
func (s *Shell) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// Do runs f on the UI goroutine and redraws. It never blocks and is safe from
// any goroutine, including the UI goroutine itself — which is the entire point:
// callers no longer have to know where they are running.
//
// f runs later, so it must not be used to compute a value the caller then reads.
// Pass results back over a channel instead (see flows' question reporter).
func (s *Shell) Do(f func()) {
	if f == nil {
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, f)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default: // a wake-up is already pending; the pump drains to empty
	}
}

// pump forwards pending work to tview in batches. It runs on its own goroutine
// for the whole life of the app, which is what makes Do's no-block guarantee
// hold: QueueUpdateDraw waits for the event loop, and the event loop is never
// this goroutine.
func (s *Shell) pump() {
	for {
		select {
		case <-s.wake:
		case <-s.stop:
			return
		}
		for {
			s.mu.Lock()
			batch := s.pending
			s.pending = nil
			s.mu.Unlock()
			if len(batch) == 0 {
				break
			}
			// One draw per batch rather than per closure: a burst of progress
			// updates coalesces instead of queueing a redraw each.
			s.app.QueueUpdateDraw(func() {
				for _, f := range batch {
					f()
				}
			})
		}
	}
}

// Layer is one overlay on the stack. The name is allocated by Push and is not
// reused, so no other flow can address — or replace — this overlay.
type Layer struct {
	s    *Shell
	name string
	prim tview.Primitive
	// focus is what to give focus to while this layer is on top. Usually the
	// layer's own primitive; a form inside a flex may want an inner widget.
	focus tview.Primitive
	open  bool
}

// Name is the tview page name backing this layer. Only needed by code that has
// to reach tview directly; prefer the Layer methods.
func (l *Layer) Name() string { return l.name }

// Primitive is the overlay this layer shows.
func (l *Layer) Primitive() tview.Primitive { return l.prim }

// Close pops this layer and refocuses whatever is beneath it. Closing an
// already-closed layer is a no-op, so a flow that can finish down two paths
// (a Done button and an Esc capture) does not have to coordinate.
//
// UI goroutine only. From elsewhere: shell.Do(layer.Close).
func (l *Layer) Close() {
	if l == nil {
		return
	}
	l.s.closeLayer(l)
}

// Push puts p on top of the overlay stack and focuses it. Returns the handle
// used to take it down again.
//
// UI goroutine only. From a background goroutine, wrap it:
//
//	shell.Do(func() { h := shell.Push(p); ... })
func (s *Shell) Push(p tview.Primitive) *Layer { return s.PushFocus(p, p) }

// PushFocus is Push for an overlay whose focus target is not the primitive
// itself — a centred modal wrapped in flexes, say, where focus belongs on the
// form inside.
func (s *Shell) PushFocus(p, focus tview.Primitive) *Layer {
	s.mu.Lock()
	s.seq++
	l := &Layer{s: s, name: fmt.Sprintf("layer-%d", s.seq), prim: p, focus: focus, open: true}
	s.stack = append(s.stack, l)
	s.mu.Unlock()

	// AddPage hands focus to the topmost visible page only when Pages already
	// has it. Focus stranded on a widget that left the layout (a collapsed
	// pane, say) would leave the new overlay on screen but deaf to every key,
	// so the overlay is always focused explicitly.
	s.pages.AddPage(l.name, p, true, true)
	if focus != nil {
		s.app.SetFocus(focus)
	}
	return l
}

// Toast shows a transient message, replacing any toast already up. Unlike Push
// it is safe from any goroutine, because reporting an error is exactly the case
// where the caller should not have to think about where it is running.
//
// The returned value is nil: a toast is dismissed by its own button.
func (s *Shell) Toast(p tview.Primitive) {
	s.Do(func() {
		s.mu.Lock()
		prev := s.toast
		s.toast = nil
		s.mu.Unlock()
		if prev != nil {
			s.closeLayer(prev)
		}
		l := s.Push(p)
		s.mu.Lock()
		s.toast = l
		s.mu.Unlock()
	})
}

// CloseToast takes down the current toast, if any. Used by toast buttons.
func (s *Shell) CloseToast() {
	s.mu.Lock()
	l := s.toast
	s.toast = nil
	s.mu.Unlock()
	if l != nil {
		s.closeLayer(l)
	}
}

// closeLayer removes a layer and restores focus to the new top of the stack
// (or the base focus when the stack empties). Removing a layer that is not on
// top is allowed — flows do finish out of order — and leaves the layers above
// it untouched.
func (s *Shell) closeLayer(l *Layer) {
	s.mu.Lock()
	if !l.open {
		s.mu.Unlock()
		return
	}
	l.open = false
	for i, cur := range s.stack {
		if cur == l {
			s.stack = append(s.stack[:i], s.stack[i+1:]...)
			break
		}
	}
	if s.toast == l {
		s.toast = nil
	}
	var top *Layer
	if n := len(s.stack); n > 0 {
		top = s.stack[n-1]
	}
	s.mu.Unlock()

	// RemovePage re-focuses the last visible page by itself, so a layer under
	// this one is picked up without help. The base screen is the exception:
	// "main" holds both panes, and tview would focus the first rather than the
	// active one.
	s.pages.RemovePage(l.name)

	switch {
	case top != nil && top.focus != nil && top.focus != top.prim:
		s.app.SetFocus(top.focus)
	case top == nil && s.baseFocus != nil:
		if p := s.baseFocus(); p != nil {
			s.app.SetFocus(p)
		}
	}
}

// IsOpen reports whether this layer is still on the stack. A nil layer counts
// as closed, so a flow can test one it never opened.
func (l *Layer) IsOpen() bool {
	if l == nil {
		return false
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	return l.open
}

// Focus gives keyboard focus to p. UI goroutine only — from elsewhere, wrap
// it in Do. It exists so callers do not need the *tview.Application to move
// focus within an overlay they already own.
func (s *Shell) Focus(p tview.Primitive) { s.app.SetFocus(p) }

// Depth is the number of open overlays. Input handlers use it to tell "Esc
// closes an overlay" from "Esc acts on the list".
func (s *Shell) Depth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stack)
}
