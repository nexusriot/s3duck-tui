package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestParseChord(t *testing.T) {
	cases := []struct {
		spec string
		key  tcell.Key
		ch   rune
		mods tcell.ModMask
	}{
		{"P", tcell.KeyRune, 'P', 0},
		{"/", tcell.KeyRune, '/', 0},
		{"Space", tcell.KeyRune, ' ', 0},
		{"Enter", tcell.KeyEnter, 0, 0},
		{"Esc", tcell.KeyEsc, 0, 0},
		{"Backspace", tcell.KeyBackspace2, 0, 0},
		{"Del", tcell.KeyDelete, 0, 0},
		{"F5", tcell.KeyF5, 0, 0},
		{"Tab", tcell.KeyTab, 0, 0},
		// tcell reports Ctrl+letter as its own key constant, so a Ctrl
		// binding has to be expressed that way or it would never match.
		{"Ctrl+D", tcell.KeyCtrlD, 0, tcell.ModCtrl},
		{"ctrl+d", tcell.KeyCtrlD, 0, tcell.ModCtrl},
		{"Alt+Left", tcell.KeyLeft, 0, tcell.ModAlt},
	}
	for _, tc := range cases {
		got, err := parseChord(tc.spec)
		if err != nil {
			t.Errorf("parseChord(%q): %v", tc.spec, err)
			continue
		}
		if got.key != tc.key || got.ch != tc.ch || got.mods != tc.mods {
			t.Errorf("parseChord(%q) = %+v, want key=%v ch=%q mods=%v", tc.spec, got, tc.key, tc.ch, tc.mods)
		}
	}
	for _, bad := range []string{"", "   ", "Ctrl+", "NotAKey", "ab", "Ctrl+1"} {
		if _, err := parseChord(bad); err == nil {
			t.Errorf("parseChord(%q) should have failed", bad)
		}
	}
}

func TestChordOfMatchesParsedSpecs(t *testing.T) {
	// The property the dispatcher depends on: a spec and the event a user
	// actually produces must normalize to the same map key.
	cases := []struct {
		spec  string
		event *tcell.EventKey
	}{
		{"P", tcell.NewEventKey(tcell.KeyRune, 'P', tcell.ModNone)},
		{"Ctrl+D", tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl)},
		{"Space", tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone)},
		{"F5", tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone)},
		{"Alt+Left", tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModAlt)},
		{"Delete", tcell.NewEventKey(tcell.KeyDelete, 0, tcell.ModNone)},
	}
	for _, tc := range cases {
		if got, want := chordOf(tc.event), normalizeChord(tc.spec); got != want {
			t.Errorf("event for %q normalizes to %q, spec to %q", tc.spec, got, want)
		}
	}
}

func TestDefaultBindingsAreParseableAndUnique(t *testing.T) {
	seenDirect := map[string]actionID{}
	seenLeader := map[string]actionID{}
	for _, b := range defaultBindings() {
		if _, err := parseChord(b.chord); err != nil {
			t.Errorf("default binding %s = %q does not parse: %v", b.id, b.chord, err)
			continue
		}
		if b.help == "" {
			t.Errorf("default binding %s has no description", b.id)
		}
		c := normalizeChord(b.chord)
		table := seenDirect
		if b.leader {
			table = seenLeader
		}
		if prev, dup := table[c]; dup {
			t.Errorf("chord %q is bound twice: %s and %s", b.chord, prev, b.id)
		}
		table[c] = b.id
	}
}

func TestDefaultKeymapKeepsTheHistoricalKeys(t *testing.T) {
	// These were the bindings before the keymap existed. Changing one is a
	// user-visible break, so it should have to be done deliberately here.
	km := loadKeymap(filepath.Join(t.TempDir(), "absent.json"))
	want := map[string]actionID{
		"Ctrl+D": actDownload,
		"Ctrl+U": actUpload,
		"Ctrl+N": actCreate,
		"Ctrl+R": actRename,
		"Ctrl+Y": actCopy,
		"Ctrl+T": actMove,
		"Ctrl+G": actSummary,
		"Ctrl+L": actProperties,
		"Ctrl+W": actPresign,
		"Ctrl+E": actSync,
		"Ctrl+F": actSearch,
		"Ctrl+B": actBookmarks,
		"Ctrl+K": actPalette,
		"Ctrl+O": actDualPane,
		"Ctrl+P": actProfiles,
		"Ctrl+S": actSelectAll,
		"Ctrl+X": actClearSelection,
		"Ctrl+Q": actQuit,
		"Delete": actDelete,
		"Space":  actToggleSelect,
		"/":      actFilter,
		"r":      actRefresh,
		"F5":     actRefreshAlt,
		"s":      actSortKey,
		"S":      actSortDir,
		"v":      actVersions,
		"m":      actMeta,
		"c":      actStorageClass,
		"e":      actEdit,
		"D":      actDuplicates,
		"=":      actCompare,
		">":      actCrossCopy,
		"y":      actYank,
		"x":      actCut,
		"p":      actPaste,
		"u":      actUndo,
		"t":      actTransfers,
		"[":      actHistoryBack,
		"]":      actHistoryFwd,
		"Tab":    actSwapPane,
	}
	for spec, id := range want {
		got, ok := km.direct[normalizeChord(spec)]
		if !ok {
			t.Errorf("%s is not bound at all", spec)
			continue
		}
		if got != id {
			t.Errorf("%s = %s, want %s", spec, got, id)
		}
	}
}

func TestLoadKeymapOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	body := `{
	  "leader": "\\",
	  "keys": {"preview": "z", "download": "", "usage": "Ctrl+J"},
	  "leader_keys": {"activity-log": "L"}
	}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	km := loadKeymap(path)
	if len(km.warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", km.warnings)
	}
	if got := km.direct[normalizeChord("z")]; got != actPreview {
		t.Errorf("preview not rebound to z: %v", got)
	}
	// A rebound action must lose its default chord, or both keys would work
	// and the rebind would look like it had not taken.
	if got, ok := km.direct[normalizeChord("P")]; ok {
		t.Errorf("the default P is still bound to %v after a rebind", got)
	}
	// An empty chord is an explicit unbind.
	if _, ok := km.direct[normalizeChord("Ctrl+D")]; ok {
		t.Error("download should be unbound")
	}
	if got := km.direct[normalizeChord("Ctrl+J")]; got != actUsage {
		t.Errorf("usage not rebound: %v", got)
	}
	if got := km.leader[normalizeChord("L")]; got != actActivityLog {
		t.Errorf("leader rebinding failed: %v", got)
	}
	if km.leaderChord != normalizeChord("\\") {
		t.Errorf("leader chord = %q, want the override", km.leaderChord)
	}
}

func TestLoadKeymapReportsBadInputWithoutBreaking(t *testing.T) {
	dir := t.TempDir()

	// Malformed JSON: the defaults must survive, with a warning.
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0600)
	km := loadKeymap(bad)
	if len(km.warnings) == 0 {
		t.Error("a malformed file should warn")
	}
	if got := km.direct[normalizeChord("Ctrl+D")]; got != actDownload {
		t.Error("the defaults must survive a malformed override file")
	}

	// Unknown action and unparseable chord: warn, ignore, keep going.
	odd := filepath.Join(dir, "odd.json")
	os.WriteFile(odd, []byte(`{"keys": {"not-an-action": "z", "preview": "Ctrl+"}}`), 0600)
	km = loadKeymap(odd)
	if len(km.warnings) != 2 {
		t.Errorf("warnings = %v, want one per problem", km.warnings)
	}
	if got := km.direct[normalizeChord("P")]; got != actPreview {
		t.Error("an unparseable override should leave the default in place")
	}
}

func TestKeymapLookupAndLeader(t *testing.T) {
	km := loadKeymap(filepath.Join(t.TempDir(), "none.json"))
	if id, ok := km.lookup(tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl)); !ok || id != actDownload {
		t.Errorf("lookup(Ctrl+D) = %v/%v", id, ok)
	}
	if _, ok := km.lookup(tcell.NewEventKey(tcell.KeyRune, 'ß', tcell.ModNone)); ok {
		t.Error("an unbound key must not resolve")
	}
	if !km.isLeader(tcell.NewEventKey(tcell.KeyRune, ',', tcell.ModNone)) {
		t.Error("the default leader is not recognised")
	}
	if id, ok := km.lookupLeader(tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone)); !ok || id != actActivityLog {
		t.Errorf("leader a = %v/%v, want the activity log", id, ok)
	}
}

func TestHelpLinesCoverTheBindings(t *testing.T) {
	km := loadKeymap(filepath.Join(t.TempDir(), "none.json"))
	lines := km.helpLines()
	if len(lines) != len(km.help) {
		t.Fatalf("helpLines dropped entries: %d vs %d", len(lines), len(km.help))
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Ctrl+D", "download", "Ctrl+Q", "l", "local directory"} {
		if !strings.Contains(joined, want) {
			t.Errorf("help is missing %q:\n%s", want, joined)
		}
	}
	// Leader bindings are shown with their prefix, so the panel explains how
	// to reach them.
	if !strings.Contains(joined, ", a") {
		t.Errorf("leader bindings are not shown with the leader key:\n%s", joined)
	}
}
