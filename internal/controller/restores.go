package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// The Glacier restore tracker. A restore is asynchronous on S3's side —
// minutes for Expedited, hours for Standard, up to two days for Bulk — and
// before this the app's whole answer was "check the status with m or c". The
// tracker remembers every restore requested here (or seen in progress), checks
// them with a HEAD while their profile is open, rings the bell when one is
// readable, and marks the object in the listing.
//
// It persists, because the wait outlives a session: the list is state, kept
// beside the activity log rather than in config.json.

// restoresFileName is the tracker's file under the state directory.
const restoresFileName = "restores.json"

// RestoresPath is where the tracked restores live: beside the activity log.
func RestoresPath(homeDir string) string {
	return filepath.Join(filepath.Dir(AuditPath(homeDir)), restoresFileName)
}

// restorePollEvery is how often the open profile's restores are checked. An
// Expedited restore is ready within minutes, so an hour would be too coarse;
// one HEAD per tracked object per minute costs nothing worth saving.
const restorePollEvery = time.Minute

// restoreReadyRecheck is how often a restore that is already readable is
// looked at again — only to notice that its temporary copy has expired.
const restoreReadyRecheck = 30 * time.Minute

// restoreCheckMax bounds one round of checks, so a profile with thousands of
// tracked objects cannot turn every minute into a burst of HEADs.
const restoreCheckMax = 100

// restoreState is where one tracked restore stands.
type restoreState string

const (
	restorePending restoreState = "in progress"
	restoreReady   restoreState = "ready"
	// restoreExpired: it was readable, and the temporary copy is gone again.
	restoreExpired restoreState = "expired"
	// restoreNone: the object is archived and no restore is running — the
	// request was never accepted, or happened outside S3's memory of it.
	restoreNone restoreState = "not restoring"
	// restoreNotArchived: the object left the archive tier (rewritten or
	// copied over), so there is nothing to restore any more.
	restoreNotArchived restoreState = "not archived"
	restoreGone        restoreState = "object gone"
	// restoreUnknown: the last check failed; Err says why. It keeps being
	// checked.
	restoreUnknown restoreState = "check failed"
)

// final reports a state no further check can change.
func (s restoreState) final() bool {
	switch s {
	case restoreExpired, restoreNone, restoreNotArchived, restoreGone:
		return true
	}
	return false
}

// restoreEntry is one tracked restore. JSON-tagged: it is the file format.
type restoreEntry struct {
	Profile   string       `json:"profile"`
	Bucket    string       `json:"bucket"`
	Key       string       `json:"key"`
	Tier      string       `json:"tier,omitempty"`
	Days      int32        `json:"days,omitempty"`
	Requested time.Time    `json:"requested"`
	State     restoreState `json:"state"`
	// Expiry is the restored copy's expiry as S3 reports it.
	Expiry  string    `json:"expiry,omitempty"`
	Checked time.Time `json:"checked,omitempty"`
	Err     string    `json:"error,omitempty"`
}

func (e restoreEntry) id() string { return e.Profile + "\x00" + e.Bucket + "\x00" + e.Key }

// due reports whether the entry should be checked in a round at now.
func (e restoreEntry) due(now time.Time) bool {
	switch {
	case e.State.final():
		return false
	case e.State == restoreReady:
		return now.Sub(e.Checked) >= restoreReadyRecheck
	}
	return true
}

