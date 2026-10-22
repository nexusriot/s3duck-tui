package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

// confirmLabelLiteral finds a done handler deciding the confirm dialog's
// outcome by comparing its label against a string literal.
var confirmLabelLiteral = regexp.MustCompile(`\b(?:label|buttonLabel)\s*[!=]=\s*"([^"]*)"`)

// TestConfirmHandlersUseTheSharedLabel keeps every yes/no dialog comparing
// against widgets.ConfirmOK instead of retyping it.
//
// A done handler is told which button was pressed only as a string, so a typo
// there does not fail to compile — it just never matches, the dialog closes,
// and the operation behind it quietly does nothing. That is what happened to
// Empty Trash, which compared against "ok" while NewConfirm offers "OK": the
// confirmation sized the trash, promised to remove it, and removed nothing.
//
// Dialogs that build their own buttons ("Export list", "Retry failed…") are
// not this dialog and keep their literals; only a spelling of ConfirmOK is an
// offence.
func TestConfirmHandlersUseTheSharedLabel(t *testing.T) {
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
		for i, line := range strings.Split(string(src), "\n") {
			m := confirmLabelLiteral.FindStringSubmatch(line)
			if m == nil || !strings.EqualFold(m[1], widgets.ConfirmOK) {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", f, i+1, strings.TrimSpace(line)))
		}
	}

	if len(offenders) > 0 {
		t.Errorf("confirm handlers compare the button label to a literal; use widgets.ConfirmOK so a typo cannot silently decline:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
