package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// newDownload builds a Download wired to fakes. These tests are the point of
// phase 4: every branch below used to be reachable only by driving the real
// TUI against a live endpoint.
func newDownload(t *testing.T, fs FS, f *fakeFetcher, tg []model.DownloadTarget) *Download {
	t.Helper()
	var total int64
	for _, x := range tg {
		total += x.Size
	}
	return &Download{
		Bucket:        bucketObj("b"),
		SrcPrefix:     "",
		DestDir:       "/dl",
		Targets:       tg,
		SelectedCount: len(tg),
		TotalBytes:    total,
		Fetcher:       f,
		FS:            fs,
	}
}

func TestDownloadHappyPath(t *testing.T) {
	f := &fakeFetcher{progress: true}
	d := newDownload(t, newMemFS(), f, targets("a.txt", 10, "b.txt", 20))
	rep := &recorder{}

	res := d.Run(context.Background(), rep)

	if res.Done != 2 || res.Failed() != 0 || res.Skipped != 0 || res.Canceled {
		t.Fatalf("result = %+v, want 2 done and nothing else", res)
	}
	if res.Bytes != 30 {
		t.Errorf("bytes = %d, want 30", res.Bytes)
	}
	if got := f.keys(); len(got) != 2 {
		t.Errorf("fetched %v, want both objects", got)
	}
	if len(rep.asked) != 0 {
		t.Errorf("asked %v with nothing to overwrite", rep.askedPaths())
	}
	if res.Retry != nil {
		t.Error("a clean run should offer no retry")
	}
}

func TestDownloadDescribeCounts(t *testing.T) {
	d := newDownload(t, newMemFS(), &fakeFetcher{}, targets("a", 1024, "b", 1024))
	d.SelectedCount = 1 // one folder that expanded to two objects

	got := d.Describe()
	if got.Kind != KindDownload || got.TotalItems != 2 || got.TotalBytes != 2048 {
		t.Errorf("descriptor = %+v", got)
	}
	for _, want := range []string{"/dl", "Selected: 1", "Resolved: 2"} {
		if !strings.Contains(got.ConfirmText, want) {
			t.Errorf("confirm text %q is missing %q", got.ConfirmText, want)
		}
	}
}

// Directory markers have no bytes and must exist before the files under them
// land, so they are created in phase 1 and never queued for transfer.
func TestDownloadCreatesDirectoryMarkersWithoutTransferring(t *testing.T) {
	fs := newMemFS()
	f := &fakeFetcher{}
	d := newDownload(t, fs, f, targets("dir/", 0, "dir/x.txt", 5))

	res := d.Run(context.Background(), &recorder{})

	if fs.madeCount() != 1 {
		t.Errorf("created %d directories, want 1", fs.madeCount())
	}
	if got := f.keys(); len(got) != 1 || got[0] != "dir/x.txt" {
		t.Errorf("fetched %v, want only the file", got)
	}
	if res.Done != 2 {
		t.Errorf("done = %d, want 2 (the marker counts as done)", res.Done)
	}
}

func TestDownloadReportsAnUnmakeableDirectory(t *testing.T) {
	fs := newMemFS()
	fs.mkdirErr = errors.New("read-only filesystem")
	d := newDownload(t, fs, &fakeFetcher{}, targets("dir/", 0))

	res := d.Run(context.Background(), &recorder{})

	if res.Failed() != 1 || !strings.Contains(res.Failures[0].Err.Error(), "read-only") {
		t.Errorf("failures = %v, want the mkdir error", res.Failures)
	}
}

func TestDownloadRejectsAnEmptyKey(t *testing.T) {
	d := newDownload(t, newMemFS(), &fakeFetcher{}, targets("", 0, "ok.txt", 1))

	res := d.Run(context.Background(), &recorder{})

	if res.Failed() != 1 {
		t.Fatalf("failures = %v, want 1", res.Failures)
	}
	if res.Failures[0].Label != "<nil-key>" {
		t.Errorf("label = %q, want <nil-key>", res.Failures[0].Label)
	}
	if res.Done != 1 {
		t.Errorf("done = %d; the good object should still transfer", res.Done)
	}
}

