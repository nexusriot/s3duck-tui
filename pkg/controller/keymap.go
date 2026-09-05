package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// actionID names one thing the browser can do. Strings rather than an iota so
// the config file is readable and stays valid when actions are added.
type actionID string

const (
	actDown           actionID = "enter"
	actUp             actionID = "up"
	actToggleSelect   actionID = "toggle-select"
	actSelectAll      actionID = "select-all"
	actClearSelection actionID = "clear-selection"
	actFilter         actionID = "filter"
	actCancelListing  actionID = "cancel-listing"
	actRefresh        actionID = "refresh"
	actRefreshAlt     actionID = "refresh-alt"
	actSortKey        actionID = "sort-key"
	actSortDir        actionID = "sort-dir"
	actDownload       actionID = "download"
	actUpload         actionID = "upload"
	actDelete         actionID = "delete"
	actCreate         actionID = "create"
	actRename         actionID = "rename"
	actCopy           actionID = "copy"
	actMove           actionID = "move"
	actYank           actionID = "yank"
	actCut            actionID = "cut"
	actPaste          actionID = "paste"
	actUndo           actionID = "undo"
	actSearch         actionID = "search"
	actProperties     actionID = "properties"
	actPresign        actionID = "presign"
	actSummary        actionID = "summary"
	actUsage          actionID = "usage"
	actPreview        actionID = "preview"
	actVerify         actionID = "verify"
	actVersions       actionID = "versions"
	actMeta           actionID = "metadata"
	actStorageClass   actionID = "storage-class"
	actEdit           actionID = "edit"
	actDuplicates     actionID = "duplicates"
	actSync           actionID = "sync"
	actCompare        actionID = "compare-panes"
	actCrossCopy      actionID = "cross-profile-copy"
	actBookmarks      actionID = "bookmarks"
	actPalette        actionID = "palette"
	actTransfers      actionID = "transfers"
	actHistoryBack    actionID = "history-back"
	actHistoryFwd     actionID = "history-forward"
	actHistoryBackAlt actionID = "history-back-alt"
	actHistoryFwdAlt  actionID = "history-forward-alt"
	actDualPane       actionID = "dual-pane"
	actSwapPane       actionID = "swap-pane"
	actLocalPane      actionID = "local-pane"
	actTrashRestore   actionID = "trash-restore"
	actTrashEmpty     actionID = "trash-empty"
	actProfiles       actionID = "profiles"
	actHotkeys        actionID = "hotkeys"
	actAbout          actionID = "about"
	actActivityLog    actionID = "activity-log"
	actBucketConfig   actionID = "bucket-config"
	actAbortUploads   actionID = "abort-uploads"
	actQuit           actionID = "quit"
)

// defaultBinding pairs an action with its out-of-the-box chord and the
// description shown in the hotkey panel.
type defaultBinding struct {
	id     actionID
	chord  string
	leader bool // reached through the leader key rather than directly
	help   string
}

