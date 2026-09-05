package controller

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// readDoc loads a documentation file from the repository root. The keys,
// action names and config fields it checks are contracts a user reads in the
// README and types, so they are asserted against the code rather than kept in
// step by hand.
func readDoc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatalf("cannot read %s: %v", name, err)
	}
	return string(data)
}

// docChords extracts the key column of every two-column markdown table row,
// splitting the "a / b" and ", x" spellings the tables use.
func docChords(doc string) map[string]bool {
	out := map[string]bool{}
	rows := regexp.MustCompile(`(?m)^\| ([^|]+) \| [^|]+ \|$`).FindAllStringSubmatch(doc, -1)
	for _, r := range rows {
		cell := strings.TrimSpace(r[1])
		out[cell] = true
		for _, part := range strings.Split(cell, "/") {
			out[strings.TrimSpace(part)] = true
		}
	}
	return out
}

func TestReadmeDocumentsEveryDefaultBinding(t *testing.T) {
	readme := readDoc(t, "README.md")
	chords := docChords(readme)

	// Spellings the README may legitimately use for a chord.
	aliases := map[string][]string{
		"Delete":    {"Del", "Del / Delete"},
		"Backspace": {"Backspace"},
		"Space":     {"Space"},
		"Alt+Left":  {"[ / ]", "["}, // documented in the history row's prose
		"Alt+Right": {"[ / ]", "]"},
	}

	var missing []string
	for _, b := range defaultBindings() {
		chord := b.chord
		if b.leader {
			chord = ", " + b.chord
		}
		if chords[chord] {
			continue
		}
		found := false
		for _, alt := range aliases[b.chord] {
			if chords[alt] {
				found = true
			}
		}
		// A chord may also be documented inside a combined row ("s / S").
		if !found {
			for cell := range chords {
				if strings.Contains(cell, chord) {
					found = true
					break
				}
			}
		}
		if !found {
			missing = append(missing, fmt.Sprintf("%s (action %s)", chord, b.id))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("README's hotkey tables do not document:\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestReadmeKeysAreRealBindings(t *testing.T) {
	// The other direction: a table row naming a key that nothing binds would
	// send a user hunting for a feature that does not exist.
	readme := readDoc(t, "README.md")
	km := loadKeymap(t.TempDir() + "/absent.json")

	bound := map[string]bool{}
	for _, b := range defaultBindings() {
		bound[b.chord] = true
		if b.leader {
			bound[", "+b.chord] = true
		}
	}
	// Keys handled by the widgets rather than the keymap, plus the profiles
	// screen's own (non-configurable) bindings.
	widget := map[string]bool{
		"↑ / ↓": true, "↑": true, "↓": true, "Enter": true, "PgUp": true, "PgDn": true,
		"Ctrl+I": true, "o": true, "Ctrl+V": true, "Key": true, "--- ": true, "---": true,
		"d": true, "c": true, "g": true, "w": true, "D": true, "Esc": true, ",": true,
	}
	var unknown []string
	for cell := range docChords(readme) {
		if cell == "" || widget[cell] || bound[cell] {
			continue
		}
		// A combined cell ("s / S") is covered by its parts.
		if strings.Contains(cell, "/") {
			continue
		}
		// Anything that does not parse as a chord is prose, not a key.
		if _, err := parseChord(strings.TrimPrefix(cell, ", ")); err != nil {
			continue
		}
		if _, ok := km.direct[normalizeChord(cell)]; ok {
			continue
		}
		if _, ok := km.leader[normalizeChord(strings.TrimPrefix(cell, ", "))]; ok {
			continue
		}
		unknown = append(unknown, cell)
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("README documents keys nothing binds: %v", unknown)
	}
}

func TestEveryActionHasAKeyAndAHandler(t *testing.T) {
	c := &Controller{}
	table := c.actionTable()

	bound := map[actionID]bool{}
	for _, b := range defaultBindings() {
		bound[b.id] = true
		if _, ok := table[b.id]; !ok {
			t.Errorf("action %q has a default key but no handler", b.id)
		}
	}
	for id := range table {
		if !bound[id] {
			t.Errorf("action %q has a handler but no default key (it would be unreachable outside the palette)", id)
		}
	}
}

func TestDocsDocumentEveryProfileField(t *testing.T) {
	// Every persisted config field is part of the file format, so both the
	// README (where a user edits it) and DESIGN.md (where the shape is
	// recorded) must name it.
	cfgSrc, err := os.ReadFile("../../internal/config/config.go")
	if err != nil {
		t.Fatal(err)
	}
	fields := regexp.MustCompile(`json:"([a-z_0-9]+)`).FindAllStringSubmatch(string(cfgSrc), -1)
	if len(fields) < 10 {
		t.Fatalf("only %d json fields found; the parser is broken, not the docs", len(fields))
	}

	for _, doc := range []string{"README.md", "DESIGN.md"} {
		text := readDoc(t, doc)
		var missing []string
		for _, f := range fields {
			if !strings.Contains(text, `"`+f[1]+`"`) {
				missing = append(missing, f[1])
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s does not document profile fields: %v", doc, missing)
		}
	}
}

func TestKeymapExampleActionsExist(t *testing.T) {
	// The keys.json example in the README must use real action names, or
	// copying it produces a file the app reports as invalid.
	readme := readDoc(t, "README.md")
	block := readme[strings.Index(readme, `"leader":`):]
	block = block[:strings.Index(block, "```")]

	known := map[string]bool{}
	for _, b := range defaultBindings() {
		known[string(b.id)] = true
	}
	for _, m := range regexp.MustCompile(`"([a-z-]+)":\s*"`).FindAllStringSubmatch(block, -1) {
		name := m[1]
		if name == "leader" {
			continue
		}
		if !known[name] {
			t.Errorf("the README's keys.json example names %q, which is not an action", name)
		}
	}
}

func TestRoadmapDoesNotPromiseWhatShipped(t *testing.T) {
	// A roadmap that still lists a delivered feature is worse than no
	// roadmap. These are the items this round closed; each names a symbol
	// that now exists, so the check is against the code, not a date.
	roadmap := readDoc(t, "ROADMAP.md")
	pending := roadmap[strings.Index(roadmap, "## Next"):]

	shipped := map[string]string{
		"Sync include/exclude globs":  "applyExcludes",
		"Read-only profile flag":      "readOnlyBlocked",
		"OS keyring":                  "", // still open, must NOT be flagged
		"Summary by storage class":    "classBreakdown",
		"Post-transfer verify":        "VerifyLocalFile",
		"Text preview via ranged GET": "GetObjectHead",
		"Anonymous profiles":          "", // still open
	}
	for phrase, symbol := range shipped {
		if symbol == "" {
			continue
		}
		if strings.Contains(pending, phrase) {
			t.Errorf("ROADMAP still lists %q as pending, but %s exists", phrase, symbol)
		}
	}
}

func TestGeneratedHelpLinesFitThePanel(t *testing.T) {
	// The hotkey panel is 76 columns wide including borders (view.helpWidth),
	// and word wrap is deliberately off inside it — a line that overflows is
	// silently clipped, which is how the old panel's tail went unnoticed.
	const inner = 74
	km := loadKeymap(t.TempDir() + "/absent.json")
	lines := km.helpLines()
	if len(lines) == 0 {
		t.Fatal("no help lines generated")
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > inner {
			t.Errorf("help line is %d cells wide, the panel fits %d: %q", n, inner, l)
		}
		if strings.Contains(l, "\t") {
			t.Errorf("help line contains a tab, which breaks the column: %q", l)
		}
	}
}

func TestGeneratedHelpHasNoDuplicateChords(t *testing.T) {
	km := loadKeymap(t.TempDir() + "/absent.json")
	seen := map[string]string{}
	for _, h := range km.help {
		if prev, dup := seen[h.Chord]; dup {
			t.Errorf("the panel lists %q twice: %q and %q", h.Chord, prev, h.Help)
		}
		seen[h.Chord] = h.Help
	}
}