// A key that would escape the destination directory must be refused, not
// written outside it.
func TestDownloadRefusesAnEscapingKey(t *testing.T) {
	d := newDownload(t, newMemFS(), &fakeFetcher{}, targets("../../etc/passwd", 10))

	res := d.Run(context.Background(), &recorder{})

	if res.Done != 0 || res.Failed() != 1 {
		t.Fatalf("result = %+v, want the key refused", res)
	}
}

func TestDownloadOverwriteAnswers(t *testing.T) {
	for _, tc := range []struct {
		name            string
		answer          Answer
		wantDone        int
		wantSkipped     int
		wantOverwritten int
		wantFetched     bool
	}{
		{"yes replaces it", AnswerYes, 1, 0, 1, true},
		{"no leaves it alone", AnswerNo, 0, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFetcher{}
			d := newDownload(t, newMemFS("a.txt"), f, targets("a.txt", 10))
			rep := &recorder{fallback: tc.answer}

			res := d.Run(context.Background(), rep)

			if res.Done != tc.wantDone || res.Skipped != tc.wantSkipped || res.Overwritten != tc.wantOverwritten {
				t.Errorf("result = done %d / skipped %d / overwritten %d, want %d / %d / %d",
					res.Done, res.Skipped, res.Overwritten, tc.wantDone, tc.wantSkipped, tc.wantOverwritten)
			}
			if gotFetched := len(f.keys()) == 1; gotFetched != tc.wantFetched {
				t.Errorf("fetched = %v, want %v", gotFetched, tc.wantFetched)
			}
			if len(rep.asked) != 1 {
				t.Errorf("asked %d times, want exactly 1", len(rep.asked))
			}
		})
	}
}

// "Overwritten" is counted on completion, not on the decision: the file is
// only actually replaced once its transfer succeeded.
func TestDownloadDoesNotCountAFailedOverwrite(t *testing.T) {
	f := &fakeFetcher{failFor: map[string]error{"a.txt": errors.New("boom")}}
	d := newDownload(t, newMemFS("a.txt"), f, targets("a.txt", 10))

	res := d.Run(context.Background(), &recorder{fallback: AnswerYes})

	if res.Overwritten != 0 {
		t.Errorf("overwritten = %d after a failed transfer, want 0", res.Overwritten)
	}
	if res.Failed() != 1 {
		t.Errorf("failures = %v, want 1", res.Failures)
	}
}

// The ToAll answers must stop the asking, which is what keeps a 400-conflict
// download from being 400 dialogs.
func TestDownloadToAllAnswersAskOnlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		answer      Answer
		wantDone    int
		wantSkipped int
	}{
		{"yes to all", AnswerYesToAll, 3, 0},
		{"no to all", AnswerNoToAll, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFetcher{}
			d := newDownload(t, newMemFS("a", "b", "c"), f, targets("a", 1, "b", 1, "c", 1))
			rep := &recorder{answers: []Answer{tc.answer}, fallback: AnswerCancel}

			res := d.Run(context.Background(), rep)

			if len(rep.asked) != 1 {
				t.Errorf("asked %d times, want 1: %v", len(rep.asked), rep.askedPaths())
			}
			if res.Done != tc.wantDone || res.Skipped != tc.wantSkipped {
				t.Errorf("done %d / skipped %d, want %d / %d", res.Done, res.Skipped, tc.wantDone, tc.wantSkipped)
			}
		})
	}
}

// Only conflicting destinations are asked about; the rest go straight through.
func TestDownloadAsksOnlyAboutExistingFiles(t *testing.T) {
	f := &fakeFetcher{}
	d := newDownload(t, newMemFS("b.txt"), f, targets("a.txt", 1, "b.txt", 1, "c.txt", 1))
	rep := &recorder{fallback: AnswerYes}

	res := d.Run(context.Background(), rep)

	asked := rep.askedPaths()
	if len(asked) != 1 || !strings.HasSuffix(asked[0], "b.txt") {
		t.Errorf("asked about %v, want only b.txt", asked)
	}
	if res.Done != 3 {
		t.Errorf("done = %d, want 3", res.Done)
	}
}