// defaultBindings is the browser layout. Everything that was bound before this
// table existed keeps its chord; the actions added alongside it live either on
// a free key or behind the leader.
func defaultBindings() []defaultBinding {
	return []defaultBinding{
		{actUp, "Backspace", false, "up one level"},
		{actToggleSelect, "Space", false, "mark / unmark"},
		{actSelectAll, "Ctrl+S", false, "mark everything visible"},
		{actClearSelection, "Ctrl+X", false, "clear the marks"},
		{actFilter, "/", false, "filter the listing (text, *.glob, re:regex)"},
		{actCancelListing, "Esc", false, "stop a running listing"},
		{actRefresh, "r", false, "re-list from the server"},
		{actRefreshAlt, "F5", false, "re-list from the server"},
		{actSortKey, "s", false, "cycle sort: name / size / date"},
		{actSortDir, "S", false, "reverse the sort"},
		{actDownload, "Ctrl+D", false, "download"},
		{actUpload, "Ctrl+U", false, "upload (local browser)"},
		{actDelete, "Delete", false, "delete"},
		{actCreate, "Ctrl+N", false, "new bucket / folder"},
		{actRename, "Ctrl+R", false, "rename (pattern rename when several are marked)"},
		{actCopy, "Ctrl+Y", false, "copy to…"},
		{actMove, "Ctrl+T", false, "move to…"},
		{actYank, "y", false, "yank (copy) to the object clipboard"},
		{actCut, "x", false, "cut to the object clipboard"},
		{actPaste, "p", false, "paste here"},
		{actUndo, "u", false, "undo the last move / rename"},
		{actSearch, "Ctrl+F", false, "recursive search"},
		{actProperties, "Ctrl+L", false, "properties"},
		{actPresign, "Ctrl+W", false, "presigned share link"},
		{actSummary, "Ctrl+G", false, "size summary"},
		{actUsage, "G", false, "usage browser (drill down by size)"},
		{actPreview, "P", false, "preview (read-only)"},
		{actVerify, "V", false, "verify the local copy against the object"},
		{actVersions, "v", false, "version history"},
		{actMeta, "m", false, "metadata and tags"},
		{actStorageClass, "c", false, "storage class / restore"},
		{actEdit, "e", false, "edit in $EDITOR"},
		{actDuplicates, "D", false, "find duplicates"},
		{actSync, "Ctrl+E", false, "sync"},
		{actCompare, "=", false, "compare the two panes"},
		{actCrossCopy, ">", false, "copy to another profile"},
		{actBookmarks, "Ctrl+B", false, "bookmarks"},
		{actPalette, "Ctrl+K", false, "command palette"},
		{actTransfers, "t", false, "transfers panel"},
		{actHistoryBack, "[", false, "back"},
		{actHistoryFwd, "]", false, "forward"},
		{actHistoryBackAlt, "Alt+Left", false, "back"},
		{actHistoryFwdAlt, "Alt+Right", false, "forward"},
		{actDualPane, "Ctrl+O", false, "toggle dual pane"},
		{actSwapPane, "Tab", false, "switch pane"},
		{actLocalPane, "l", false, "point the other pane at a local directory"},
		{actProfiles, "Ctrl+P", false, "back to profiles"},
		{actHotkeys, "Ctrl+H", false, "this panel"},
		{actAbout, "Ctrl+A", false, "about"},
		{actQuit, "Ctrl+Q", false, "quit"},

		// Behind the leader: everything that arrived after the direct keys ran
		// out, plus the actions that were already palette-only.
		{actTrashRestore, "R", false, "restore from the trash"},
		{actTrashEmpty, "t", true, "empty the trash"},
		{actActivityLog, "a", true, "activity log"},
		{actBucketConfig, "b", true, "bucket configuration"},
		{actAbortUploads, "m", true, "abort incomplete multipart uploads"},
		{actUsage, "u", true, "usage browser"},
		{actVerify, "v", true, "verify the local copy"},
	}
}

// defaultLeader is the prefix key for the second namespace. A comma is chosen
// because it is unbound in every screen and needs no modifier — the point of a
// leader is that the keys behind it are cheap to reach.
const defaultLeader = ","

// keymap resolves a key event to an action. direct holds the ordinary chords;
// leader holds the ones reached after the leader key.
type keymap struct {
	direct map[string]actionID
	leader map[string]actionID
	// leaderChord is the chord that arms the leader namespace.
	leaderChord string
	// help is the rendered key/description list for the hotkey panel.
	help []keyHelp
	// warnings records overrides that could not be parsed, surfaced once at
	// startup rather than silently ignored.
	warnings []string
}

// keyHelp is one line of the hotkey panel, derived from the live keymap so the
// panel can never disagree with what the keys actually do.
type keyHelp struct {
	Chord string
	Help  string
}

// keysFileName is the override file, alongside the profile config.
const keysFileName = "keys.json"

// keysFile is the JSON shape of the override file.
type keysFile struct {
	// Leader replaces the leader chord.
	Leader string `json:"leader,omitempty"`
	// Keys maps action name to chord. An empty chord unbinds the action.
	Keys map[string]string `json:"keys,omitempty"`
	// LeaderKeys maps action name to a chord in the leader namespace.
	LeaderKeys map[string]string `json:"leader_keys,omitempty"`
}

// KeysPath is where the override file lives.
func KeysPath(homeDir string) string {
	return filepath.Join(homeDir, ".config", "s3duck-tui", keysFileName)
}

