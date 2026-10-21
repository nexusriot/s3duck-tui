// Package session holds a browser pane's state: where it is pointed, what it
// listed there, what is marked, and how it got there.
//
// It exists to make a pane *addressable*. The controller used to keep the
// active pane's state in its own fields and snapshot the inactive one into an
// array, copying twelve fields each way on every swap. Navigation goroutines
// wrote those same fields with no lock at all, so a Tab during a slow
// bucket-open could land the navigation in the other pane — the "navigation
// race" in DESIGN.md's limitations table.
//
// Here each pane is one object with one mutex. A flow captures the *Pane it
// started on and writes to that, so a swap cannot redirect it, and every field
// a background goroutine touches is behind the lock.
//
// Nothing in this package knows about tview.
package session

import (
	"sync"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// Location is where a pane is pointed. A nil Bucket means the buckets screen;
// a non-empty LocalDir means this pane browses the filesystem instead of S3.
type Location struct {
	Bucket   *model.Object
	Path     string
	LocalDir string
}

// Pane is one browser pane. Every field but hist is behind mu: the UI
// goroutine reads them while listing, transfer and navigation goroutines write
// them. hist is not locked — Hist hands out a pointer — and is only touched by
// the controller's navigation handlers, on the UI goroutine.
type Pane struct {
	mu sync.Mutex

	loc     Location
	buckets []*model.Object
	objs    map[string]*model.Object

	// selectedByScope holds the marks per listing. The scope key keeps marks
	// from leaking between locations — see ScopeKey.
	selectedByScope map[string]map[string]bool

	filter      string
	partial     bool
	restoreNext string
	bucketPos   int

	hist History
}

// New builds an empty pane with its selection map ready, so a mark recorded
// before the first listing cannot write to a nil map.
func New() *Pane {
	return &Pane{selectedByScope: make(map[string]map[string]bool)}
}

// ---------- location ----------

// Loc returns where the pane is pointed.
func (p *Pane) Loc() Location {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loc
}

// SetLoc repoints the pane. Callers that only change part of the location use
// the narrower setters below.
func (p *Pane) SetLoc(l Location) {
	p.mu.Lock()
	p.relocateLocked(l)
	p.mu.Unlock()
}

// relocateLocked points the pane at l and, when that is a different listing,
// drops the objects listed at the old one. Callers must hold mu.
//
// The object map is what every action resolves a row against — Lookup turns
// the highlighted or marked name into the object to delete, move or download —
// while the bucket it acts in comes from loc. Keeping the old map across a
// move to another bucket paired the previous bucket's rows with the new
// bucket: until the new listing landed (or for good, when it failed), Delete
// on a folder still showing from bucket A removed the same-named prefix in
// bucket B. An empty map makes a stale row resolve to nothing instead.
func (p *Pane) relocateLocked(l Location) {
	if !SameLoc(p.loc, l) {
		p.objs = nil
		p.partial = false
	}
	p.loc = l
}

// SameLoc reports whether two locations name the same listing. Buckets compare
// by name: navigation builds a fresh *model.Object for a bucket it re-enters.
func SameLoc(a, b Location) bool {
	return bucketName(a.Bucket) == bucketName(b.Bucket) &&
		a.Path == b.Path && a.LocalDir == b.LocalDir
}

func bucketName(b *model.Object) string {
	if b == nil || b.Key == nil {
		return ""
	}
	return *b.Key
}

// Bucket is the open bucket, or nil on the buckets screen.
func (p *Pane) Bucket() *model.Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loc.Bucket
}

func (p *Pane) SetBucket(b *model.Object) {
	p.mu.Lock()
	l := p.loc
	l.Bucket = b
	p.relocateLocked(l)
	p.mu.Unlock()
}

// Path is the prefix within the open bucket.
func (p *Pane) Path() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loc.Path
}

func (p *Pane) SetPath(s string) {
	p.mu.Lock()
	l := p.loc
	l.Path = s
	p.relocateLocked(l)
	p.mu.Unlock()
}

// Enter points the pane at a bucket and prefix in one step. Navigation
// goroutines use it so a reader can never observe the new bucket with the old
// prefix.
func (p *Pane) Enter(b *model.Object, path, restore string) {
	p.mu.Lock()
	l := p.loc
	l.Bucket = b
	l.Path = path
	p.relocateLocked(l)
	p.restoreNext = restore
	p.mu.Unlock()
}

// LocalDir is non-empty when this pane browses the filesystem.
func (p *Pane) LocalDir() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loc.LocalDir
}

func (p *Pane) SetLocalDir(d string) {
	p.mu.Lock()
	l := p.loc
	l.LocalDir = d
	p.relocateLocked(l)
	p.mu.Unlock()
}

// IsLocal reports whether this pane is a filesystem browser.
func (p *Pane) IsLocal() bool { return p.LocalDir() != "" }

// ScopeKey identifies the listing the pane is showing, which is what selection
// marks are filed under. A local pane gets its own scope: sharing the empty
// one with the buckets screen would make its marks leak between the two.
func (p *Pane) ScopeKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return scopeKey(p.loc)
}

func scopeKey(l Location) string {
	if l.LocalDir != "" {
		return "local:" + l.LocalDir
	}
	if l.Bucket == nil || l.Bucket.Key == nil {
		return ""
	}
	return *l.Bucket.Key + ":" + l.Path
}