// Cancelling at an overwrite prompt abandons the run without transferring
// anything — including the objects already cleared.
func TestDownloadCancelAtThePromptTransfersNothing(t *testing.T) {
	f := &fakeFetcher{}
	d := newDownload(t, newMemFS("b"), f, targets("a", 1, "b", 1, "c", 1))
	rep := &recorder{fallback: AnswerCancel}

	res := d.Run(context.Background(), rep)

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	if got := f.keys(); len(got) != 0 {
		t.Errorf("transferred %v after a cancel at the prompt", got)
	}
}

// A context cancelled before the run starts must not transfer anything.
func TestDownloadHonoursAnAlreadyCancelledContext(t *testing.T) {
	f := &fakeFetcher{}
	d := newDownload(t, newMemFS(), f, targets("a", 1, "b", 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := d.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	if got := f.keys(); len(got) != 0 {
		t.Errorf("transferred %v with a dead context", got)
	}
}

// Cancelling mid-transfer stops new work being handed out, and lets the
// transfers already started drain so the report is about what actually
// happened.
//
// Timing-free by construction: every worker slot is parked inside a fetch, so
// the dispatcher is demonstrably blocked waiting for one when the cancel
// lands. An earlier version cancelled from inside a fetch and asserted that
// "not everything" transferred, which held on an idle machine and failed
// about three runs in four under CPU contention — the dispatcher simply got
// through all the work before the cancelling goroutine was scheduled again.
func TestDownloadCancelMidTransferDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeFetcher{
		gate:    make(chan struct{}),
		started: make(chan string, DownloadWorkers),
	}

	var tg []model.DownloadTarget
	for i := range DownloadWorkers * 10 {
		tg = append(tg, model.DownloadTarget{Key: fmt.Sprintf("k%02d", i), Size: 1})
	}
	d := newDownload(t, newMemFS(), f, tg)

	done := make(chan Result, 1)
	go func() { done <- d.Run(ctx, &recorder{}) }()

	// Every slot is now held by a parked fetch, so the dispatcher is blocked
	// trying to take one for the next object.
	for range DownloadWorkers {
		select {
		case <-f.started:
		case <-time.After(5 * time.Second):
			t.Fatal("workers never reached the fetcher")
		}
	}

	cancel()
	close(f.gate) // let the parked transfers finish

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the cancel")
	}

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	if got := len(f.keys()); got != DownloadWorkers {
		t.Errorf("transferred %d objects, want exactly the %d already in flight; "+
			"a cancel must stop dispatch, and must not abandon what had started",
			got, DownloadWorkers)
	}
}

// A full gate means the transfer never starts; the run reports a cancel rather
// than claiming success, and never takes a slot it would then release.
func TestDownloadCancelWhileQueuedBehindTheGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeFetcher{}
	d := newDownload(t, newMemFS(), f, targets("a", 1))
	d.Gate = blockingGate{}

	go cancel()
	res := d.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("a run cancelled while queued should be marked cancelled")
	}
	if got := f.keys(); len(got) != 0 {
		t.Errorf("transferred %v without a transfer slot", got)
	}
}

func TestDownloadVerification(t *testing.T) {
	for _, tc := range []struct {
		name           string
		result         model.VerifyResult
		err            error
		wantDone       int
		wantFailed     int
		wantVerified   int
		wantUnverified int
	}{
		{"verified", model.VerifyResult{Verified: true}, nil, 1, 0, 1, 0},
		{"unverifiable is not a failure", model.VerifyResult{}, nil, 1, 0, 0, 1},
		{"mismatch is a failure", model.VerifyResult{Mismatch: true}, nil, 0, 1, 0, 0},
		{"verifier error is a failure", model.VerifyResult{}, errors.New("no attributes"), 0, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFetcher{}
			d := newDownload(t, newMemFS(), f, targets("a.txt", 10))
			d.Verify = true
			d.Checker = &fakeChecker{
				results: map[string]model.VerifyResult{"a.txt": tc.result},
				errs:    map[string]error{"a.txt": tc.err},
			}

			res := d.Run(context.Background(), &recorder{})

			if res.Done != tc.wantDone || res.Failed() != tc.wantFailed ||
				res.Verified != tc.wantVerified || res.Unverified != tc.wantUnverified {
				t.Errorf("done %d / failed %d / verified %d / unverified %d; want %d / %d / %d / %d",
					res.Done, res.Failed(), res.Verified, res.Unverified,
					tc.wantDone, tc.wantFailed, tc.wantVerified, tc.wantUnverified)
			}
		})
	}
}

