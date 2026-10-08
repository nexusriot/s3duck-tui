package ops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// syncHarness records which plan entries were applied, in order, and can be
// told which ones fail or block.
type syncHarness struct {
	mu      sync.Mutex
	applied []string
	units   []SyncUnit
	failFor map[string]error
	// hook runs inside Apply, before the result is decided, so a test can
	// cancel mid-run.
	hook func(label string)
	// bytes makes a unit report progress as it goes.
	bytes bool
}

func (h *syncHarness) apply(ctx context.Context, i int, onProgress func(int64)) error {
	label := h.units[i].Label
	if h.hook != nil {
		h.hook(label)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	h.mu.Lock()
	h.applied = append(h.applied, label)
	err := h.failFor[label]
	h.mu.Unlock()
	if err != nil {
		return err
	}
	if h.bytes && onProgress != nil {
		onProgress(h.units[i].Bytes / 2)
		onProgress(h.units[i].Bytes)
	}
	return nil
}

func (h *syncHarness) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.applied)
}

func newSync(units ...SyncUnit) (*Sync, *syncHarness) {
	h := &syncHarness{units: units, failFor: map[string]error{}}
	s := &Sync{
		Title:   "upload  /src → b/dst/",
		Units:   units,
		Workers: 1, // deterministic order unless a test asks otherwise
		Apply:   h.apply,
	}
	return s, h
}

func write(label string, bytes int64) SyncUnit { return SyncUnit{Label: label, Bytes: bytes} }
func del(label string) SyncUnit                { return SyncUnit{Label: label, Delete: true} }

func TestSyncHappyPath(t *testing.T) {
	s, h := newSync(write("upload a", 10), write("upload b", 20))

	res := s.Run(context.Background(), &recorder{})

	if res.Done != 2 || res.Failed() != 0 || res.Canceled {
		t.Fatalf("result = %+v", res)
	}
	if res.Bytes != 30 {
		t.Errorf("bytes = %d, want 30", res.Bytes)
	}
	if got := len(h.seen()); got != 2 {
		t.Errorf("applied %d units, want 2", got)
	}
}

func TestSyncDescribeSumsBytesWhenNotGiven(t *testing.T) {
	s, _ := newSync(write("a", 10), write("b", 32))
	d := s.Describe()
	if d.TotalBytes != 42 || d.TotalItems != 2 {
		t.Errorf("descriptor = %+v, want 42 bytes over 2 units", d)
	}
	if d.Kind != KindSync {
		t.Errorf("kind = %q", d.Kind)
	}
	// A remote→local sync changes nothing the browser shows.
	if d.RefreshAfter {
		t.Error("a local-destination sync asked for a refresh")
	}
	s.WritesRemote = true
	if !s.Describe().RefreshAfter {
		t.Error("a remote-destination sync should ask for a refresh")
	}
}

func TestSplitSyncPhases(t *testing.T) {
	units := []SyncUnit{write("w1", 1), del("d1"), write("w2", 1), del("d2")}
	writes, deletes := SplitSyncPhases(units)

	if len(writes) != 2 || len(deletes) != 2 {
		t.Fatalf("writes %d / deletes %d, want 2 / 2", len(writes), len(deletes))
	}
	// Indices must stay the plan's, or a worker would apply the wrong entry.
	if writes[1].Index != 2 || deletes[1].Index != 3 {
		t.Errorf("indices not preserved: writes %v deletes %v", writes, deletes)
	}
}

// Every write completes before any delete starts. A run interrupted partway
// must leave the destination holding the new files but not yet missing the old
// ones — the safer of the two intermediate states.
func TestSyncWritesRunBeforeDeletes(t *testing.T) {
	s, h := newSync(del("delete old1"), write("upload new1", 1), del("delete old2"), write("upload new2", 1))

	s.Run(context.Background(), &recorder{})

	seen := h.seen()
	if len(seen) != 4 {
		t.Fatalf("applied %v, want all four", seen)
	}
	for i, label := range seen {
		isDelete := strings.HasPrefix(label, "delete")
		if i < 2 && isDelete {
			t.Fatalf("a delete ran in position %d, before the writes finished: %v", i, seen)
		}
		if i >= 2 && !isDelete {
			t.Fatalf("a write ran in position %d, after the deletes began: %v", i, seen)
		}
	}
}

// A failed write means the destination is not the mirror the reviewed plan
// assumed, so removing anything from it is no longer covered by the approval
// the user gave.
func TestSyncSkipsDeletesWhenAWriteFailed(t *testing.T) {
	s, h := newSync(write("upload a", 1), write("upload b", 1), del("delete x"), del("delete y"))
	h.failFor["upload b"] = errors.New("endpoint said no")

	res := s.Run(context.Background(), &recorder{})

	for _, label := range h.seen() {
		if strings.HasPrefix(label, "delete") {
			t.Fatalf("a delete ran after a write failed: %v", h.seen())
		}
	}
	// The skip is reported, not silent: the user asked for those deletes.
	var note string
	for _, f := range res.Failures {
		if strings.Contains(f.Label, "delete(s) skipped") {
			note = f.Err.Error()
		}
	}
	if note == "" {
		t.Fatalf("the skipped deletes were not reported: %v", res.Failures)
	}
	if !strings.Contains(note, "no longer covered by your approval") {
		t.Errorf("note = %q, want it to explain why", note)
	}
}