// LocalScopeKey is the scope a local pane at dir would use. Exported because
// the controller files marks for a local pane before it owns one.
func LocalScopeKey(dir string) string { return "local:" + dir }

// ---------- listing ----------

// Objs returns the pane's object map. The map itself is handed back, so
// callers must not mutate it; use SetObjs to replace it wholesale.
func (p *Pane) Objs() map[string]*model.Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.objs
}

func (p *Pane) SetObjs(m map[string]*model.Object) {
	p.mu.Lock()
	p.objs = m
	p.mu.Unlock()
}

// SetListing stores a listing taken at l, with its partial flag — but only if
// the pane is still pointed there, and reports whether it did. A listing that
// finishes after the pane has moved on (a cancelled one keeps what arrived)
// must not land under the new location, where its rows would resolve against
// the wrong bucket; the newer location's own listing follows anyway.
func (p *Pane) SetListing(l Location, m map[string]*model.Object, partial bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !SameLoc(p.loc, l) {
		return false
	}
	p.objs = m
	p.partial = partial
	return true
}

// Lookup finds one listed object by its key.
func (p *Pane) Lookup(name string) (*model.Object, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.objs[name]
	return o, ok
}

// Snapshot copies the listed objects out for rendering, so the caller can sort
// and filter them without holding the lock.
func (p *Pane) Snapshot() []*model.Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*model.Object, 0, len(p.objs))
	for _, o := range p.objs {
		out = append(out, o)
	}
	return out
}

func (p *Pane) Buckets() []*model.Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buckets
}

func (p *Pane) SetBuckets(b []*model.Object) {
	p.mu.Lock()
	p.buckets = b
	p.mu.Unlock()
}

// Partial records that this pane's listing is incomplete — cancelled, or
// capped — so the title can say so.
func (p *Pane) Partial() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.partial
}

func (p *Pane) SetPartial(v bool) {
	p.mu.Lock()
	p.partial = v
	p.mu.Unlock()
}

// Filter is the active in-listing name filter ("" means none).
func (p *Pane) Filter() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.filter
}

func (p *Pane) SetFilter(s string) {
	p.mu.Lock()
	p.filter = s
	p.mu.Unlock()
}

// RestoreNext is the key the cursor should land on after the next listing.
func (p *Pane) RestoreNext() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restoreNext
}

func (p *Pane) SetRestoreNext(s string) {
	p.mu.Lock()
	p.restoreNext = s
	p.mu.Unlock()
}

// TakeRestoreNext returns the pending cursor target and clears it, so one
// listing cannot consume another's.
func (p *Pane) TakeRestoreNext() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.restoreNext
	p.restoreNext = ""
	return s
}

// BucketPos remembers the cursor row on the buckets screen.
func (p *Pane) BucketPos() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bucketPos
}

func (p *Pane) SetBucketPos(i int) {
	p.mu.Lock()
	p.bucketPos = i
	p.mu.Unlock()
}

// ---------- selection ----------

// selLocked returns the mark set for the current scope, creating it on first
// use. Callers must hold mu.
func (p *Pane) selLocked() map[string]bool {
	s := scopeKey(p.loc)
	if s == "" {
		return nil
	}
	m, ok := p.selectedByScope[s]
	if !ok {
		m = make(map[string]bool)
		p.selectedByScope[s] = m
	}
	return m
}

func (p *Pane) IsSelected(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.selLocked()
	return m != nil && m[name]
}

func (p *Pane) ToggleSelected(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.selLocked()
	if m == nil {
		return
	}
	if m[name] {
		delete(m, name)
	} else {
		m[name] = true
	}
}

func (p *Pane) ClearSelected() {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.selLocked()
	for k := range m {
		delete(m, k)
	}
}

// ClearSelectedIn clears the marks filed under a specific scope — used when an
// operation finishes in a different listing than the one its items were marked
// in (a paste completing in the destination scope, say).
func (p *Pane) ClearSelectedIn(scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.selectedByScope[scope]; ok {
		for k := range m {
			delete(m, k)
		}
	}
}

func (p *Pane) SelectNames(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.selLocked()
	if m == nil {
		return
	}
	for _, n := range names {
		m[n] = true
	}
}

// Unselect drops one mark from the current scope — used when the item behind
// it is gone (deleted), so the "Selected: N" count stays truthful.
func (p *Pane) Unselect(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.selLocked(); m != nil {
		delete(m, name)
	}
}

// UnselectIn drops one mark from a given scope — for an operation that
// finishes after the user may have navigated away from where it was marked.
func (p *Pane) UnselectIn(scope, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.selectedByScope[scope]; ok {
		delete(m, name)
	}
}

// SelectedSet copies the marks in the current scope out, so the caller can
// iterate without holding the lock.
func (p *Pane) SelectedSet() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.selLocked()
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SelectedCount is how many items are marked in the current scope.
func (p *Pane) SelectedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.selLocked())
}

// HasSelectionIn reports whether a given scope holds any marks.
func (p *Pane) HasSelectionIn(scope string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.selectedByScope[scope]) > 0
}

// ---------- history ----------

// Hist is the pane's back/forward stack. It is only ever touched on the UI
// goroutine (navigation commands), so it is handed out directly.
func (p *Pane) Hist() *History { return &p.hist }
