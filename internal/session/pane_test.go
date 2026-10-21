package session

import (
	"sync"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

func bucket(name string) *model.Object {
	return &model.Object{Key: &name, Ot: model.Bucket}
}

func TestNewPaneAcceptsMarksImmediately(t *testing.T) {
	p := New()
	p.SetBucket(bucket("b"))
	// Before the pane owned its selection map this wrote to a nil map.
	p.ToggleSelected("x")
	if !p.IsSelected("x") {
		t.Fatal("a mark set on a fresh pane did not stick")
	}
}

// Enter is one write for the whole location, so a reader can never catch the
// new bucket paired with the old prefix.
func TestEnterSetsLocationAtomically(t *testing.T) {
	p := New()
	p.Enter(bucket("a"), "one/", "r1")
	p.Enter(bucket("b"), "two/", "r2")

	loc := p.Loc()
	if *loc.Bucket.Key != "b" || loc.Path != "two/" {
		t.Errorf("location = %s/%q, want b/two/", *loc.Bucket.Key, loc.Path)
	}
	if got := p.RestoreNext(); got != "r2" {
		t.Errorf("restoreNext = %q, want r2", got)
	}
}

func TestScopeKeySeparatesLocations(t *testing.T) {
	p := New()

	if got := p.ScopeKey(); got != "" {
		t.Errorf("the buckets screen should have the empty scope, got %q", got)
	}

	p.Enter(bucket("b"), "x/", "")
	first := p.ScopeKey()
	p.SetPath("y/")
	if second := p.ScopeKey(); second == first {
		t.Error("two prefixes in one bucket share a selection scope")
	}

	// A local pane must not share the buckets screen's empty scope, or its
	// marks would leak between the two.
	p.Enter(nil, "", "")
	p.SetLocalDir("/tmp/x")
	if got := p.ScopeKey(); got == "" {
		t.Error("a local pane fell back to the empty scope")
	}
	if got, want := p.ScopeKey(), LocalScopeKey("/tmp/x"); got != want {
		t.Errorf("local scope = %q, want %q", got, want)
	}
}

func TestMarksAreScopedToTheirListing(t *testing.T) {
	p := New()
	p.Enter(bucket("b"), "one/", "")
	p.ToggleSelected("file")
	if !p.IsSelected("file") {
		t.Fatal("mark did not stick")
	}

	p.SetPath("two/")
	if p.IsSelected("file") {
		t.Error("a mark leaked into another prefix")
	}
	if got := p.SelectedCount(); got != 0 {
		t.Errorf("count in the new scope = %d, want 0", got)
	}

	// Going back finds it again: marks are remembered per listing.
	p.SetPath("one/")
	if !p.IsSelected("file") {
		t.Error("returning to a listing lost its marks")
	}
}

func TestToggleAndClear(t *testing.T) {
	p := New()
	p.Enter(bucket("b"), "", "")

	p.SelectNames([]string{"a", "b", "c"})
	if got := p.SelectedCount(); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
	p.ToggleSelected("b") // already marked -> unmark
	if p.IsSelected("b") {
		t.Error("toggling a marked item did not unmark it")
	}
	p.Unselect("a")
	if p.IsSelected("a") {
		t.Error("Unselect left the mark")
	}
	if got := p.SelectedCount(); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	p.ClearSelected()
	if got := p.SelectedCount(); got != 0 {
		t.Errorf("count after clear = %d, want 0", got)
	}
}

// An operation can finish in a different listing than the one its items were
// marked in — a paste completing in the destination scope, say.
func TestClearSelectedInAnotherScope(t *testing.T) {
	p := New()
	p.Enter(bucket("b"), "src/", "")
	p.SelectNames([]string{"x"})
	srcScope := p.ScopeKey()

	p.SetPath("dst/")
	p.SelectNames([]string{"y"})

	p.ClearSelectedIn(srcScope)

	if !p.HasSelectionIn(p.ScopeKey()) {
		t.Error("clearing the source scope also cleared the destination's marks")
	}
	p.SetPath("src/")
	if p.SelectedCount() != 0 {
		t.Error("the source scope was not cleared")
	}
}

func TestSelectedSetIsACopy(t *testing.T) {
	p := New()
	p.Enter(bucket("b"), "", "")
	p.SelectNames([]string{"a"})

	set := p.SelectedSet()
	set["b"] = true // mutating the copy must not reach the pane

	if p.IsSelected("b") {
		t.Error("SelectedSet handed out the live map")
	}
}

func TestTakeRestoreNextConsumesIt(t *testing.T) {
	p := New()
	p.SetRestoreNext("row")
	if got := p.TakeRestoreNext(); got != "row" {
		t.Fatalf("take = %q, want row", got)
	}
	if got := p.TakeRestoreNext(); got != "" {
		t.Errorf("a second take returned %q; one listing consumed another's target", got)
	}
}

func TestSnapshotDoesNotAliasTheObjectMap(t *testing.T) {
	p := New()
	p.SetObjs(map[string]*model.Object{"a": nil, "b": nil})

	snap := p.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(snap))
	}
	p.SetObjs(map[string]*model.Object{})
	if len(snap) != 2 {
		t.Error("replacing the map changed a snapshot taken before it")
	}
}

