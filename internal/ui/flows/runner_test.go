package flows

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

func newTestRunner() (*Runner, *tview.Application) {
	app := tview.NewApplication()
	pages := tview.NewPages().AddPage("main", tview.NewBox(), true, true)
	return &Runner{Shell: shell.New(app, pages)}, app
}

// A job finishing under another dialog (an overwrite question, an error
// toast) must not pull focus to its own, hidden summary: the user's next
// Enter would press the summary's Done instead of answering what they see.
func TestSummaryDoesNotStealFocusFromAnOverlayAbove(t *testing.T) {
	r, app := newTestRunner()
	progress := tview.NewModal()
	lay := r.Shell.Push(progress)
	question := tview.NewModal().SetText("Overwrite?")
	r.Shell.Push(question)

	r.showSummary(lay, progress, ops.Descriptor{Kind: "download"}, ops.Result{Done: 1})
	if !question.HasFocus() || progress.HasFocus() {
		t.Errorf("focus moved off the question on top (now on %T)", app.GetFocus())
	}
}

// With nothing above it, the summary does take focus.
func TestSummaryTakesFocusWhenOnTop(t *testing.T) {
	r, app := newTestRunner()
	progress := tview.NewModal()
	lay := r.Shell.Push(progress)
	app.SetFocus(tview.NewBox())

	r.showSummary(lay, progress, ops.Descriptor{Kind: "download"}, ops.Result{Done: 1})
	if !progress.HasFocus() {
		t.Errorf("focus is on %T, want the summary", app.GetFocus())
	}
}

// Keys in the report are shown as written: a bracketed segment is part of the
// name, not a colour tag.
func TestSummaryEscapesFailedKeys(t *testing.T) {
	r, _ := newTestRunner()
	progress := tview.NewModal()
	lay := r.Shell.Push(progress)
	res := ops.Result{Failures: []ops.Failure{{Label: "logs/[draft]/x.txt", Err: errString("denied")}}}
	r.showSummary(lay, progress, ops.Descriptor{Kind: "download"}, res)

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(100, 20)
	progress.SetRect(0, 0, 100, 20)
	progress.Draw(screen)
	screen.Show()
	cells, w, h := screen.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if rs := cells[y*w+x].Runes; len(rs) > 0 {
				b.WriteRune(rs[0])
			}
		}
		b.WriteByte('\n')
	}
	if !strings.Contains(b.String(), "logs/[draft]/x.txt") {
		t.Errorf("summary rendered:\n%s", b.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// After Cancel (the progress modal's second button) the summary's focused
// button is Done, not whatever sits second: Enter must close the report, not
// copy it.
func TestSummaryFocusesDone(t *testing.T) {
	r, app := newTestRunner()
	progress := tview.NewModal().AddButtons([]string{"Background", "Cancel"})
	lay := r.Shell.Push(progress)
	progress.SetFocus(1) // the user pressed Cancel
	app.SetFocus(progress)

	copied := false
	r.CopyText = func(string) error { copied = true; return nil }
	r.showSummary(lay, progress, ops.Descriptor{Kind: "download"}, ops.Result{Canceled: true})

	handler := app.GetFocus().InputHandler()
	handler(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(p tview.Primitive) { app.SetFocus(p) })
	if copied {
		t.Error("Enter on the summary pressed Copy report")
	}
	if lay.IsOpen() {
		t.Error("Enter on the summary did not close it (Done)")
	}
}