// nextRestoreState folds one HEAD result into an entry. It is the tracker's
// whole judgement, kept free of I/O so every transition is testable. ready is
// true exactly when this check is the one that saw the restore complete — the
// moment worth a bell.
func nextRestoreState(e restoreEntry, meta model.ObjectMeta, err error, now time.Time) (next restoreEntry, ready bool) {
	next = e
	next.Checked = now
	next.Err = ""
	switch {
	case err != nil && model.IsNotFound(err):
		next.State = restoreGone
		return next, false
	case err != nil:
		next.State = restoreUnknown
		next.Err = err.Error()
		return next, false
	}

	ongoing, expiry, ok := model.ParseRestoreStatus(meta.Restore)
	switch {
	case ok && ongoing:
		next.State = restorePending
	case ok:
		next.State = restoreReady
		next.Expiry = expiry
		return next, e.State != restoreReady
	case !model.IsArchived(meta.StorageClass):
		next.State = restoreNotArchived
	case e.State == restoreReady:
		next.State = restoreExpired
	default:
		next.State = restoreNone
	}
	return next, false
}

// restoreTracker is the list and its file. Safe from any goroutine.
type restoreTracker struct {
	mu      sync.Mutex
	path    string
	entries []restoreEntry
}

// restoresFile is the JSON shape of the tracker's file.
type restoresFile struct {
	Restores []restoreEntry `json:"restores"`
}

// loadRestoreTracker reads the file. A missing one is the normal first run; an
// unreadable one starts the list empty — it is a convenience cache of pending
// waits, not data whose loss costs anything but a re-request of the status.
func loadRestoreTracker(path string) *restoreTracker {
	t := &restoreTracker{path: path}
	if path == "" {
		return t
	}
	if data, err := os.ReadFile(path); err == nil {
		var f restoresFile
		if json.Unmarshal(data, &f) == nil {
			t.entries = f.Restores
		}
	}
	return t
}

// saveLocked writes the list atomically. The caller holds mu. Errors are
// dropped for the reason the activity log drops them: a tracker that cannot
// be written must not interrupt the restore it describes.
func (t *restoreTracker) saveLocked() {
	if t.path == "" {
		return
	}
	data, err := json.MarshalIndent(restoresFile{Restores: t.entries}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(t.path), restoresFileName+".*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), t.path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// track adds a restore, or restarts the tracking of one already listed (a
// second request for the same object replaces the first).
func (t *restoreTracker) track(e restoreEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.State == "" {
		e.State = restorePending
	}
	for i := range t.entries {
		if t.entries[i].id() == e.id() {
			t.entries[i] = e
			t.saveLocked()
			return
		}
	}
	t.entries = append(t.entries, e)
	t.saveLocked()
}

// tracked reports whether an object is on the list at all.
func (t *restoreTracker) tracked(profile, bucket, key string) bool {
	_, ok := t.state(profile, bucket, key)
	return ok
}

// state is one object's tracked state, for the listing.
func (t *restoreTracker) state(profile, bucket, key string) (restoreState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := restoreEntry{Profile: profile, Bucket: bucket, Key: key}.id()
	for _, e := range t.entries {
		if e.id() == want {
			return e.State, true
		}
	}
	return "", false
}

// snapshot copies the list out, newest request first.
func (t *restoreTracker) snapshot() []restoreEntry {
	t.mu.Lock()
	out := make([]restoreEntry, len(t.entries))
	copy(out, t.entries)
	t.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Requested.After(out[j].Requested) })
	return out
}

// remove drops one entry.
func (t *restoreTracker) remove(e restoreEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.entries[:0]
	for _, x := range t.entries {
		if x.id() != e.id() {
			kept = append(kept, x)
		}
	}
	t.entries = kept
	t.saveLocked()
}

// clearFinished drops every entry in a final state, plus the ready ones —
// whatever needs no more watching.
func (t *restoreTracker) clearFinished() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.entries[:0]
	for _, x := range t.entries {
		if !x.State.final() && x.State != restoreReady {
			kept = append(kept, x)
		}
	}
	n := len(t.entries) - len(kept)
	t.entries = kept
	t.saveLocked()
	return n
}

// dueFor lists the entries of one profile a round should check.
func (t *restoreTracker) dueFor(profile string, now time.Time, max int) []restoreEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []restoreEntry
	for _, e := range t.entries {
		if e.Profile == profile && e.due(now) {
			out = append(out, e)
			if len(out) == max {
				break
			}
		}
	}
	return out
}

