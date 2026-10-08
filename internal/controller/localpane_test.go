package controller

import (
	"strings"
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
)

func TestLocalTitleEscapesItsTag(t *testing.T) {
	// "[local]" is a colour tag as far as tview is concerned, so the literal
	// bracket has to be escaped or the marker vanishes from the pane title —
	// which is exactly what a pty smoke test caught.
	c := newTestController()
	c.params = &cfg.Params{HomeDir: "/home/u"}
	c.pane().SetLocalDir(t.TempDir())
	got := c.localTitle()
	if !strings.Contains(got, "[local[]") {
		t.Errorf("localTitle = %q, want an escaped [local[] marker", got)
	}
	if !strings.Contains(got, "file(s)") {
		t.Errorf("localTitle = %q, want the file count", got)
	}
}

func TestLocalPaneScopeIsDistinct(t *testing.T) {
	// A local pane must not share the buckets screen's empty selection scope,
	// or marks would leak between them.
	c := newTestController()
	if got := c.pane().ScopeKey(); got != "" {
		t.Errorf("the buckets screen scope should be empty, got %q", got)
	}
	c.pane().SetLocalDir("/tmp/x")
	if got := c.pane().ScopeKey(); got != "local:/tmp/x" {
		t.Errorf("local scope = %q", got)
	}
	name := "bkt"
	c.pane().SetLocalDir("")
	c.pane().Enter(&model.Object{Key: &name, Ot: model.Bucket}, "p/", "")
	if got := c.pane().ScopeKey(); got != "bkt:p/" {
		t.Errorf("remote scope = %q", got)
	}
}

func TestLocalModeAndPaneLabel(t *testing.T) {
	c := newTestController()
	if c.localMode() || c.hasLocalPane() {
		t.Error("a fresh controller has no local pane")
	}
	if !strings.Contains(c.localPaneLabel(), "browse the filesystem") {
		t.Errorf("closed label = %q", c.localPaneLabel())
	}
	c.pane().SetLocalDir("/tmp")
	if !c.localMode() || !c.hasLocalPane() {
		t.Error("localDir should make the pane local")
	}
	if !strings.Contains(c.localPaneLabel(), "close it") {
		t.Errorf("open label = %q — a toggle has to say which way it goes", c.localPaneLabel())
	}
}
