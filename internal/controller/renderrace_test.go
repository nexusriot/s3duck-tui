package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

func raceObj(key string) *model.Object {
	k, size := key, int64(1)
	return &model.Object{Key: &k, Ot: model.File, Size: &size, FullPath: &key}
}

// A render snapshotted from pane A must not land in pane B's list when a Tab
// is processed before its queued closure runs. It did: B showed A's rows under
// A's title, and Delete — resolving the highlighted name against B — removed
// a same-named object from B's bucket while the screen said A's.
func TestRenderDoesNotLandInThePaneTabbedTo(t *testing.T) {
	v := widgets.NewView()
	c := &Controller{view: v, panes: [2]*session.Pane{session.New(), session.New()}, browsing: true, dual: true}
	c.shell = shell.New(v.App, v.Pages)
	prod, backup := "prod", "backup"
	c.panes[0].Enter(&model.Object{Key: &prod, Ot: model.Bucket}, "", "")
	c.panes[0].SetObjs(map[string]*model.Object{"only-in-prod.txt": raceObj("only-in-prod.txt"), "shared.txt": raceObj("shared.txt")})
	c.panes[1].Enter(&model.Object{Key: &backup, Ot: model.Bucket}, "", "")
	c.panes[1].SetObjs(map[string]*model.Object{"shared.txt": raceObj("shared.txt")})
	c.view.List, c.view.Filter, c.view.Header = v.PaneList(0), v.PaneFilter(0), v.PaneHeader(0)

	sim := tcell.NewSimulationScreen("UTF-8")
	v.App.SetScreen(sim)
	go func() { _ = v.App.Run() }()
	defer v.App.Stop()

	c.renderList() // snapshot of pane 0 queued …
	c.swapPane()   // … and Tab handled before it runs
	ran := make(chan struct{})
	c.shell.Do(func() { close(ran) }) // queued after the render: FIFO
	c.shell.Start()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell never ran the queued work")
	}

	inspected := make(chan struct{})
	v.App.QueueUpdate(func() {
		defer close(inspected)
		l1 := v.PaneList(1)
		for i := 0; i < l1.GetItemCount(); i++ {
			if _, key := l1.GetItemText(i); key == "only-in-prod.txt" {
				t.Errorf("pane 1 (backup) shows pane 0's row %q under title %q", key, l1.GetTitle())
			}
		}
	})
	select {
	case <-inspected:
	case <-time.After(5 * time.Second):
		t.Fatal("could not inspect the list")
	}
}

// drawPages renders the view's page stack (the overlays on top) to text.
func drawPages(t *testing.T, v *widgets.View) string {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(120, 30)
	v.Pages.SetRect(0, 0, 120, 30)
	v.Pages.Draw(screen)
	screen.Show()
	cells, w, h := screen.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if rs := cells[y*w+x].Runes; len(rs) > 0 {
				b.WriteRune(rs[0])
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Confirmations and waits name objects; the user answers the question for
// the key they are shown, so a bracketed segment must be shown as written.
func TestConfirmAndWaitShowKeysLiterally(t *testing.T) {
	v := widgets.NewView()
	c := &Controller{view: v}
	c.shell = shell.New(v.App, v.Pages)

	c.confirmThen("Undo rename backup[old].tar?", func() {})
	if got := drawPages(t, v); !strings.Contains(got, "backup[old].tar") {
		t.Errorf("confirmation rendered:\n%s", got)
	}
	c.wait("Reading a[b]c ...")
	if got := drawPages(t, v); !strings.Contains(got, "a[b]c") {
		t.Errorf("wait rendered:\n%s", got)
	}
}