// update replaces an entry with a newer version of itself. One the user
// removed while it was being checked stays removed.
func (t *restoreTracker) update(e restoreEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.entries {
		if t.entries[i].id() == e.id() {
			t.entries[i] = e
			t.saveLocked()
			return
		}
	}
}

// trackRestore records a restore. The profile is passed in, read on the UI
// goroutine by the caller, because this runs after the request returns.
func (c *Controller) trackRestore(profile, bucket, key string, days int32, tier string, state restoreState) {
	if c.restores == nil || profile == "" {
		return
	}
	c.restores.track(restoreEntry{
		Profile: profile, Bucket: bucket, Key: key,
		Days: days, Tier: tier, Requested: time.Now(), State: state,
	})
}

// profileName is the open profile's name, or "" on the profiles screen.
func (c *Controller) profileName() string {
	if c.activeConfig == nil {
		return ""
	}
	return c.activeConfig.Name
}

// listRestoreState is the tracked state of a listed object, for its row.
// Called on the UI goroutine from the render.
func (c *Controller) listRestoreState(o *model.Object) restoreState {
	if c.restores == nil || c.activeConfig == nil || o == nil || o.Ot != model.File || o.FullPath == nil {
		return ""
	}
	b := c.pane().Bucket()
	if b == nil || b.Key == nil {
		return ""
	}
	st, _ := c.restores.state(c.activeConfig.Name, *b.Key, *o.FullPath)
	return st
}

// runRestorePoller checks the open profile's restores every restorePollEvery,
// for the app's lifetime. The tick only *schedules* a round on the UI
// goroutine, where the open profile and its client can be read without a
// race; the HEADs themselves run off it.
func (c *Controller) runRestorePoller() {
	go func() {
		t := time.NewTicker(restorePollEvery)
		defer t.Stop()
		for range t.C {
			c.shell.Do(c.scheduleRestoreCheck)
		}
	}()
}

// scheduleRestoreCheck starts one round for the open profile, if there is
// one. UI goroutine.
func (c *Controller) scheduleRestoreCheck() {
	if !c.browsing || c.activeConfig == nil || c.model == nil || c.restores == nil {
		return
	}
	go c.checkRestores(c.model, c.activeConfig.Name)
}

// checkRestores runs one round: a HEAD per due entry, the result folded in,
// and a notice + bell for every restore that has just become readable.
func (c *Controller) checkRestores(mdl *model.Model, profile string) {
	c.restoreCheckMu.Lock()
	defer c.restoreCheckMu.Unlock()

	changed := false
	for _, e := range c.restores.dueFor(profile, time.Now(), restoreCheckMax) {
		bucketName := e.Bucket
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		meta, err := mdl.HeadObject(ctx, &model.Object{Key: &bucketName, Ot: model.Bucket}, e.Key)
		cancel()
		next, ready := nextRestoreState(e, meta, err, time.Now())
		c.restores.update(next)
		if next.State != e.State {
			changed = true
		}
		if ready {
			until := ""
			if next.Expiry != "" {
				until = " until " + next.Expiry
			}
			c.logActivity("Restore ready: s3://%s/%s%s", e.Bucket, e.Key, until)
			c.queue.SetNotice(fmt.Sprintf(" ✓ restored: %s ", path.Base(e.Key)))
			c.shell.Do(c.view.Beep)
		}
	}
	if changed {
		// The class column shows the state; a re-render is enough (no
		// listing round trip).
		go c.renderList()
	}
}