// loadKeymap builds the keymap from the defaults plus any overrides in path.
// A missing file is the normal case. A malformed one is reported and ignored
// rather than fatal: locking a user out of their own keyboard over a stray
// comma would be a poor trade.
func loadKeymap(path string) keymap {
	km := keymap{
		direct:      map[string]actionID{},
		leader:      map[string]actionID{},
		leaderChord: normalizeChord(defaultLeader),
	}

	overrides := keysFile{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &overrides); err != nil {
			km.warnings = append(km.warnings, fmt.Sprintf("%s: %v (using the default keys)", path, err))
			overrides = keysFile{}
		}
	}
	if overrides.Leader != "" {
		if _, err := parseChord(overrides.Leader); err != nil {
			km.warnings = append(km.warnings, fmt.Sprintf("leader %q: %v", overrides.Leader, err))
		} else {
			km.leaderChord = normalizeChord(overrides.Leader)
		}
	}

	// Overridden actions drop their default chord entirely, so a rebind never
	// leaves the old key working as well — the confusing half-state.
	//
	// Only *usable* overrides count: an unparseable chord must leave the
	// default in place rather than unbinding the action, or one typo in the
	// config file would silently remove a key with nothing to replace it.
	// An empty chord is a deliberate unbind and does count.
	usable := func(chord string) bool {
		if strings.TrimSpace(chord) == "" {
			return true
		}
		_, err := parseChord(chord)
		return err == nil
	}
	overridden := map[actionID]bool{}
	for name, chord := range overrides.Keys {
		if usable(chord) {
			overridden[actionID(name)] = true
		}
	}
	leaderOverridden := map[actionID]bool{}
	for name, chord := range overrides.LeaderKeys {
		if usable(chord) {
			leaderOverridden[actionID(name)] = true
		}
	}

	for _, b := range defaultBindings() {
		if b.leader {
			if leaderOverridden[b.id] {
				continue
			}
			km.bind(true, b.chord, b.id, b.help)
			continue
		}
		if overridden[b.id] {
			continue
		}
		km.bind(false, b.chord, b.id, b.help)
	}

	helpFor := map[actionID]string{}
	for _, b := range defaultBindings() {
		if _, ok := helpFor[b.id]; !ok {
			helpFor[b.id] = b.help
		}
	}
	apply := func(m map[string]string, leader bool) {
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic warnings and help order
		for _, name := range names {
			chord := m[name]
			id := actionID(name)
			if _, known := helpFor[id]; !known {
				km.warnings = append(km.warnings, fmt.Sprintf("unknown action %q in %s", name, path))
				continue
			}
			if strings.TrimSpace(chord) == "" {
				continue // explicit unbind
			}
			if _, err := parseChord(chord); err != nil {
				km.warnings = append(km.warnings, fmt.Sprintf("%s = %q: %v", name, chord, err))
				continue
			}
			km.bind(leader, chord, id, helpFor[id])
		}
	}
	apply(overrides.Keys, false)
	apply(overrides.LeaderKeys, true)

	return km
}

// bind records one chord→action mapping and its help line.
func (k *keymap) bind(leader bool, chord string, id actionID, help string) {
	c := normalizeChord(chord)
	if leader {
		k.leader[c] = id
		k.help = append(k.help, keyHelp{Chord: k.leaderChord + " " + chord, Help: help})
		return
	}
	k.direct[c] = id
	k.help = append(k.help, keyHelp{Chord: chord, Help: help})
}

// lookup resolves a key event in the direct namespace.
func (k keymap) lookup(ev *tcell.EventKey) (actionID, bool) {
	id, ok := k.direct[chordOf(ev)]
	return id, ok
}

// lookupLeader resolves a key event in the leader namespace.
func (k keymap) lookupLeader(ev *tcell.EventKey) (actionID, bool) {
	id, ok := k.leader[chordOf(ev)]
	return id, ok
}

// isLeader reports whether the event arms the leader namespace.
func (k keymap) isLeader(ev *tcell.EventKey) bool {
	return k.leaderChord != "" && chordOf(ev) == k.leaderChord
}

// namedKeys maps the spellings accepted in the config file onto tcell keys.
// Only keys that make sense as a binding are listed; a rune is expressed as
// itself.
var namedKeys = map[string]tcell.Key{
	"enter":     tcell.KeyEnter,
	"esc":       tcell.KeyEsc,
	"escape":    tcell.KeyEsc,
	"tab":       tcell.KeyTab,
	"backtab":   tcell.KeyBacktab,
	"backspace": tcell.KeyBackspace2,
	"del":       tcell.KeyDelete,
	"delete":    tcell.KeyDelete,
	"insert":    tcell.KeyInsert,
	"home":      tcell.KeyHome,
	"end":       tcell.KeyEnd,
	"pgup":      tcell.KeyPgUp,
	"pgdn":      tcell.KeyPgDn,
	"up":        tcell.KeyUp,
	"down":      tcell.KeyDown,
	"left":      tcell.KeyLeft,
	"right":     tcell.KeyRight,
	"f1":        tcell.KeyF1,
	"f2":        tcell.KeyF2,
	"f3":        tcell.KeyF3,
	"f4":        tcell.KeyF4,
	"f5":        tcell.KeyF5,
	"f6":        tcell.KeyF6,
	"f7":        tcell.KeyF7,
	"f8":        tcell.KeyF8,
	"f9":        tcell.KeyF9,
	"f10":       tcell.KeyF10,
	"f11":       tcell.KeyF11,
	"f12":       tcell.KeyF12,
	"space":     tcell.KeyRune, // handled as the ' ' rune
}

