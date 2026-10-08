package session

import "github.com/nexusriot/s3duck-tui/internal/model"

// HistMax bounds each stack. A hundred steps is far more than anyone walks
// back through, and it keeps a long session from holding every prefix it ever
// visited.
const HistMax = 100

// Visit is one position in the navigation history: a bucket (nil = the buckets
// screen) and a prefix within it.
type Visit struct {
	Bucket *model.Object
	Path   string
}

// History is a browser-style back/forward stack. The "current" position lives
// on the pane; these stacks hold where you came from (back) and where you
// returned from (fwd).
//
// Touched only on the UI goroutine, so it carries no lock of its own.
type History struct {
	back []Visit
	fwd  []Visit
}

// Record pushes cur onto the back stack and starts a new branch (clears fwd).
func (h *History) Record(cur Visit) {
	h.back = append(h.back, cur)
	if len(h.back) > HistMax {
		h.back = h.back[len(h.back)-HistMax:]
	}
	h.fwd = nil
}

// Back returns the previous position (and true), moving cur onto the forward
// stack. ok is false when there is nowhere to go back to.
func (h *History) Back(cur Visit) (Visit, bool) {
	if len(h.back) == 0 {
		return Visit{}, false
	}
	prev := h.back[len(h.back)-1]
	h.back = h.back[:len(h.back)-1]
	h.fwd = append(h.fwd, cur)
	return prev, true
}

// Forward is Back's inverse.
func (h *History) Forward(cur Visit) (Visit, bool) {
	if len(h.fwd) == 0 {
		return Visit{}, false
	}
	next := h.fwd[len(h.fwd)-1]
	h.fwd = h.fwd[:len(h.fwd)-1]
	h.back = append(h.back, cur)
	return next, true
}

// Depths reports how many steps are available in each direction. Used by
// tests and by anything that wants to grey out a direction.
func (h *History) Depths() (back, fwd int) { return len(h.back), len(h.fwd) }
