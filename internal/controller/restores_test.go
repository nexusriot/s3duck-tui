package controller

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

const restoredHeader = `ongoing-request="false", expiry-date="Fri, 23 Oct 2026 00:00:00 GMT"`

func TestNextRestoreStateTransitions(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	glacier := func(restore string) model.ObjectMeta {
		return model.ObjectMeta{StorageClass: "GLACIER", Restore: restore}
	}
	pending := restoreEntry{Profile: "p", Bucket: "b", Key: "k", State: restorePending}
	ready := pending
	ready.State = restoreReady

	cases := []struct {
		name      string
		from      restoreEntry
		meta      model.ObjectMeta
		err       error
		want      restoreState
		wantReady bool
	}{
		{"still running", pending, glacier(`ongoing-request="true"`), nil, restorePending, false},
		{"just finished", pending, glacier(restoredHeader), nil, restoreReady, true},
		{"still readable", ready, glacier(restoredHeader), nil, restoreReady, false},
		{"copy expired", ready, glacier(""), nil, restoreExpired, false},
		{"never started", pending, glacier(""), nil, restoreNone, false},
		{"left the archive", pending, model.ObjectMeta{StorageClass: "STANDARD"}, nil, restoreNotArchived, false},
		{"object deleted", pending, model.ObjectMeta{}, &smithy.GenericAPIError{Code: "NotFound"}, restoreGone, false},
		{"check failed", pending, model.ObjectMeta{}, errors.New("timeout"), restoreUnknown, false},
	}
	for _, c := range cases {
		next, ready := nextRestoreState(c.from, c.meta, c.err, now)
		if next.State != c.want || ready != c.wantReady {
			t.Errorf("%s: state %q ready %v, want %q %v", c.name, next.State, ready, c.want, c.wantReady)
		}
		if !next.Checked.Equal(now) {
			t.Errorf("%s: Checked not stamped", c.name)
		}
		if c.want == restoreReady && next.Expiry == "" {
			t.Errorf("%s: the expiry should be kept for the panel", c.name)
		}
		if c.want == restoreUnknown && next.Err == "" {
			t.Errorf("%s: a failed check should say why", c.name)
		}
	}
}

func TestRestoreDue(t *testing.T) {
	now := time.Now()
	if !(restoreEntry{State: restorePending, Checked: now}).due(now) {
		t.Error("a pending restore is checked every round")
	}
	if (restoreEntry{State: restoreReady, Checked: now.Add(-time.Minute)}).due(now) {
		t.Error("a ready restore is only rechecked now and then")
	}
	if !(restoreEntry{State: restoreReady, Checked: now.Add(-restoreReadyRecheck)}).due(now) {
		t.Error("a ready restore must eventually be rechecked, to see it expire")
	}
	for _, s := range []restoreState{restoreExpired, restoreGone, restoreNone, restoreNotArchived} {
		if (restoreEntry{State: s}).due(now) {
			t.Errorf("%q is final and should not be checked", s)
		}
	}
}

func TestRestoreTrackerPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", restoresFileName)
	tr := loadRestoreTracker(path)
	tr.track(restoreEntry{Profile: "p", Bucket: "b", Key: "a", Requested: time.Unix(100, 0)})
	tr.track(restoreEntry{Profile: "p", Bucket: "b", Key: "z", Requested: time.Unix(200, 0)})
	tr.track(restoreEntry{Profile: "q", Bucket: "b", Key: "a", Requested: time.Unix(300, 0)})
	// A second request for the same object replaces the first.
	tr.track(restoreEntry{Profile: "p", Bucket: "b", Key: "a", Requested: time.Unix(400, 0), Tier: "Expedited"})

	again := loadRestoreTracker(path)
	got := again.snapshot()
	if len(got) != 3 {
		t.Fatalf("reloaded %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Key != "a" || got[0].Profile != "p" || got[0].Tier != "Expedited" || got[0].State != restorePending {
		t.Errorf("newest first, re-request replaced, default state pending: %+v", got[0])
	}
	if st, ok := again.state("q", "b", "a"); !ok || st != restorePending {
		t.Errorf("profiles are kept apart: %q %v", st, ok)
	}
	if due := again.dueFor("p", time.Now(), 10); len(due) != 2 {
		t.Errorf("dueFor(p) = %d entries, want 2", len(due))
	}
	if due := again.dueFor("p", time.Now(), 1); len(due) != 1 {
		t.Errorf("dueFor must respect its cap, got %d", len(due))
	}
}

func TestRestoreTrackerClearAndRemove(t *testing.T) {
	tr := loadRestoreTracker(filepath.Join(t.TempDir(), restoresFileName))
	for i, s := range []restoreState{restorePending, restoreReady, restoreExpired, restoreGone, restoreUnknown} {
		tr.track(restoreEntry{Profile: "p", Bucket: "b", Key: string(rune('a' + i)), State: s})
	}
	if n := tr.clearFinished(); n != 3 {
		t.Errorf("cleared %d, want ready + expired + gone = 3", n)
	}
	left := tr.snapshot()
	if len(left) != 2 {
		t.Fatalf("left %+v", left)
	}
	tr.remove(left[0])
	if len(tr.snapshot()) != 1 {
		t.Error("remove dropped nothing")
	}
	// An update for an entry the user removed meanwhile must not resurrect it.
	tr.update(left[0])
	if len(tr.snapshot()) != 1 {
		t.Error("an update brought a removed entry back")
	}
}

func TestLoadRestoreTrackerToleratesAMissingOrBrokenFile(t *testing.T) {
	if got := loadRestoreTracker(filepath.Join(t.TempDir(), "nope.json")).snapshot(); len(got) != 0 {
		t.Errorf("missing file: %v", got)
	}
	if got := loadRestoreTracker("").snapshot(); len(got) != 0 {
		t.Errorf("no path: %v", got)
	}
}

func TestRestoreStateInTheClassColumn(t *testing.T) {
	cols := listColumns{nameWidth: 20, size: true, date: true, class: true}
	when := colTime(t)
	row := stripTags(listRowRestore(colFile("cold.bin", 10, when, "GLACIER"), false, cols, restoreReady))
	if !strings.HasSuffix(row, "restored") || strings.Contains(row, "GLACIER") {
		t.Errorf("a ready restore should replace the class: %q", row)
	}
	row = stripTags(listRowRestore(colFile("cold.bin", 10, when, "GLACIER"), false, cols, restoreExpired))
	if !strings.HasSuffix(row, "GLACIER") {
		t.Errorf("states with nothing to say leave the class alone: %q", row)
	}
	if len(stripTags(restoreCell(restorePending))) > colClassWidth {
		t.Error("the restore cell must fit the class column")
	}
}