// With no deletes in the plan there is nothing to warn about, so a failed write
// must not invent a second failure entry.
func TestSyncNoSkipNoteWhenThereAreNoDeletes(t *testing.T) {
	s, h := newSync(write("upload a", 1))
	h.failFor["upload a"] = errors.New("boom")

	res := s.Run(context.Background(), &recorder{})

	if res.Failed() != 1 {
		t.Errorf("failures = %v, want exactly the failed write", res.Failures)
	}
}

func TestSyncDeletesRunWhenEveryWriteSucceeded(t *testing.T) {
	s, h := newSync(write("upload a", 1), del("delete x"))

	res := s.Run(context.Background(), &recorder{})

	if !slices.Contains(h.seen(), "delete x") {
		t.Errorf("the delete did not run: %v", h.seen())
	}
	if res.Done != 2 || res.Failed() != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestSyncCancelBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, h := newSync(write("upload a", 1), write("upload b", 1))

	res := s.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	if got := h.seen(); len(got) != 0 {
		t.Errorf("applied %v with a dead context", got)
	}
}

// Cancelling mid-run stops dispatch and skips the delete phase: a half-written
// destination must not then lose files.
func TestSyncCancelMidRunSkipsDeletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	units := []SyncUnit{write("upload a", 1), write("upload b", 1), write("upload c", 1), del("delete x")}
	s, h := newSync(units...)
	h.hook = func(label string) {
		if label == "upload a" {
			cancel()
		}
	}

	res := s.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	if slices.Contains(h.seen(), "delete x") {
		t.Errorf("a delete ran after a cancel: %v", h.seen())
	}
}

func TestSyncCancelWhileQueuedBehindTheGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, h := newSync(write("upload a", 1))
	s.Gate = blockingGate{}

	go cancel()
	res := s.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("a run cancelled while queued should be marked cancelled")
	}
	if got := h.seen(); len(got) != 0 {
		t.Errorf("applied %v without a transfer slot", got)
	}
}

// The retry re-runs exactly the failed units, and its Apply must still reach
// the right plan entries — an off-by-one here would re-run the wrong files.
func TestSyncRetryRemapsIndices(t *testing.T) {
	s, h := newSync(write("upload a", 1), write("upload b", 7), write("upload c", 1))
	h.failFor["upload b"] = errors.New("boom")

	res := s.Run(context.Background(), &recorder{})

	rs, ok := res.Retry.(*Sync)
	if !ok {
		t.Fatalf("retry is %T, want *Sync", res.Retry)
	}
	if len(rs.Units) != 1 || rs.Units[0].Label != "upload b" {
		t.Fatalf("retry units = %v, want just 'upload b'", rs.Units)
	}
	if got := rs.Describe().TotalBytes; got != 7 {
		t.Errorf("retry total = %d, want 7 (b's real size)", got)
	}

	// Run the retry with the failure cleared: it must apply "upload b", not
	// whatever lives at index 0 of the original plan.
	delete(h.failFor, "upload b")
	h.mu.Lock()
	h.applied = nil
	h.mu.Unlock()

	res2 := rs.Run(context.Background(), &recorder{})

	if got := h.seen(); len(got) != 1 || got[0] != "upload b" {
		t.Errorf("the retry applied %v, want [upload b]", got)
	}
	if res2.Done != 1 || res2.Failed() != 0 {
		t.Errorf("retry result = %+v", res2)
	}
}

// The synthetic "deletes skipped" entry has no unit behind it and must not
// become a phantom retry target.
func TestSyncRetryIgnoresTheSkippedDeletesNote(t *testing.T) {
	s, h := newSync(write("upload a", 1), del("delete x"))
	h.failFor["upload a"] = errors.New("boom")

	res := s.Run(context.Background(), &recorder{})

	rs, ok := res.Retry.(*Sync)
	if !ok {
		t.Fatalf("retry is %T", res.Retry)
	}
	if len(rs.Units) != 1 || rs.Units[0].Label != "upload a" {
		t.Errorf("retry units = %v, want only the failed write", rs.Units)
	}
}

func TestSyncProgressCountsInFlightBytes(t *testing.T) {
	s, h := newSync(write("upload a", 100), write("upload b", 100))
	h.bytes = true
	rep := &recorder{}

	s.Run(context.Background(), rep)

	last, ok := rep.lastProgress()
	if !ok {
		t.Fatal("no progress reported")
	}
	if last.TotalBytes != 200 {
		t.Errorf("total = %d, want 200", last.TotalBytes)
	}
	if last.Bytes > last.TotalBytes {
		t.Errorf("reported %d of %d bytes", last.Bytes, last.TotalBytes)
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	for _, s := range rep.samples {
		if s.Bytes > s.TotalBytes {
			t.Fatalf("a sample over-reported: %+v", s)
		}
	}
}

func TestSyncWorkerCount(t *testing.T) {
	for _, tc := range []struct{ n, want int }{
		{0, 1}, {1, 1}, {3, 3}, {4, SyncWorkers}, {500, SyncWorkers},
	} {
		if got := SyncWorkerCount(tc.n); got != tc.want {
			t.Errorf("SyncWorkerCount(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// Several workers writing the same counters is the shape the real run has;
// -race turns this into an assertion.
func TestSyncParallelPhaseIsRaceFree(t *testing.T) {
	var units []SyncUnit
	for i := range 40 {
		units = append(units, write(string(rune('a'+i%26))+"-"+string(rune('0'+i/26)), 1))
	}
	s, h := newSync(units...)
	s.Workers = SyncWorkers
	h.bytes = true

	res := s.Run(context.Background(), &recorder{})

	if res.Done != len(units) || res.Failed() != 0 {
		t.Errorf("result = done %d / failed %d, want %d / 0", res.Done, res.Failed(), len(units))
	}
}
