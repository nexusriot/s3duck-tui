package view

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestScrollHint(t *testing.T) {
	tests := []struct {
		name                   string
		offset, visible, total int
		want                   string
	}{
		{"whole list visible", 0, 20, 15, "Esc / q closes"},
		{"exactly fits", 0, 15, 15, "Esc / q closes"},
		{"no height yet", 0, 0, 48, "Esc / q closes"},
		{
			"top of a longer list", 0, 40, 48,
			"↑/↓ PgUp/PgDn scroll — lines 1-40 of 48 — Esc / q closes",
		},
		{
			"scrolled", 5, 40, 48,
			"↑/↓ PgUp/PgDn scroll — lines 6-45 of 48 — Esc / q closes",
		},
		// tview lets the offset run past the end (and starts it at -1) and
		// clamps it while drawing, so the hint has to clamp it too.
		{
			"offset past the end", 999, 40, 48,
			"↑/↓ PgUp/PgDn scroll — lines 9-48 of 48 — Esc / q closes",
		},
		{
			"offset before the start", -1, 40, 48,
			"↑/↓ PgUp/PgDn scroll — lines 1-40 of 48 — Esc / q closes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrollHint(tt.offset, tt.visible, tt.total); got != tt.want {
				t.Errorf("scrollHint(%d, %d, %d) = %q, want %q",
					tt.offset, tt.visible, tt.total, got, tt.want)
			}
		})
	}
}

var styleTag = regexp.MustCompile(`\[::[a-z-]+\]`)

// The panel does not word-wrap, so a line wider than the panel is clipped.
func TestHelpTextFitsPanel(t *testing.T) {
	const inner = helpWidth - 2 // borders

	for name, text := range map[string]string{"browser": helpBrowser, "profiles": helpProfiles} {
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "\t") {
				t.Errorf("%s help line %d contains a tab: %q", name, i+1, line)
			}
			if w := len([]rune(styleTag.ReplaceAllString(line, ""))); w > inner {
				t.Errorf("%s help line %d is %d cells wide, panel fits %d: %q",
					name, i+1, w, inner, line)
			}
		}
	}
}

// Every binding should be listed once: the browser list used to name Ctrl+K
// twice, once as "Command palette" and once as the palette's contents.
func TestHelpKeysAreUnique(t *testing.T) {
	for name, text := range map[string]string{"browser": helpBrowser, "profiles": helpProfiles} {
		seen := make(map[string]int)
		for i, line := range strings.Split(text, "\n") {
			key, ok := helpKey(line)
			if !ok {
				continue
			}
			if prev, dup := seen[key]; dup {
				t.Errorf("%s help lists %q on lines %d and %d", name, key, prev, i+1)
				continue
			}
			seen[key] = i + 1
		}
		if len(seen) == 0 {
			t.Errorf("%s help: no key entries parsed, the test is not checking anything", name)
		}
	}
}

// helpKey pulls the key column out of an entry line ("    Ctrl+N   Create..."),
// reporting false for headings and blank lines.
func helpKey(line string) (string, bool) {
	if !strings.HasPrefix(line, "    ") {
		return "", false
	}
	fields := strings.SplitN(strings.TrimSpace(line), "  ", 2)
	if len(fields) != 2 {
		return "", false
	}
	return strings.TrimSpace(fields[0]), true
}

// The hotkey list is taller than a 24-row terminal, so the panel has to be
// clamped to the space Pages actually has, not to its requested height.
func TestClampToContent(t *testing.T) {
	v := &View{}

	// Before the first draw nothing is known, so the request stands.
	if w, h := v.clampToContent(76, 51); w != 76 || h != 51 {
		t.Errorf("clampToContent before first draw = (%d, %d), want (76, 51)", w, h)
	}

	v.screenW.Store(120)
	v.screenH.Store(50)
	if w, h := v.ContentSize(); w != 118 || h != 50-frameChromeRows {
		t.Errorf("ContentSize() = (%d, %d), want (118, %d)", w, h, 50-frameChromeRows)
	}
	if w, h := v.clampToContent(76, 51); w != 76 || h != 44 {
		t.Errorf("clampToContent on a 50-row terminal = (%d, %d), want (76, 44)", w, h)
	}

	v.screenW.Store(60)
	v.screenH.Store(24)
	if w, h := v.clampToContent(76, 51); w != 58 || h != 18 {
		t.Errorf("clampToContent on a 24-row terminal = (%d, %d), want (58, 18)", w, h)
	}
}

// End-to-end on a simulated 20-row terminal: the list does not fit, so the tail
// must be reachable by scrolling and the scroll keys must not close the panel.
// Both were broken before — the panel was a fixed 44 rows and closed on any key.
func TestHelpPanelScrolls(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer screen.Fini()
	screen.SetSize(80, 20)

	v := &View{}
	// What the frame leaves for Pages on this screen: 80x20.
	v.screenW.Store(82)
	v.screenH.Store(20 + frameChromeRows)

	var closed int
	panel := v.HotkeysModal(false, func() { closed++ })
	panel.SetRect(0, 0, 80, 20)

	draw := func() string {
		panel.Draw(screen)
		screen.Show()
		return renderScreen(t, screen)
	}

	first := draw()
	if !strings.Contains(first, "Navigation") {
		t.Fatalf("top of the list not drawn:\n%s", first)
	}
	if strings.Contains(first, "Ctrl+Q") {
		t.Fatalf("expected the tail of the list to be off-screen:\n%s", first)
	}
	if want := fmt.Sprintf("of %d", strings.Count(helpBrowser, "\n")); !strings.Contains(first, want) {
		t.Errorf("footer does not report the list length (%q):\n%s", want, first)
	}

	// The text view is the focus target inside the panel, which is what the
	// application would hand keys to.
	target := focusDeepest(panel)
	send := func(key tcell.Key, r rune) {
		handler := target.InputHandler()
		if handler == nil {
			t.Fatalf("focused primitive %T takes no input", target)
		}
		handler(tcell.NewEventKey(key, r, tcell.ModNone), func(tview.Primitive) {})
	}

	send(tcell.KeyEnd, 0)
	end := draw()
	if !strings.Contains(end, "Ctrl+Q") {
		t.Errorf("End did not reveal the end of the list:\n%s", end)
	}
	if closed != 0 {
		t.Errorf("scrolling closed the panel (%d times)", closed)
	}

	send(tcell.KeyPgUp, 0)
	if up := draw(); strings.Contains(up, "Ctrl+Q") {
		t.Errorf("PgUp did not scroll back up from the end:\n%s", up)
	}
	if closed != 0 {
		t.Errorf("scrolling closed the panel (%d times)", closed)
	}

	send(tcell.KeyEsc, 0)
	if closed != 1 {
		t.Errorf("Esc closed the panel %d times, want 1", closed)
	}
	send(tcell.KeyRune, 'q')
	if closed != 2 {
		t.Errorf("q closed the panel %d times, want 2", closed-1)
	}
}

// focusDeepest walks the focus chain the way Application.SetFocus does, ending
// at the primitive that would actually receive keys.
func focusDeepest(p tview.Primitive) tview.Primitive {
	for i := 0; i < 10; i++ {
		var next tview.Primitive
		p.Focus(func(q tview.Primitive) { next = q })
		if next == nil || next == p {
			break
		}
		p = next
	}
	return p
}

func renderScreen(t *testing.T, screen tcell.SimulationScreen) string {
	t.Helper()
	cells, w, h := screen.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if runes := cells[y*w+x].Runes; len(runes) > 0 {
				b.WriteRune(runes[0])
			} else {
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