// A mismatch must say so: a silently corrupt file is the worst of the three
// outcomes, so the report cannot read like an ordinary network failure.
func TestDownloadMismatchMessageMentionsTheDownload(t *testing.T) {
	d := newDownload(t, newMemFS(), &fakeFetcher{}, targets("a.txt", 10))
	d.Verify = true
	d.Checker = &fakeChecker{results: map[string]model.VerifyResult{
		"a.txt": {Mismatch: true},
	}}

	res := d.Run(context.Background(), &recorder{})

	if res.Failed() != 1 {
		t.Fatalf("failures = %v", res.Failures)
	}
	if msg := res.Failures[0].Err.Error(); !strings.Contains(msg, "downloaded, but") {
		t.Errorf("message = %q, want it to say the bytes landed but did not match", msg)
	}
}

// The retry covers exactly the objects that failed, with their real sizes, so
// the retry's gauge has a true total without re-resolving anything.
func TestDownloadRetryCoversOnlyTheFailures(t *testing.T) {
	f := &fakeFetcher{failFor: map[string]error{"b": errors.New("boom")}}
	d := newDownload(t, newMemFS(), f, targets("a", 10, "b", 25, "c", 10))

	res := d.Run(context.Background(), &recorder{})

	if res.Retry == nil {
		t.Fatal("a run with failures should offer a retry")
	}
	rd, ok := res.Retry.(*Download)
	if !ok {
		t.Fatalf("retry is %T, want *Download", res.Retry)
	}
	if len(rd.Targets) != 1 || rd.Targets[0].Key != "b" {
		t.Errorf("retry targets = %v, want just b", rd.Targets)
	}
	if rd.TotalBytes != 25 {
		t.Errorf("retry total = %d, want 25 (b's real size)", rd.TotalBytes)
	}
	if rd.Describe().TotalItems != 1 {
		t.Errorf("retry descriptor counts %d items, want 1", rd.Describe().TotalItems)
	}
}

// The retry must carry the original's settings, or a verified download would
// silently retry unverified.
func TestDownloadRetryKeepsTheVerifySetting(t *testing.T) {
	f := &fakeFetcher{failFor: map[string]error{"a": errors.New("boom")}}
	d := newDownload(t, newMemFS(), f, targets("a", 1))
	d.Verify = true
	d.Checker = &fakeChecker{}

	res := d.Run(context.Background(), &recorder{})

	rd := res.Retry.(*Download)
	if !rd.Verify {
		t.Error("the retry dropped the verify setting")
	}
	if rd.DestDir != d.DestDir || rd.SrcPrefix != d.SrcPrefix {
		t.Error("the retry changed where the files come from or go")
	}
}

// A failure with no object behind it (the empty key) cannot be retried, so it
// must not produce a retry with a phantom target.
func TestDownloadNoRetryForAnUnretryableFailure(t *testing.T) {
	d := newDownload(t, newMemFS(), &fakeFetcher{}, targets("", 0))

	res := d.Run(context.Background(), &recorder{})

	if res.Failed() != 1 {
		t.Fatalf("failures = %v, want 1", res.Failures)
	}
	if res.Retry != nil {
		t.Error("an empty-key failure should offer no retry")
	}
}

// The gauge must measure what is actually being sent: skipped objects are not
// in the total, so a transfer cannot finish at 60%.
func TestDownloadProgressTotalExcludesSkippedObjects(t *testing.T) {
	f := &fakeFetcher{progress: true}
	d := newDownload(t, newMemFS("big"), f, targets("big", 1000, "small", 10))
	rep := &recorder{fallback: AnswerNo} // skip "big"

	d.Run(context.Background(), rep)

	last, ok := rep.lastProgress()
	if !ok {
		t.Fatal("no progress was reported")
	}
	if last.TotalBytes != 10 {
		t.Errorf("progress total = %d, want 10 (the skipped object excluded)", last.TotalBytes)
	}
	if last.Bytes > last.TotalBytes {
		t.Errorf("reported %d of %d bytes", last.Bytes, last.TotalBytes)
	}
}