func TestLookup(t *testing.T) {
	p := New()
	want := &model.Object{}
	p.SetObjs(map[string]*model.Object{"k": want})

	if got, ok := p.Lookup("k"); !ok || got != want {
		t.Errorf("Lookup(k) = %v, %v", got, ok)
	}
	if _, ok := p.Lookup("missing"); ok {
		t.Error("Lookup found an object that is not listed")
	}
}

func TestIsLocal(t *testing.T) {
	p := New()
	if p.IsLocal() {
		t.Error("a fresh pane is not local")
	}
	p.SetLocalDir("/tmp")
	if !p.IsLocal() {
		t.Error("a pane with a local dir should report local")
	}
}

// The navigation race was a background goroutine writing location fields while
// the UI goroutine read them. Under -race this catches a regression; without
// it, it at least exercises the locking.
func TestConcurrentNavigationAndReadsAreRaceFree(t *testing.T) {
	p := New()
	var wg sync.WaitGroup

	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			p.Enter(bucket("b"), "p/", "..")
			p.SetObjs(map[string]*model.Object{"k": nil})
			p.SetPartial(i%2 == 0)
			p.SetFilter("f")
			p.ToggleSelected("x")
		}()
		go func() {
			defer wg.Done()
			_ = p.Loc()
			_ = p.ScopeKey()
			_ = p.Snapshot()
			_ = p.Partial()
			_ = p.Filter()
			_ = p.SelectedCount()
		}()
	}
	wg.Wait()
}

func TestHistoryBackForward(t *testing.T) {
	v := func(p string) Visit { return Visit{Path: p} }
	var h History

	if _, ok := h.Back(v("cur")); ok {
		t.Error("Back on an empty history should fail")
	}
	if _, ok := h.Forward(v("cur")); ok {
		t.Error("Forward on an empty history should fail")
	}

	h.Record(v("a"))
	h.Record(v("b"))

	got, ok := h.Back(v("cur"))
	if !ok || got.Path != "b" {
		t.Fatalf("Back = %q, %v; want b, true", got.Path, ok)
	}
	fwd, ok := h.Forward(v("b"))
	if !ok || fwd.Path != "cur" {
		t.Fatalf("Forward = %q, %v; want cur, true", fwd.Path, ok)
	}

	// Recording a new branch clears the forward stack.
	h.Record(v("c"))
	if _, ok := h.Forward(v("x")); ok {
		t.Error("Record should clear the forward stack")
	}
}

