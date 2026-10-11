package controller

import (
	"path/filepath"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

// newBookmarkController builds a controller wired far enough to open the
// bookmark manager: a real view and shell, a profile holding bookmarks, and a
// model pointed at a dead endpoint so the jump's background goroutine fails
// fast instead of reaching the network.
func newBookmarkController(t *testing.T, bookmarks []cfg.Bookmark) *Controller {
	t.Helper()

	dir := t.TempDir()
	conf := &cfg.Config{Name: "test", BaseUrl: "http://127.0.0.1:1", Bookmarks: bookmarks}
	mdl, err := model.NewModel(modelConfigFor(conf))
	if err != nil {
		t.Fatalf("model.NewModel: %v", err)
	}

	v := widgets.NewView()
	c := &Controller{
		view:         v,
		model:        mdl,
		activeConfig: conf,
		params:       &cfg.Params{HomeDir: dir, FileName: filepath.Join(dir, "config.json"), Config: []*cfg.Config{conf}},
		panes:        [2]*session.Pane{session.New(), session.New()},
	}
	c.shell = shell.New(v.App, v.Pages)
	name := "bucket"
	c.pane().SetBucket(&model.Object{Key: &name, Ot: model.Bucket})
	return c
}

// pressEnterAt drives the list's own Enter handling, which is what invokes the
// row's selected function.
func pressEnterAt(list *tview.List, i int) {
	list.SetCurrentItem(i)
	list.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
}

// TestBookmarkJumpClosesManager pins the close that makes the manager usable:
// picking a bookmark must take its overlay down before navigating, or the user
// lands behind a modal covering the listing they jumped to.
//
// The rows are built before the layer is pushed, so they have to reach it
// indirectly; a row that captured the handle by value captured nil, and
// Layer.Close is nil-safe, so the miss was silent.
func TestBookmarkJumpClosesManager(t *testing.T) {
	c := newBookmarkController(t, []cfg.Bookmark{{Name: "logs", Bucket: "bucket", Prefix: "logs/"}})

	var lay *shell.Layer
	list := tview.NewList().ShowSecondaryText(false)
	c.rebuildBookmarkList(list, &lay)
	lay = c.shell.Push(list)

	// Row 0 is "add current location" (the pane has a bucket open), so the
	// first bookmark is row 1.
	if got := list.GetItemCount(); got != 2 {
		t.Fatalf("GetItemCount() = %d, want 2 (add-current row + one bookmark)", got)
	}
	pressEnterAt(list, c.bookmarkOffset())

	if lay.IsOpen() {
		t.Fatal("the bookmark manager is still open after jumping to a bookmark; it covers the location the user jumped to")
	}
}

// TestBookmarkRowsSurviveRebuild covers the same wiring after the list is
// rebuilt in place (adding or removing a bookmark), which is the path that
// re-created the rows and so could re-introduce the stale handle.
func TestBookmarkRowsSurviveRebuild(t *testing.T) {
	c := newBookmarkController(t, []cfg.Bookmark{{Name: "logs", Bucket: "bucket", Prefix: "logs/"}})

	var lay *shell.Layer
	list := tview.NewList().ShowSecondaryText(false)
	c.rebuildBookmarkList(list, &lay)
	lay = c.shell.Push(list)

	// Add the pane's current location, which rebuilds every row.
	pressEnterAt(list, 0)
	if !lay.IsOpen() {
		t.Fatal("adding a bookmark closed the manager; it should stay open")
	}
	if got := len(c.activeConfig.Bookmarks); got != 2 {
		t.Fatalf("len(Bookmarks) = %d after adding the current location, want 2", got)
	}

	pressEnterAt(list, c.bookmarkOffset())
	if lay.IsOpen() {
		t.Fatal("a row rebuilt by addCurrentBookmark no longer closes the manager")
	}
}
