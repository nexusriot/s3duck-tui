package jobs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatusString(t *testing.T) {
	cases := map[Status]string{
		Running:  "running",
		Queued:   "queued",
		Done:     "done",
		Failed:   "failed",
		Canceled: "canceled",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestStatusActive(t *testing.T) {
	for _, s := range []Status{Running, Queued} {
		if !s.Active() {
			t.Errorf("%s should be active", s)
		}
	}
	for _, s := range []Status{Done, Failed, Canceled} {
		if s.Active() {
			t.Errorf("%s should not be active", s)
		}
	}
}

func TestBadge(t *testing.T) {
	if got := Badge(nil); got != "" {
		t.Errorf("an idle app should carry no badge, got %q", got)
	}
	done := []View{{Status: Done}, {Status: Failed}, {Status: Canceled}}
	if got := Badge(done); got != "" {
		t.Errorf("finished jobs are not active, got %q", got)
	}
	one := []View{{Status: Running, Done: 50, Total: 100}}
	got := Badge(one)
	if !strings.Contains(got, "1 transfer") || !strings.Contains(got, "50%") {
		t.Errorf("badge = %q", got)
	}
	if strings.Contains(got, "transfers") {
		t.Errorf("one job should be singular: %q", got)
	}
	two := []View{{Status: Running, Done: 1, Total: 4}, {Status: Queued, Done: 0, Total: 4}}
	// 1 of 8 bytes: %.0f rounds half to even, so 12.5% prints as 12%.
	if got := Badge(two); !strings.Contains(got, "2 transfers") || !strings.Contains(got, "12%") {
		t.Errorf("badge = %q, want the combined progress", got)
	}
	// A job whose total is unknown must not divide by zero or claim a share.
	if got := Badge([]View{{Status: Running, Done: 10}}); !strings.Contains(got, "1 transfer") || strings.Contains(got, "%") {
		t.Errorf("unknown total = %q", got)
	}
	// Over-reported progress is clamped rather than printing 130%.
	if got := Badge([]View{{Status: Running, Done: 13, Total: 10}}); !strings.Contains(got, "100%") {
		t.Errorf("clamped badge = %q", got)
	}
}

func TestNoticeText(t *testing.T) {
	got := NoticeText(View{Kind: "download", Desc: "photos/", Status: Done})
	if !strings.Contains(got, "✓") || !strings.Contains(got, "download") || !strings.Contains(got, "photos/") {
		t.Errorf("notice = %q", got)
	}
	if got := NoticeText(View{Kind: "upload", Desc: "x", Status: Failed, Failed: 3}); !strings.Contains(got, "3 failed") {
		t.Errorf("a failure count should be shown: %q", got)
	}
	// A long description is truncated so the notice cannot push the version
	// string off the header row.
	long := strings.Repeat("a", 100)
	if got := NoticeText(View{Kind: "sync", Desc: long, Status: Done}); len(got) > 80 {
		t.Errorf("notice is %d chars, too long: %q", len(got), got)
	}
}

func TestAddAssignsRisingIDs(t *testing.T) {
	q := NewQueue(2, nil, nil)
	a := q.Add("download", "a", 10, 1, nil)
	b := q.Add("upload", "b", 20, 2, nil)
	if a.View().ID == b.View().ID {
		t.Fatal("two jobs share an id")
	}
	if got := len(q.Snapshot()); got != 2 {
		t.Fatalf("snapshot has %d jobs, want 2", got)
	}
}

func TestFinishSetsTerminalStatus(t *testing.T) {
	q := NewQueue(1, nil, nil)
	for _, tc := range []struct {
		name     string
		canceled bool
		failed   int
		want     Status
	}{
		{"clean run", false, 0, Done},
		{"with failures", false, 2, Failed},
		{"cancel wins over failures", true, 2, Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := q.Add("sync", tc.name, 0, 0, nil)
			q.Finish(j, tc.canceled, tc.failed)
			if got := j.View().Status; got != tc.want {
				t.Errorf("status = %s, want %s", got, tc.want)
			}
			if got := j.View().Failed; got != tc.failed {
				t.Errorf("failed = %d, want %d", got, tc.failed)
			}
		})
	}
}

// A foreground job still owns a modal that reports its own outcome, so only a
// backgrounded one should raise a notice or ring the bell.
func TestOnlyBackgroundedJobsAreAnnounced(t *testing.T) {
	bells := 0
	q := NewQueue(1, nil, func() { bells++ })

	fg := q.Add("download", "fg", 0, 0, nil)
	q.Finish(fg, false, 0)
	if got := q.Status(); got != "" {
		t.Errorf("a foreground job raised the notice %q", got)
	}
	if bells != 0 {
		t.Errorf("a foreground job rang the bell %d time(s)", bells)
	}

	bg := q.Add("download", "bg", 0, 0, nil)
	bg.SetBackgrounded()
	q.Finish(bg, false, 0)
	if got := q.Status(); !strings.Contains(got, "bg") {
		t.Errorf("status = %q, want the finished background job", got)
	}
	if bells != 1 {
		t.Errorf("bells = %d, want 1", bells)
	}
}

// Cancelling is the user's own decision; it should not also ring at them.
func TestCancelDoesNotRing(t *testing.T) {
	bells := 0
	q := NewQueue(1, nil, func() { bells++ })
	j := q.Add("upload", "x", 0, 0, nil)
	j.SetBackgrounded()
	q.Finish(j, true, 0)
	if bells != 0 {
		t.Errorf("a cancel rang the bell %d time(s)", bells)
	}
}

func TestStatusPrefersAnUnexpiredNotice(t *testing.T) {
	q := NewQueue(1, nil, nil)
	running := q.Add("download", "live", 100, 1, nil)
	running.SetProgress(50, 0)

	if got := q.Status(); !strings.Contains(got, "1 transfer") {
		t.Fatalf("with no notice the badge should show: %q", got)
	}
	q.SetNotice(" ✓ done ")
	if got := q.Status(); got != " ✓ done " {
		t.Errorf("status = %q, want the fresh notice", got)
	}

	// Once it expires the badge takes the field back.
	q.mu.Lock()
	q.noticeUntil = time.Now().Add(-time.Second)
	q.mu.Unlock()
	if got := q.Status(); !strings.Contains(got, "1 transfer") {
		t.Errorf("status = %q, want the badge back after the notice expired", got)
	}
}

func TestCancelAtOnlyReachesActiveJobs(t *testing.T) {
	q := NewQueue(1, nil, nil)
	canceled := false
	j := q.Add("download", "x", 0, 0, func() { canceled = true })

	q.CancelAt(99) // out of range
	q.CancelAt(-1)
	if canceled {
		t.Fatal("an out-of-range row cancelled a job")
	}

	q.CancelAt(0)
	if !canceled {
		t.Fatal("cancelling row 0 did not reach the running job")
	}

	canceled = false
	q.Finish(j, false, 0)
	q.CancelAt(0)
	if canceled {
		t.Error("a finished job was cancelled again")
	}
}

func TestClearFinishedKeepsActiveJobs(t *testing.T) {
	q := NewQueue(1, nil, nil)
	live := q.Add("download", "live", 0, 0, nil)
	gone := q.Add("upload", "gone", 0, 0, nil)
	q.Finish(gone, false, 0)

	q.ClearFinished()

	snap := q.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("kept %d jobs, want 1", len(snap))
	}
	if snap[0].ID != live.View().ID {
		t.Errorf("kept job %q, want the running one", snap[0].Desc)
	}
}

