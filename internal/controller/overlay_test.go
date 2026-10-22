package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPushedFormsInstallEscape pins the other half of the overlay boundary.
//
// internal/ui/widgets builds a dialog as a plain primitive and installs no key
// handling of its own (see TestFormsDoNotManageTheirOwnOverlay there). Closing
// it is therefore the job of whoever pushed the layer — and the only thing
// that makes Esc work is an explicit escapeCloses on the pushed form. Forget
// it and the dialog is still perfectly usable through its Cancel button, so
// nothing fails and nobody notices until a user presses Esc and nothing
// happens. That is exactly how Esc came to be dead in ten dialogs at once.
func TestPushedFormsInstallEscape(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, fn := range splitTopLevelFuncs(string(src)) {
			if !buildsAViewForm(fn.body) || !pushesAnOverlay(fn.body) {
				continue
			}
			if !wiresEscape(fn.body) {
				offenders = append(offenders, f+": "+fn.name)
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("these push a widgets form as an overlay without wiring Esc to close it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

type topFunc struct{ name, body string }

// splitTopLevelFuncs cuts a file at its top-level "func " declarations. Nested
// closures stay with the function that owns them, which is what matters here:
// several flows build the form in one closure and push it in another.
func splitTopLevelFuncs(src string) []topFunc {
	var out []topFunc
	parts := strings.Split(src, "\nfunc ")
	for _, p := range parts[1:] {
		name := p
		if i := strings.IndexByte(name, '\n'); i >= 0 {
			name = name[:i]
		}
		// Drop a method receiver so the name is the method's own.
		if strings.HasPrefix(name, "(") {
			if i := strings.IndexByte(name, ')'); i >= 0 {
				name = strings.TrimSpace(name[i+1:])
			}
		}
		if i := strings.IndexByte(name, '('); i >= 0 {
			name = strings.TrimSpace(name[:i])
		}
		if name == "" {
			name = "<unnamed>"
		}
		out = append(out, topFunc{name: name, body: p})
	}
	return out
}

// buildsAViewForm reports whether the body constructs one of the widgets
// dialogs, which are the primitives with no key handling of their own.
func buildsAViewForm(body string) bool {
	i := strings.Index(body, "c.view.New")
	for i >= 0 {
		rest := body[i:]
		if end := strings.Index(rest, "("); end >= 0 && strings.HasSuffix(rest[:end], "Form") {
			return true
		}
		next := strings.Index(rest[len("c.view.New"):], "c.view.New")
		if next < 0 {
			return false
		}
		i += len("c.view.New") + next
	}
	return false
}

func pushesAnOverlay(body string) bool {
	return strings.Contains(body, "c.pushModal(") ||
		strings.Contains(body, "c.pushModalClamped(") ||
		strings.Contains(body, "c.pushRaw(")
}

// wiresEscape reports whether a flow closes its overlay on Esc, either through
// the escapeCloses helper or by handling tcell.KeyEsc and closing the layer
// itself — which is what the two local-filesystem pickers do, because Esc
// there shares a capture with Tab and Backspace.
func wiresEscape(body string) bool {
	if strings.Contains(body, "escapeCloses(") {
		return true
	}
	return strings.Contains(body, "tcell.KeyEsc") && strings.Contains(body, ".Close()")
}