// parsedChord is the canonical form of a binding.
type parsedChord struct {
	key  tcell.Key
	ch   rune
	mods tcell.ModMask
}

// parseChord reads a chord spec: an optional "Ctrl+"/"Alt+"/"Shift+" prefix
// chain followed by a named key or a single character.
func parseChord(spec string) (parsedChord, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return parsedChord{}, fmt.Errorf("empty binding")
	}
	var mods tcell.ModMask
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "ctrl+"):
			mods |= tcell.ModCtrl
			s = s[5:]
		case strings.HasPrefix(lower, "alt+"):
			mods |= tcell.ModAlt
			s = s[4:]
		case strings.HasPrefix(lower, "shift+"):
			mods |= tcell.ModShift
			s = s[6:]
		default:
			goto done
		}
	}
done:
	if s == "" {
		return parsedChord{}, fmt.Errorf("modifier with no key")
	}
	if strings.EqualFold(s, "space") {
		return parsedChord{key: tcell.KeyRune, ch: ' ', mods: mods}, nil
	}
	if k, ok := namedKeys[strings.ToLower(s)]; ok && k != tcell.KeyRune {
		return parsedChord{key: k, mods: mods}, nil
	}
	runes := []rune(s)
	if len(runes) != 1 {
		return parsedChord{}, fmt.Errorf("not a single key or a known key name")
	}
	r := runes[0]
	if mods&tcell.ModCtrl != 0 {
		// tcell reports Ctrl+letter as its own key constant with the rune
		// cleared, so a Ctrl binding has to be expressed that way to match.
		k, err := ctrlKeyFor(r)
		if err != nil {
			return parsedChord{}, err
		}
		return parsedChord{key: k, mods: tcell.ModCtrl}, nil
	}
	return parsedChord{key: tcell.KeyRune, ch: r, mods: mods}, nil
}

// ctrlKeyFor maps a letter onto tcell's Ctrl+<letter> key constant.
func ctrlKeyFor(r rune) (tcell.Key, error) {
	up := strings.ToUpper(string(r))
	if len(up) != 1 || up[0] < 'A' || up[0] > 'Z' {
		return 0, fmt.Errorf("ctrl+%s is not a control key (only letters have a control code)", string(r))
	}
	return tcell.KeyCtrlA + tcell.Key(up[0]-'A'), nil
}

// normalizeChord renders a spec in the canonical form used as a map key. An
// unparseable spec normalizes to itself, which simply never matches an event.
func normalizeChord(spec string) string {
	pc, err := parseChord(spec)
	if err != nil {
		return "?" + spec
	}
	return chordKey(pc.key, pc.ch, pc.mods)
}

// chordOf is normalizeChord for a live event.
func chordOf(ev *tcell.EventKey) string {
	return chordKey(ev.Key(), ev.Rune(), ev.Modifiers())
}

// chordKey builds the canonical map key. Only Ctrl and Alt are significant:
// Shift is already expressed in the rune ("S" vs "s"), and terminals do not
// report it consistently for anything else.
func chordKey(key tcell.Key, ch rune, mods tcell.ModMask) string {
	var b strings.Builder
	if mods&tcell.ModAlt != 0 {
		b.WriteString("alt-")
	}
	if key == tcell.KeyRune {
		if mods&tcell.ModCtrl != 0 {
			b.WriteString("ctrl-")
		}
		fmt.Fprintf(&b, "r:%c", ch)
		return b.String()
	}
	fmt.Fprintf(&b, "k:%d", int(key))
	return b.String()
}

// helpLines renders the keymap for the hotkey panel, two columns wide.
func (k keymap) helpLines() []string {
	out := make([]string, 0, len(k.help))
	for _, h := range k.help {
		out = append(out, fmt.Sprintf("  %-14s %s", h.Chord, h.Help))
	}
	return out
}
