package widgets

import (
	"os"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// formConstructors is every dialog this package builds for the controller to
// push as an overlay.
func formConstructors(v *View) map[string]tview.Primitive {
	return map[string]tview.Primitive{
		"NewCreateProfileForm":  v.NewCreateProfileForm("h"),
		"NewInputForm":          v.NewInputForm("h", "label", "value"),
		"NewProfileOptionsForm": v.NewProfileOptionsForm("p", ProfileOptions{}, []string{""}, []string{""}),
		"NewSearchForm":         v.NewSearchForm("h"),
		"NewBatchRenameForm":    v.NewBatchRenameForm("h"),
		"NewCopyMoveForm":       v.NewCopyMoveForm("h", []string{"b"}, "b", ""),
		"NewObjectMetaForm":     v.NewObjectMetaForm("k", "summary", false),
		"NewStorageClassForm":   v.NewStorageClassForm("k", "s", []string{"STANDARD"}, "STANDARD", []string{"Standard"}, false),
		"NewCreateForm":         v.NewCreateForm("h", false),
	}
}

// TestFormsDoNotManageTheirOwnOverlay pins the boundary between this package
// and the shell: a widget here is a primitive, not an overlay, and it must not
// try to take itself off screen.
//
// These constructors used to install an Esc handler that called
// Pages.RemovePage("modal") — a page name from before overlays were handles.
// The shell names every overlay "layer-N", so no page called "modal" has
// existed for some time and the handler removed nothing: Esc silently stopped
// closing ten dialogs, and the compiler had no reason to care. The forms now
// install no capture at all, and the caller that pushed the layer installs one
// that closes it.
func TestFormsDoNotManageTheirOwnOverlay(t *testing.T) {
	v := NewView()
	for name, p := range formConstructors(v) {
		box, ok := p.(interface {
			GetInputCapture() func(*tcell.EventKey) *tcell.EventKey
		})
		if !ok {
			continue
		}
		if box.GetInputCapture() != nil {
			t.Errorf("%s installs its own input capture; overlay lifetime belongs to whoever pushed it, and a page name written here cannot stay in step with the shell", name)
		}
	}
}

// TestNoHardCodedPageNames guards the other half: the shell allocates every
// page name, so a literal one in this package is addressing a page that either
// does not exist or belongs to someone else.
func TestNoHardCodedPageNames(t *testing.T) {
	src := readSource(t, "view.go")
	for _, bad := range []string{`RemovePage("`, `AddPage("modal`, `SwitchToPage("`} {
		if idx := indexOf(src, bad); idx >= 0 {
			t.Errorf("view.go addresses a page by literal name (%s...); the shell owns page names", bad)
		}
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
