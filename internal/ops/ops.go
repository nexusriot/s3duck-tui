// Package ops holds the app's long-running operations — download, upload, copy,
// move, sync — as values that can be run, reported on, and tested.
//
// # Why this package exists
//
// These flows used to live in the controller as four near-copies of one
// pipeline (runDownload at 449 lines, runCrossCopy at 226, runSync at 203,
// runUpload at 124), each interleaving three unrelated jobs: deciding what to
// transfer, driving tview's modal and page machinery, and bookkeeping the
// result. DESIGN.md justified that on the grounds that "splitting them would
// mean passing around opaque page/modal handles".
//
// The premise was right and the conclusion was not. Handles had to be passed
// because an operation asked the user questions by calling into tview
// directly. Invert that — let the operation *emit* a Question and block on the
// answer — and it needs no handle, no *tview.Modal and no *tview.Application.
// One tview-aware runner renders every question for every operation.
//
// So an Operation here depends on nothing but narrow S3 ports (see ports.go)
// and a Reporter. That makes the interesting decisions — retry, skip,
// overwrite, partial failure, cancellation draining, worker-pool barriers —
// testable with a fake store in milliseconds, which is the part the integration
// suite could only reach with a live endpoint.
package ops

import (
	"context"
	"fmt"
	"sync"
)

// Kind is an operation's category, used for the job label and the activity log.
type Kind string

const (
	KindDownload Kind = "download"
	KindUpload   Kind = "upload"
	KindCopy     Kind = "copy"
	KindMove     Kind = "move"
	KindSync     Kind = "sync"
)

// Descriptor is everything the UI needs to announce an operation before it
// starts: what to call it, what to confirm, and how big it is.
type Descriptor struct {
	Kind Kind
	// Title is the one-line job label ("5 obj → /home/u/dl").
	Title string
	// ConfirmText is the body of the pre-flight confirmation. Empty means the
	// operation needs no confirmation.
	ConfirmText string
	TotalBytes  int64
	TotalItems  int
	// RefreshAfter asks the driver to re-list the browser when this finishes.
	// False for an operation that only writes locally — a download, or a
	// remote→local sync — where re-listing costs a network round trip and can
	// change nothing the user is looking at.
	RefreshAfter bool
}

// Progress is one sample of an operation's advance. Active names the units
// currently in flight, which is what makes a parallel transfer legible.
type Progress struct {
	Bytes, TotalBytes int64
	Items, TotalItems int
	Active            []string
	// Phase labels what is happening when it is not plain byte movement
	// ("resolving", "scanning"). Empty during the transfer itself.
	Phase string
	// ByObject says the Items counters measure objects rather than bytes, so
	// the renderer shows an object rate instead of a byte rate. Set by
	// server-side operations, which move no bytes through this process and
	// have nothing to report but the server's acknowledgements.
	ByObject bool
}

// QKind is the sort of decision an operation needs from the user.
type QKind int

const (
	// QOverwrite asks about one existing destination file.
	QOverwrite QKind = iota
)

// Question is asked by an operation and answered by whoever is driving it. The
// operation never holds a widget: it hands over a value and waits.
type Question struct {
	Kind QKind
	// Path is what the question is about (the destination that already exists).
	Path string
}

// Answer is a reply to a Question. The ToAll variants apply to every remaining
// unit, so the operation stops asking.
type Answer int

const (
	AnswerYes Answer = iota
	AnswerNo
	AnswerYesToAll
	AnswerNoToAll
	AnswerCancel
)

// Reporter is how an operation talks to whatever is driving it. The UI
// implements it over the shell; tests implement it to record progress and to
// script answers.
//
// Progress may be called very often and from several goroutines at once;
// implementations are expected to throttle rather than make callers do it.
type Reporter interface {
	Progress(Progress)
	// Ask blocks until the user answers. An implementation that cannot ask
	// (a headless retry, say) must still return something.
	Ask(Question) Answer
	Log(format string, args ...any)
}

// Failure is one unit that did not make it, kept with enough identity to retry
// just that unit.
type Failure struct {
	// Label identifies the unit in the report (a key, or a local path).
	Label string
	Err   error
}

func (f Failure) String() string { return fmt.Sprintf("%s: %v", f.Label, f.Err) }

// Result is an operation's outcome. The counts are the headline; Failures is
// the complete ledger (the report text caps what it prints, but a retry has to
// know about every one).
type Result struct {
	Done        int
	Skipped     int
	Overwritten int
	Bytes       int64
	// Objects counts units written beneath the item level — a folder item is
	// many objects. 0 when an operation's items and its units are the same
	// thing, which is the usual case.
	Objects  int
	Failures []Failure
	Canceled bool

	// Verified / Unverified count post-transfer checksum comparisons: how many
	// units were confirmed byte-for-byte, and how many could not be (a
	// multipart object with no whole-object checksum).
	Verified, Unverified int

	// Retry, when non-nil, re-runs exactly the units that failed.
	Retry Operation
}

// Failed is how many units failed.
func (r Result) Failed() int { return len(r.Failures) }

// ledger accumulates a Result under a lock, so parallel workers report into one
// place and no operation has to re-invent the bookkeeping.
type ledger struct {
	mu  sync.Mutex
	res Result
}

func (l *ledger) ok(bytes int64, overwrote bool) {
	l.mu.Lock()
	l.res.Done++
	l.res.Bytes += bytes
	if overwrote {
		l.res.Overwritten++
	}
	l.mu.Unlock()
}

func (l *ledger) addObjects(n int) {
	l.mu.Lock()
	l.res.Objects += n
	l.mu.Unlock()
}

func (l *ledger) skip() {
	l.mu.Lock()
	l.res.Skipped++
	l.mu.Unlock()
}

func (l *ledger) fail(label string, err error) {
	l.mu.Lock()
	if label == "" {
		label = "<nil-key>"
	}
	l.res.Failures = append(l.res.Failures, Failure{Label: label, Err: err})
	l.mu.Unlock()
}

func (l *ledger) countVerified(ok bool) {
	l.mu.Lock()
	if ok {
		l.res.Verified++
	} else {
		l.res.Unverified++
	}
	l.mu.Unlock()
}

func (l *ledger) cancel() {
	l.mu.Lock()
	l.res.Canceled = true
	l.mu.Unlock()
}

func (l *ledger) result() Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.res
}

// Operation is one long-running job. Describe is called on the UI goroutine
// before anything starts; Run is called on a goroutine of the driver's choosing
// and must honour ctx.
type Operation interface {
	Describe() Descriptor
	Run(ctx context.Context, rep Reporter) Result
}
