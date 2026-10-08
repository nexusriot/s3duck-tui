package shell

import (
	"sync"
	"testing"

	"github.com/rivo/tview"
)

// newTestShell builds a Shell over a real tview.Pages but no running
// application. Pages and focus work without a screen; the pump is not started,
// so Do's queue can be drained by hand (drain below) without an event loop.
func newTestShell() (*Shell, *tview.Pages) {
	pages := tview.NewPages().AddPage("main", tview.NewBox(), true, true)
	return New(tview.NewApplication(), pages), pages
}

// drain runs whatever Do has queued, the way the pump would.
func (s *Shell) drain() {
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, f := range batch {
		f()
	}
}

func TestPushAllocatesDistinctNames(t *testing.T) {
	s, pages := newTestShell()

	a := s.Push(tview.NewBox())
	b := s.Push(tview.NewBox())

	if a.Name() == b.Name() {
		t.Fatalf("two layers share the page name %q; a flow could replace another's overlay", a.Name())
	}
	// Both overlays plus "main".
	if got := pages.GetPageCount(); got != 3 {
		t.Fatalf("page count = %d, want 3 (main + two layers)", got)
	}
	if s.Depth() != 2 {
		t.Fatalf("Depth() = %d, want 2", s.Depth())
	}
}

func TestCloseRemovesOnlyItsOwnLayer(t *testing.T) {
	s, pages := newTestShell()

	under := s.Push(tview.NewBox())
	over := s.Push(tview.NewBox())

	over.Close()

	if !under.IsOpen() {
		t.Fatal("closing the top layer also closed the one beneath it")
	}
	if pages.HasPage(over.Name()) {
		t.Fatal("the closed layer's page is still registered")
	}
	if !pages.HasPage(under.Name()) {
		t.Fatal("the surviving layer's page was removed")
	}
	if s.Depth() != 1 {
		t.Fatalf("Depth() = %d, want 1", s.Depth())
	}
}

// A flow that can finish down two paths (a Done button and an Esc capture)
// closes its layer twice; the second must not take down whatever opened in
// between. This is the soft-lock the hand-rolled RemovePage calls could cause.
func TestCloseIsIdempotentAndDoesNotTakeTheLayerAbove(t *testing.T) {
	s, _ := newTestShell()

	first := s.Push(tview.NewBox())
	first.Close()
	second := s.Push(tview.NewBox())

	first.Close() // the stale handle fires again

	if !second.IsOpen() {
		t.Fatal("a second Close on a stale handle closed an unrelated layer")
	}
	if s.Depth() != 1 {
		t.Fatalf("Depth() = %d, want 1", s.Depth())
	}
}

// Closing out of order happens whenever a background flow finishes while the
// user has opened something else on top of it.
func TestCloseOutOfOrderKeepsTheLayerAbove(t *testing.T) {
	s, pages := newTestShell()

	bottom := s.Push(tview.NewBox())
	top := s.Push(tview.NewBox())

	bottom.Close()

	if !top.IsOpen() || !pages.HasPage(top.Name()) {
		t.Fatal("closing a buried layer removed the one above it")
	}
	if s.Depth() != 1 {
		t.Fatalf("Depth() = %d, want 1", s.Depth())
	}
}

func TestBaseFocusAppliedOnlyWhenTheStackEmpties(t *testing.T) {
	s, _ := newTestShell()
	base := tview.NewBox()
	calls := 0
	s.SetBaseFocus(func() tview.Primitive { calls++; return base })

	a := s.Push(tview.NewBox())
	b := s.Push(tview.NewBox())

	b.Close()
	if calls != 0 {
		t.Fatalf("base focus restored while %d layer(s) were still open", s.Depth())
	}
	a.Close()
	if calls != 1 {
		t.Fatalf("base focus called %d times after the stack emptied, want 1", calls)
	}
}

// A toast replaces the previous one rather than burying it — what the single
// dedicated "modal-msg" page name used to guarantee.
func TestToastReplacesThePreviousToast(t *testing.T) {
	s, _ := newTestShell()

	s.Toast(tview.NewBox())
	s.drain()
	if s.Depth() != 1 {
		t.Fatalf("Depth() = %d after one toast, want 1", s.Depth())
	}

	s.Toast(tview.NewBox())
	s.drain()
	if s.Depth() != 1 {
		t.Fatalf("Depth() = %d after a second toast, want 1 (it should replace, not stack)", s.Depth())
	}

	s.CloseToast()
	if s.Depth() != 0 {
		t.Fatalf("Depth() = %d after CloseToast, want 0", s.Depth())
	}
}

func TestToastDoesNotDisturbAnOpenForm(t *testing.T) {
	s, _ := newTestShell()

	form := s.Push(tview.NewBox())
	s.Toast(tview.NewBox())
	s.drain()

	if !form.IsOpen() {
		t.Fatal("an asynchronous toast destroyed the open form — the bug that split modal-msg out of modal")
	}
}

// Do must never block, from any goroutine, and must preserve the order work
// was submitted in. The old QueueUpdateDraw blocked on the event loop, which
// deadlocked outright when called from the UI goroutine.
func TestDoQueuesInOrderWithoutBlocking(t *testing.T) {
	s, _ := newTestShell()

	var got []int
	for i := range 5 {
		s.Do(func() { got = append(got, i) })
	}
	s.drain()

	if len(got) != 5 {
		t.Fatalf("ran %d closures, want 5", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("order = %v, want ascending: closure %d ran at position %d", got, v, i)
		}
	}
}

func TestDoIsSafeFromManyGoroutines(t *testing.T) {
	s, _ := newTestShell()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			s.Do(func() {})
		}()
	}
	wg.Wait() // would hang if Do blocked on an event loop that is not running

	s.mu.Lock()
	queued := len(s.pending)
	s.mu.Unlock()
	if queued != n {
		t.Fatalf("queued %d closures, want %d", queued, n)
	}
}

func TestDoIgnoresNil(t *testing.T) {
	s, _ := newTestShell()
	s.Do(nil)
	s.drain() // must not panic
}

func TestNilLayerCloseIsSafe(t *testing.T) {
	var l *Layer
	l.Close() // flows close a layer they may never have opened
	if l.IsOpen() {
		t.Fatal("a nil layer reports itself open")
	}
}