func TestHistoryIsBounded(t *testing.T) {
	var h History
	for i := range HistMax + 50 {
		h.Record(Visit{Path: string(rune('a' + i%26))})
	}
	if back, _ := h.Depths(); back != HistMax {
		t.Errorf("back stack = %d entries, want it capped at %d", back, HistMax)
	}
}

// A pane's rows belong to the location they were listed at. Every action
// resolves a row through Lookup but acts in the pane's current bucket, so a
// listing that survived a move to another bucket paired bucket A's rows with
// bucket B: Delete on folder "logs/" still showing from A removed B's "logs/".
func TestRelocationDropsTheOldListing(t *testing.T) {
	logs := &model.Object{Ot: model.Folder}
	listed := func() map[string]*model.Object {
		return map[string]*model.Object{"logs/": logs}
	}
	moves := []struct {
		name string
		move func(p *Pane)
	}{
		{"Enter another bucket", func(p *Pane) { p.Enter(bucket("b"), "", "..") }},
		{"Enter the buckets screen", func(p *Pane) { p.Enter(nil, "", "") }},
		{"SetBucket", func(p *Pane) { p.SetBucket(bucket("b")) }},
		{"SetPath", func(p *Pane) { p.SetPath("logs/") }},
		{"SetLocalDir", func(p *Pane) { p.SetLocalDir("/tmp") }},
		{"SetLoc", func(p *Pane) { p.SetLoc(Location{Bucket: bucket("b")}) }},
	}
	for _, tc := range moves {
		p := New()
		p.Enter(bucket("a"), "", "")
		p.SetListing(p.Loc(), listed(), true)

		tc.move(p)
		if _, ok := p.Lookup("logs/"); ok {
			t.Errorf("%s: a row listed at the old location still resolves", tc.name)
		}
		if p.Partial() {
			t.Errorf("%s: the old listing's partial flag survived", tc.name)
		}
	}

	// Staying put keeps the rows: a refresh must not blank the pane, and
	// re-entering a bucket builds a fresh *Object for the same name.
	p := New()
	p.Enter(bucket("a"), "x/", "")
	p.SetListing(p.Loc(), listed(), false)
	p.Enter(bucket("a"), "x/", "..")
	p.SetPath("x/")
	if _, ok := p.Lookup("logs/"); !ok {
		t.Error("re-entering the same location dropped its listing")
	}
}

// A listing that lands after the pane moved on — a cancelled one keeps what
// arrived — must not be filed under the new location.
func TestSetListingRefusesAStaleLocation(t *testing.T) {
	p := New()
	p.Enter(bucket("a"), "", "")
	at := p.Loc()

	p.Enter(bucket("b"), "", "")
	if p.SetListing(at, map[string]*model.Object{"logs/": {}}, true) {
		t.Fatal("SetListing stored a listing taken at a location the pane has left")
	}
	if _, ok := p.Lookup("logs/"); ok || p.Partial() {
		t.Error("a refused listing still reached the pane")
	}

	if !p.SetListing(p.Loc(), map[string]*model.Object{"k": {}}, true) {
		t.Fatal("SetListing refused the pane's own location")
	}
	if _, ok := p.Lookup("k"); !ok || !p.Partial() {
		t.Error("an accepted listing was not stored with its partial flag")
	}
}

// UnselectIn drops a mark where it was made, whichever listing is showing:
// trash restore finishes after the user may have navigated away.
func TestUnselectInTargetsItsOwnScope(t *testing.T) {
	p := New()
	p.Enter(bucket("b"), "trash/", "")
	p.ToggleSelected("trash/x")
	scope := p.ScopeKey()

	p.SetPath("elsewhere/")
	p.ToggleSelected("trash/x") // the same name, marked in another listing
	p.UnselectIn(scope, "trash/x")

	if p.HasSelectionIn(scope) {
		t.Error("the mark in the original scope survived")
	}
	if !p.IsSelected("trash/x") {
		t.Error("UnselectIn reached into the scope now showing")
	}
}
