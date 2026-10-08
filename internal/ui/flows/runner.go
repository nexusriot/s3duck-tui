// Package flows is the glue between an ops.Operation and the terminal: the one
// place that knows about both.
//
// Everything tview-shaped that used to be copy-pasted into runDownload,
// runUpload, runCopyOrMove and runSync lives here once — the confirmation, the
// job registration, the Background/Cancel progress modal, the throttle, the
// summary with its Retry and Export buttons. An operation supplies a
// Descriptor and a Result; it never sees a widget.
package flows

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/jobs"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// drawEvery throttles progress redraws. The UI queue is shared with everything
// else that draws; a redraw per callback would flood it on a fast endpoint.
const drawEvery = 100 * time.Millisecond

// Runner drives operations. One per application.
type Runner struct {
	Shell *shell.Shell
	Queue *jobs.Queue

	// Confirm shows a yes/no question and calls onYes if the user agrees. It
	// is supplied by the controller, which owns the confirmation widget.
	Confirm func(text string, onYes func())
	// AskOverwrite renders an ops.Question and delivers the answer. It must
	// not block the UI goroutine: it is called from the operation's goroutine
	// and is expected to push a modal through the shell and wait on a reply.
	AskOverwrite func(path string) ops.Answer
	// Log appends to the activity log.
	Log func(format string, args ...any)
	// Done is called after an operation finishes, on the UI goroutine, so the
	// listing can refresh.
	Done func()
	// ExportFailures writes a failure report and returns where it landed.
	ExportFailures func(name string, failures []ops.Failure) (string, error)
	// CopyText puts the report on the clipboard.
	CopyText func(string)
}

// Start runs op: confirm (if it asks for one), then register a job, show
// progress, and report.
//
// Must be called on the UI goroutine.
func (r *Runner) Start(op ops.Operation) {
	d := op.Describe()
	if d.ConfirmText == "" {
		r.launch(op, d)
		return
	}
	r.Confirm(d.ConfirmText, func() { r.launch(op, d) })
}

func (r *Runner) launch(op ops.Operation, d ops.Descriptor) {
	ctx, cancel := context.WithCancel(context.Background())
	job := r.Queue.Add(string(d.Kind), d.Title, d.TotalBytes, d.TotalItems, cancel)

	var lay *shell.Layer
	progress := tview.NewModal().
		SetText(fmt.Sprintf("Starting %s...\n", d.Kind)).
		AddButtons([]string{"Background", "Cancel"})
	progress.SetDoneFunc(func(_ int, label string) {
		switch label {
		case "Background":
			// Detach the modal; the operation keeps running and reports into
			// the job (watch it in the transfers panel).
			job.SetBackgrounded()
			lay.Close()
		default:
			// Signal only. The workers drain and the summary turns this same
			// modal into the report, so closing the layer here would leave the
			// summary focusing a detached modal.
			cancel()
			progress.SetText("Canceling, please wait...")
		}
	})
	lay = r.Shell.Push(progress)

	rep := &uiReporter{
		runner:   r,
		job:      job,
		layer:    lay,
		modal:    progress,
		desc:     d,
		started:  time.Now(),
		ctxIsOff: ctx.Done(),
	}

	go func() {
		defer cancel()
		res := op.Run(ctx, rep)
		r.Queue.Finish(job, res.Canceled, res.Failed())
		r.Shell.Do(func() {
			if job.IsBackgrounded() || !lay.IsOpen() {
				// A Background press racing completion already took the modal
				// down; mutating a detached one routes keys into an invisible
				// widget.
				r.finished(d)
				return
			}
			r.showSummary(lay, progress, d, res)
		})
	}()
}

// showSummary turns the progress modal into the report. Runs on the UI
// goroutine.
func (r *Runner) showSummary(lay *shell.Layer, modal *tview.Modal, d ops.Descriptor, res ops.Result) {
	report := SummaryText(d, res)

	buttons := []string{"Done", "Copy report"}
	if res.Retry != nil {
		buttons = append(buttons, fmt.Sprintf("Retry failed (%d)", res.Failed()))
	}
	if res.Failed() > 0 {
		buttons = append(buttons, "Export list")
	}

	modal.ClearButtons()
	modal.SetText(report)
	modal.AddButtons(buttons)
	modal.SetDoneFunc(func(_ int, label string) {
		switch {
		case label == "Copy report":
			if r.CopyText != nil {
				r.CopyText(report)
			}
		case strings.HasPrefix(label, "Retry failed"):
			lay.Close()
			// Inline, not in a goroutine: Start builds widgets and must run on
			// the UI goroutine, which this handler already is.
			r.Start(res.Retry)
		case label == "Export list":
			if r.ExportFailures == nil {
				return
			}
			path, err := r.ExportFailures(string(d.Kind), res.Failures)
			if err != nil {
				modal.SetText(report + "\nExport failed: " + err.Error())
				return
			}
			// Keep the report on screen: the user may still want Retry.
			modal.SetText(report + "\nWritten to " + path)
		default:
			lay.Close()
			r.finished(d)
		}
	})
	r.Shell.Focus(modal)
}

// finished runs the post-operation refresh, for the operations that ask for
// one. A download and a remote→local sync do not: re-listing would cost a
// network round trip to show exactly what is already on screen.
func (r *Runner) finished(d ops.Descriptor) {
	if d.RefreshAfter && r.Done != nil {
		r.Done()
	}
}