// restoreRow renders one tracked restore for the panel.
func restoreRow(e restoreEntry, current bool) (primary, secondary string) {
	color := "[white]"
	switch e.State {
	case restoreReady:
		color = "[green]"
	case restorePending:
		color = "[yellow]"
	case restoreUnknown, restoreGone, restoreNone:
		color = "[red]"
	case restoreExpired, restoreNotArchived:
		color = "[gray]"
	}
	primary = fmt.Sprintf("%s%-14s[-] %s", color, e.State, tview.Escape("s3://"+e.Bucket+"/"+e.Key))
	var parts []string
	if !current {
		parts = append(parts, "profile "+e.Profile+" (checked while it is open)")
	}
	if e.Tier != "" {
		parts = append(parts, fmt.Sprintf("%s, %d day(s)", e.Tier, e.Days))
	}
	parts = append(parts, "requested "+e.Requested.Local().Format(colDateLayout))
	if e.Expiry != "" {
		parts = append(parts, "readable until "+e.Expiry)
	}
	if !e.Checked.IsZero() {
		parts = append(parts, "checked "+e.Checked.Local().Format("15:04"))
	}
	if e.Err != "" {
		parts = append(parts, e.Err)
	}
	return primary, tview.Escape(strings.Join(parts, " · "))
}

// ShowRestores opens the tracker: every tracked restore, this profile's
// first. Enter reveals the object, r checks now, Del forgets one, c clears
// the finished ones.
func (c *Controller) ShowRestores() {
	if c.restores == nil {
		return
	}
	profile := ""
	if c.activeConfig != nil {
		profile = c.activeConfig.Name
	}

	// lay is assigned at the bottom; every handler below runs after that.
	var lay *shell.Layer
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(" Glacier restores — Enter: reveal • r: check now • Del: forget • c: clear finished • Esc ")
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	var shown []restoreEntry
	fill := func() {
		all := c.restores.snapshot()
		// This profile's restores first: they are the ones that can be
		// checked and revealed from here.
		sort.SliceStable(all, func(i, j int) bool {
			return all[i].Profile == profile && all[j].Profile != profile
		})
		shown = all
		cur := list.GetCurrentItem()
		list.Clear()
		if len(shown) == 0 {
			list.AddItem("[gray](no restores tracked — request one with c on an archived object)[-]", "", 0, nil)
			return
		}
		for _, e := range shown {
			p, s := restoreRow(e, e.Profile == profile)
			list.AddItem(p, s, 0, nil)
		}
		if cur >= 0 && cur < len(shown) {
			list.SetCurrentItem(cur)
		}
	}
	fill()

	at := func() (restoreEntry, bool) {
		i := list.GetCurrentItem()
		if i < 0 || i >= len(shown) {
			return restoreEntry{}, false
		}
		return shown[i], true
	}
	list.SetSelectedFunc(func(int, string, string, rune) {
		e, ok := at()
		if !ok {
			return
		}
		if e.Profile != profile {
			c.error("Restores", fmt.Errorf("%s belongs to profile %s; open that profile to reveal it", e.Key, e.Profile))
			return
		}
		lay.Close()
		c.jumpTo(e.Bucket, parentPrefix(e.Key), e.Key)
	})
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			lay.Close()
			return nil
		case tcell.KeyDelete:
			if e, ok := at(); ok {
				c.restores.remove(e)
				fill()
			}
			return nil
		case tcell.KeyRune:
			switch ev.Rune() {
			case 'c':
				n := c.restores.clearFinished()
				fill()
				c.queue.SetNotice(fmt.Sprintf(" cleared %d finished restore(s) ", n))
				return nil
			case 'r':
				if c.activeConfig == nil || c.model == nil {
					return nil
				}
				mdl, name := c.model, c.activeConfig.Name
				// "Now" means now: ready ones too, whatever their recheck
				// interval says.
				for _, e := range c.restores.snapshot() {
					if e.Profile == name && e.State == restoreReady {
						e.Checked = time.Time{}
						c.restores.update(e)
					}
				}
				go func() {
					c.checkRestores(mdl, name)
					c.shell.Do(fill)
				}()
				return nil
			}
		}
		return ev
	})

	lay = c.pushModal(list, 110, 24)
	c.view.App.SetFocus(list)
}