func TestDownloadProgressNamesWhatIsInFlight(t *testing.T) {
	f := &fakeFetcher{progress: true}
	d := newDownload(t, newMemFS(), f, targets("deep/path/file.bin", 100))
	rep := &recorder{}

	d.Run(context.Background(), rep)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	for _, s := range rep.samples {
		if slices.Contains(s.Active, "file.bin") {
			return
		}
	}
	t.Error("no progress sample named the object in flight by its base name")
}

func TestDownloadPassesTheOverwriteFlagToTheFetcher(t *testing.T) {
	f := &fakeFetcher{}
	d := newDownload(t, newMemFS("a.txt"), f, targets("a.txt", 1, "b.txt", 1))

	d.Run(context.Background(), &recorder{fallback: AnswerYes})

	if !f.overwroteKey("a.txt") {
		t.Error("the conflicting object was fetched without the overwrite flag")
	}
	if f.overwroteKey("b.txt") {
		t.Error("a non-conflicting object was fetched with the overwrite flag set")
	}
}

// When every object is skipped there is nothing to transfer, so the run must
// not queue for a transfer slot: it used to sit "running" behind two long
// transfers and, if cancelled there, report a download with nothing to do as
// cancelled.
func TestDownloadWithNothingLeftSkipsTheGate(t *testing.T) {
	f := &fakeFetcher{}
	d := newDownload(t, newMemFS("a"), f, targets("a", 1))
	d.Gate = blockingGate{}
	rep := &recorder{fallback: AnswerNoToAll}

	res := d.Run(context.Background(), rep)

	if res.Canceled || res.Skipped != 1 || len(f.keys()) != 0 {
		t.Errorf("result = %+v, fetched %v; want one skip, nothing fetched, not cancelled", res, f.keys())
	}
}

// cancelingChecker plays a user pressing Cancel while the verify is running:
// it cancels the run and fails the way a real checker's request then would.
type cancelingChecker struct{ cancel context.CancelFunc }

func (c *cancelingChecker) VerifyLocalFile(ctx context.Context, _ *model.Object, _, _ string) (model.VerifyResult, error) {
	c.cancel()
	return model.VerifyResult{}, ctx.Err()
}

// A cancel that lands during the post-download verify is not a failed
// download: the file is already in place, so ledgering it as a failure offered
// a Retry that re-downloaded a good file.
func TestDownloadCancelDuringVerifyIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDownload(t, newMemFS(), &fakeFetcher{}, targets("a.txt", 10))
	d.Verify = true
	d.Checker = &cancelingChecker{cancel: cancel}

	res := d.Run(ctx, &recorder{})

	if res.Failed() != 0 || res.Retry != nil {
		t.Errorf("failures = %v (retry offered: %v); a cancelled verify is not a failure", res.Failures, res.Retry != nil)
	}
	if !res.Canceled || res.Done != 1 || res.Unverified != 1 {
		t.Errorf("canceled %v / done %d / unverified %d; want true / 1 / 1", res.Canceled, res.Done, res.Unverified)
	}
}

// The configured width is the width: with every slot parked and dispatch then
// cancelled, exactly Workers objects were in flight — both narrower and wider
// than the default.
func TestDownloadHonoursTheConfiguredWidth(t *testing.T) {
	for _, width := range []int{2, DownloadWorkers + 2} {
		ctx, cancel := context.WithCancel(context.Background())
		f := &fakeFetcher{gate: make(chan struct{}), started: make(chan string, width)}
		var tg []model.DownloadTarget
		for i := range width * 5 {
			tg = append(tg, model.DownloadTarget{Key: fmt.Sprintf("k%02d", i), Size: 1})
		}
		d := newDownload(t, newMemFS(), f, tg)
		d.Workers = width

		done := make(chan Result, 1)
		go func() { done <- d.Run(ctx, &recorder{}) }()
		for range width {
			select {
			case <-f.started:
			case <-time.After(5 * time.Second):
				t.Fatalf("width %d: workers never reached the fetcher", width)
			}
		}
		cancel()
		close(f.gate)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("width %d: Run did not return after the cancel", width)
		}
		if got := len(f.keys()); got != width {
			t.Errorf("width %d: %d objects were in flight", width, got)
		}
	}
}