// SummaryText renders an operation's outcome. Pure, so the arithmetic and the
// wording are testable without a terminal.
func SummaryText(d ops.Descriptor, res ops.Result) string {
	var b strings.Builder
	head := titleCase(string(d.Kind))
	if res.Canceled {
		fmt.Fprintf(&b, "%s canceled.\n\n", head)
	} else if res.Failed() > 0 {
		fmt.Fprintf(&b, "%s finished with errors.\n\n", head)
	} else {
		fmt.Fprintf(&b, "%s complete.\n\n", head)
	}

	fmt.Fprintf(&b, "Done: %d of %d\n", res.Done, d.TotalItems)
	if res.Skipped > 0 {
		fmt.Fprintf(&b, "Skipped: %d\n", res.Skipped)
	}
	if res.Overwritten > 0 {
		fmt.Fprintf(&b, "Overwritten: %d\n", res.Overwritten)
	}
	if res.Failed() > 0 {
		fmt.Fprintf(&b, "Failed: %d\n", res.Failed())
	}
	if res.Objects > 0 {
		// An item can be a whole folder, so "3 of 3 done" alone would hide
		// how much actually moved.
		fmt.Fprintf(&b, "Objects written: %d\n", res.Objects)
	}
	fmt.Fprintf(&b, "Transferred: %s\n", humanize.IBytes(uint64(res.Bytes)))
	if res.Verified > 0 || res.Unverified > 0 {
		fmt.Fprintf(&b, "Verified: %d", res.Verified)
		if res.Unverified > 0 {
			fmt.Fprintf(&b, " (%d not verifiable)", res.Unverified)
		}
		b.WriteString("\n")
	}

	const maxListed = 8
	for i, f := range res.Failures {
		if i == maxListed {
			fmt.Fprintf(&b, "  ...and %d more\n", len(res.Failures)-maxListed)
			break
		}
		fmt.Fprintf(&b, "  - %s\n", f)
	}

	b.WriteString("\nPress Done to return.")
	return b.String()
}

// titleCase upper-cases the first letter. The kinds are ASCII words, so this
// needs none of the machinery strings.Title was deprecated for.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// uiReporter is the bridge an operation reports through. It is the only part of
// this that an operation touches, and it hands back values rather than widgets.
type uiReporter struct {
	runner  *Runner
	job     *jobs.Job
	layer   *shell.Layer
	modal   *tview.Modal
	desc    ops.Descriptor
	started time.Time

	ctxIsOff <-chan struct{}

	mu       sync.Mutex
	lastDraw time.Time
}

func (u *uiReporter) Progress(p ops.Progress) {
	u.job.SetProgress(p.Bytes, p.Items)
	if p.TotalBytes > 0 || p.TotalItems > 0 {
		u.job.SetTotals(p.TotalBytes, p.TotalItems)
	}
	if u.job.IsBackgrounded() {
		return
	}
	select {
	case <-u.ctxIsOff:
		return // cancelling; the modal is showing "Canceling, please wait..."
	default:
	}

	u.mu.Lock()
	if time.Since(u.lastDraw) < drawEvery {
		u.mu.Unlock()
		return
	}
	u.lastDraw = time.Now()
	u.mu.Unlock()

	text := ProgressText(u.desc, p, time.Since(u.started))
	u.runner.Shell.Do(func() { u.modal.SetText(text) })
}

func (u *uiReporter) Ask(q ops.Question) ops.Answer {
	if u.runner.AskOverwrite == nil {
		return ops.AnswerCancel
	}
	return u.runner.AskOverwrite(q.Path)
}

func (u *uiReporter) Log(format string, args ...any) {
	if u.runner.Log != nil {
		u.runner.Log(format, args...)
	}
}

// ProgressText renders one progress sample. Pure, so the percentage and the
// ETA can be tested without a terminal.
func ProgressText(d ops.Descriptor, p ops.Progress, elapsed time.Duration) string {
	if p.Phase != "" {
		return fmt.Sprintf("%s: %s\n%d / %d", d.Kind, p.Phase, p.Items, p.TotalItems)
	}
	active := strings.Join(p.Active, "  ")
	const maxActive = 50
	if r := []rune(active); len(r) > maxActive {
		active = string(r[:maxActive-3]) + "..."
	}
	if active == "" {
		active = "starting..."
	}

	// A server-side operation moves no bytes through this process: all it can
	// report is how many objects the server has acknowledged, so the gauge
	// counts those instead of pretending to a byte rate it cannot measure.
	if p.ByObject {
		return fmt.Sprintf("%s\n%d / %d object(s)\n%s\n\n%s",
			titleCase(string(d.Kind)),
			p.Items, p.TotalItems,
			ObjRateETA(p.Items, p.TotalItems, elapsed),
			active)
	}

	pct := 0.0
	if p.TotalBytes > 0 {
		pct = float64(p.Bytes) / float64(p.TotalBytes) * 100
	}
	return fmt.Sprintf("%s\n%d / %d done\n%s / %s (%.1f%%)\n%s\n\n%s",
		titleCase(string(d.Kind)),
		p.Items, p.TotalItems,
		humanize.IBytes(uint64(p.Bytes)), humanize.IBytes(uint64(p.TotalBytes)), pct,
		ByteRateETA(p.Bytes, p.TotalBytes, elapsed),
		active)
}
