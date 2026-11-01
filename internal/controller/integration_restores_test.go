//go:build integration

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// The restore tracker's reading of what a real backend answers. MinIO has no
// archive tier, so the interesting states here are the ones that say "stop
// waiting for this" — and the one that must never be reached by accident.
func TestIntegrationRestoreTrackerAgainstARealBackend(t *testing.T) {
	m := itModel(t)
	ctx := context.Background()
	b := itBucket(t, m, "s3duck-it-restore-corner")
	itPut(t, m, b, "plain.txt", "x")

	pending := restoreEntry{Profile: "p", Bucket: *b.Key, Key: "plain.txt", State: restorePending}
	now := time.Now()

	// A plain object that is not in an archive class: nothing to wait for.
	meta, err := m.HeadObject(ctx, b, "plain.txt")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	next, ready := nextRestoreState(pending, meta, err, now)
	if next.State != restoreNotArchived || ready {
		t.Errorf("a STANDARD object: state %q ready %v, want %q false", next.State, ready, restoreNotArchived)
	}

	// A key that is gone. This is the one that depends on the backend's error
	// shape: a HEAD carries no body, so MinIO answers with an untyped API
	// error, and model.IsNotFound has to recognise it. Getting this wrong
	// leaves the entry retrying a deleted object every minute forever.
	gone := pending
	gone.Key = "no-such-key.txt"
	meta, err = m.HeadObject(ctx, b, "no-such-key.txt")
	if err == nil {
		t.Fatal("HeadObject of a missing key returned no error")
	}
	if !model.IsNotFound(err) {
		t.Errorf("model.IsNotFound did not recognise this backend's 404: %v", err)
	}
	next, _ = nextRestoreState(gone, meta, err, now)
	if next.State != restoreGone {
		t.Errorf("a missing key: state %q, want %q (err was %v)", next.State, restoreGone, err)
	}

	// A restore request this backend cannot serve must fail loudly rather
	// than leaving the user waiting on a restore that was never accepted.
	if err := m.RestoreObject(ctx, b, "plain.txt", 1, "Standard"); err == nil {
		t.Log("note: this endpoint accepted a restore of a STANDARD object")
	} else {
		t.Logf("restore refused as expected: %v", err)
		unknown, _ := nextRestoreState(pending, model.ObjectMeta{}, err, now)
		if unknown.State != restoreUnknown || unknown.Err == "" {
			t.Errorf("a failed check: state %q err %q, want %q with the reason kept", unknown.State, unknown.Err, restoreUnknown)
		}
	}
}