func TestAcquireBlocksBeyondConcurrencyAndReleases(t *testing.T) {
	q := NewQueue(1, nil, nil)
	ctx := context.Background()

	if !q.Acquire(ctx) {
		t.Fatal("the first Acquire should succeed immediately")
	}

	got := make(chan bool, 1)
	go func() { got <- q.Acquire(ctx) }()

	select {
	case <-got:
		t.Fatal("a second Acquire got through while the only slot was held")
	case <-time.After(50 * time.Millisecond):
	}

	q.Release()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("Acquire reported failure after a slot freed")
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not wake after Release")
	}
	q.Release()
}

func TestAcquireReportsCancellation(t *testing.T) {
	q := NewQueue(1, nil, nil)
	if !q.Acquire(context.Background()) {
		t.Fatal("setup: first Acquire should succeed")
	}
	defer q.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if q.Acquire(ctx) {
		t.Fatal("Acquire took a slot despite a cancelled context; the caller would Release one it never held")
	}
}

func TestConcurrentProgressIsRaceFree(t *testing.T) {
	q := NewQueue(2, nil, nil)
	j := q.Add("download", "x", 1000, 10, nil)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j.SetProgress(int64(i*10), i)
			_ = j.View()
			_ = q.Snapshot()
			_ = q.Status()
		}()
	}
	wg.Wait()
}
