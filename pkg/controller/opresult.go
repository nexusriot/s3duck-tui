package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rivo/tview"
)

// reportRows is how many failures the report names inline. Past this the
// export is the honest way to see them all.
const reportRows = 8

// retryItem is one failed unit of work: a label for the report and the
// flow's own descriptor, opaque here and type-asserted back by the retry
// callback that owns it.
type retryItem struct {
	label  string
	target any
}

type opFailure struct {
	item retryItem
	err  error
}

// opResult accumulates the outcome of one multi-item operation.
type opResult struct {
	name     string
	okCount  int
	bytes    int64
	failures []opFailure
	// skipped counts units the user chose not to do (skip-existing), which is
	// not a failure and must not be reported as one.
	skipped int
	// note is one extra line for a flow whose unit count needs explaining —
	// a copy reports items done *and* objects written, which are different
	// numbers whenever a folder is involved.
	note string
}

func newOpResult(name string) *opResult { return &opResult{name: name} }

func (r *opResult) ok() { r.okCount++ }

func (r *opResult) okBytes(n int64) {
	r.okCount++
	r.bytes += n
}

func (r *opResult) skip() { r.skipped++ }

func (r *opResult) fail(it retryItem, err error) {
	r.failures = append(r.failures, opFailure{item: it, err: err})
}

// failed reports how many units failed.
func (r *opResult) failed() int { return len(r.failures) }

// retryItems returns the descriptors of the failed units, for a re-run.
func (r *opResult) retryItems() []retryItem {
	out := make([]retryItem, 0, len(r.failures))
	for _, f := range r.failures {
		out = append(out, f.item)
	}
	return out
}

// exportLines is the full failure list, one per line — what the export writes.
func (r *opResult) exportLines() []string {
	out := make([]string, 0, len(r.failures))
	for _, f := range r.failures {
		out = append(out, fmt.Sprintf("%s\t%v", f.item.label, f.err))
	}
	return out
}

// text renders the report. Pure, so the wording of the case that matters most
// — a partial failure — is testable without a terminal.
func (r *opResult) text(canceled bool) string {
	status := r.name + " complete."
	switch {
	case canceled:
		status = r.name + " canceled."
	case len(r.failures) > 0:
		status = r.name + " finished with errors."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", status)
	fmt.Fprintf(&b, "Done: %d\n", r.okCount)
	if r.skipped > 0 {
		fmt.Fprintf(&b, "Skipped: %d\n", r.skipped)
	}
	fmt.Fprintf(&b, "Failed: %d\n", len(r.failures))
	if r.bytes > 0 {
		fmt.Fprintf(&b, "Transferred: %s\n", humanize.IBytes(uint64(r.bytes)))
	}
	if r.note != "" {
		fmt.Fprintf(&b, "%s\n", r.note)
	}

	if len(r.failures) > 0 {
		b.WriteString("\nFailed:\n")
		for i, f := range r.failures {
			if i == reportRows {
				fmt.Fprintf(&b, "  ...and %d more (use Export list)\n", len(r.failures)-reportRows)
				break
			}
			fmt.Fprintf(&b, "  - %s: %v\n", f.item.label, f.err)
		}
	}
	return b.String()
}

// failureFileName is the export's name. Time is passed in rather than read so
// the naming is testable.
func failureFileName(name string, now time.Time) string {
	slug := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	return fmt.Sprintf("s3duck-%s-failures-%s.txt", slug, now.Format("20060102-150405"))
}

// writeFailureReport writes the failure list into dir and returns the path.
func (r *opResult) writeFailureReport(dir string, now time.Time) (string, error) {
	if len(r.failures) == 0 {
		return "", fmt.Errorf("nothing to export")
	}
	path := filepath.Join(dir, failureFileName(r.name, now))
	body := strings.Join(r.exportLines(), "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return "", err
	}
	return path, nil
}

// reportOpResult turns the progress modal into the final report: the text,
// a Done button, and — when something failed — Retry failed and Export list.
// retry may be nil for flows where re-running a single unit makes no sense.
//
// Called from a transfer goroutine, so all widget work goes through the UI
// queue; the buttons' handlers then run on the UI goroutine, which is why the
// retry callback is invoked in a goroutine of its own.
func (c *Controller) reportOpResult(progress *tview.Modal, res *opResult, canceled bool, retry func([]retryItem)) {
	c.view.App.QueueUpdateDraw(func() {
		buttons := []string{"Done"}
		if res.failed() > 0 {
			if retry != nil {
				buttons = append(buttons, fmt.Sprintf("Retry failed (%d)", res.failed()))
			}
			buttons = append(buttons, "Export list")
		}
		progress.SetText(res.text(canceled))
		progress.ClearButtons()
		progress.AddButtons(buttons)
		progress.SetDoneFunc(func(_ int, label string) {
			switch {
			case strings.HasPrefix(label, "Retry failed"):
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
				items := res.retryItems()
				go retry(items)
			case label == "Export list":
				dir := c.resolveDownloadDir()
				path, err := res.writeFailureReport(dir, time.Now())
				if err != nil {
					go c.error("Export failed", err)
					return
				}
				// Keep the report on screen: the user may still want Retry.
				progress.SetText(res.text(canceled) + "\nWritten to " + path)
			default:
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			}
		})
		c.view.App.SetFocus(progress)
	})
}
