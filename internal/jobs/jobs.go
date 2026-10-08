// Package jobs is the background transfer queue: what is running, how far
// along it is, and what to say when it finishes.
//
// It knows nothing about tview or about S3. A transfer flow registers a job,
// reports progress into it and finalises it; the UI reads snapshots. Keeping
// the queue here rather than on the controller is what lets its arithmetic —
// the badge percentage, the notice text, the finished-job sweep — be tested
// without a terminal.
package jobs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
)

// StatusTick is how often the header badge is recomputed. Slow enough to be
// free, fast enough that a percentage does not look stuck.
const StatusTick = 500 * time.Millisecond

// NoticeFor is how long a completion notice holds the status field before the
// badge (or nothing) takes it back.
const NoticeFor = 6 * time.Second

// Status is where a job is in its life.
type Status int

const (
	Running Status = iota
	Queued
	Done
	Failed
	Canceled
)

func (s Status) String() string {
	switch s {
	case Running:
		return "running"
	case Queued:
		return "queued"
	case Done:
		return "done"
	case Failed:
		return "failed"
	case Canceled:
		return "canceled"
	default:
		return "?"
	}
}

// Active reports whether a job in this state is still doing something, which
// is what the badge counts and what a cancel can still reach.
func (s Status) Active() bool { return s == Running || s == Queued }

// Job tracks one background transfer. Its mutable fields are guarded by mu:
// progress goroutines write them while the panel ticker reads them.
type Job struct {
	id     int
	kind   string
	desc   string
	cancel context.CancelFunc
	start  time.Time

	mu        sync.Mutex
	status    Status
	total     int64
	done      int64
	count     int
	doneCount int
	failed    int
	bg        bool
}

// Kind is the job's category ("download", "upload", "sync", "xcopy").
func (j *Job) Kind() string { return j.kind }

// Desc is the one-line description shown in the panel.
func (j *Job) Desc() string { return j.desc }

func (j *Job) SetStatus(s Status) { j.mu.Lock(); j.status = s; j.mu.Unlock() }

func (j *Job) SetProgress(done int64, doneCount int) {
	j.mu.Lock()
	j.done, j.doneCount = done, doneCount
	j.mu.Unlock()
}

func (j *Job) SetTotals(total int64, count int) {
	j.mu.Lock()
	j.total, j.count = total, count
	j.mu.Unlock()
}

// SetBackgrounded detaches the job from its progress modal. Only a
// backgrounded job is announced when it finishes: a foreground one still owns
// a modal that reports the outcome itself.
func (j *Job) SetBackgrounded() { j.mu.Lock(); j.bg = true; j.mu.Unlock() }

func (j *Job) IsBackgrounded() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.bg }

// View is an unlocked snapshot of a job, safe to render from.
type View struct {
	ID                       int
	Kind, Desc               string
	Status                   Status
	Total, Done              int64
	Count, DoneCount, Failed int
	Start                    time.Time
	// Bg is whether the job was sent to the background.
	Bg bool
}

// View snapshots the job under its lock.
func (j *Job) View() View {
	j.mu.Lock()
	defer j.mu.Unlock()
	return View{j.id, j.kind, j.desc, j.status, j.total, j.done, j.count, j.doneCount, j.failed, j.start, j.bg}
}

// Queue is the list of transfers plus the header notice and the concurrency
// gate. One per application.
type Queue struct {
	mu          sync.Mutex
	jobs        []*Job
	nextID      int
	notice      string
	noticeUntil time.Time

	// sem caps concurrent byte-transfer phases. Aggregate bandwidth is capped
	// separately by the model's rate limiter; this bounds how many transfers
	// compete for it at once.
	sem chan struct{}

	// log records a finished job in the activity log; bell rings the terminal
	// for a backgrounded outcome worth interrupting for. Both may be nil.
	log  func(format string, args ...any)
	bell func()
}

// NewQueue builds the queue. concurrency is how many byte-transfer phases may
// run at once. log and bell may be nil.
func NewQueue(concurrency int, log func(string, ...any), bell func()) *Queue {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Queue{sem: make(chan struct{}, concurrency), log: log, bell: bell}
}

// Acquire waits for a transfer slot. It returns false when ctx is cancelled
// first, in which case nothing was taken and release must not be called.
func (q *Queue) Acquire(ctx context.Context) bool {
	select {
	case q.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Release gives back a slot taken by Acquire.
func (q *Queue) Release() { <-q.sem }

// Add registers a new running job.
func (q *Queue) Add(kind, desc string, total int64, count int, cancel context.CancelFunc) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	j := &Job{
		id: q.nextID, kind: kind, desc: desc, cancel: cancel,
		start: time.Now(), status: Running, total: total, count: count,
	}
	q.jobs = append(q.jobs, j)
	return j
}

// Finish records a job's terminal state, logs it, and announces it when it was
// running in the background.
func (q *Queue) Finish(j *Job, canceled bool, failed int) {
	j.mu.Lock()
	j.failed = failed
	switch {
	case canceled:
		j.status = Canceled
	case failed > 0:
		j.status = Failed
	default:
		j.status = Done
	}
	j.mu.Unlock()

	v := j.View()
	if q.log != nil {
		q.log("%s %s: %s", v.Kind, v.Desc, v.Status)
	}
	q.announce(v)
}

// announce publishes the header notice for a finished background job. A
// foreground job already has the user's attention — its own modal is showing
// the result — so only a backgrounded one is announced.
func (q *Queue) announce(v View) {
	if !v.Bg {
		return
	}
	q.SetNotice(NoticeText(v))
	// The bell only for outcomes worth interrupting for: a transfer the user
	// walked away from, or a failure. Not for their own cancel.
	if v.Status != Canceled && q.bell != nil {
		q.bell()
	}
	if q.log != nil {
		q.log("%s %s finished: %s", v.Kind, v.Desc, strings.TrimSpace(v.Status.String()))
	}
}

// Snapshot returns a view of every job, newest last.
func (q *Queue) Snapshot() []View {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]View, 0, len(q.jobs))
	for _, j := range q.jobs {
		out = append(out, j.View())
	}
	return out
}

// CancelAt cancels the still-active job shown at row i. Out-of-range rows and
// finished jobs are no-ops, so a stale panel cannot cancel the wrong thing.
func (q *Queue) CancelAt(i int) {
	q.mu.Lock()
	var j *Job
	if i >= 0 && i < len(q.jobs) {
		j = q.jobs[i]
	}
	q.mu.Unlock()
	if j == nil {
		return
	}
	j.mu.Lock()
	active, cancel := j.status.Active(), j.cancel
	j.mu.Unlock()
	if active && cancel != nil {
		cancel()
	}
}

// ClearFinished drops done/failed/canceled jobs from the list.
func (q *Queue) ClearFinished() {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.jobs[:0]
	for _, j := range q.jobs {
		j.mu.Lock()
		st := j.status
		j.mu.Unlock()
		if st.Active() {
			kept = append(kept, j)
		}
	}
	q.jobs = kept
}

// SetNotice publishes a transient status message. Safe from any goroutine.
func (q *Queue) SetNotice(msg string) {
	q.mu.Lock()
	q.notice = msg
	q.noticeUntil = time.Now().Add(NoticeFor)
	q.mu.Unlock()
}

// Status is what the header's right field should show now: a live notice if
// one is unexpired, otherwise the transfer badge.
func (q *Queue) Status() string {
	q.mu.Lock()
	notice, until := q.notice, q.noticeUntil
	q.mu.Unlock()
	if notice != "" && time.Now().Before(until) {
		return notice
	}
	return Badge(q.Snapshot())
}

// Badge renders the header's transfer indicator: how many jobs are active and
// their combined byte progress. Empty when nothing is running, so an idle app
// carries no chrome it does not need.
//
// Pure, so the arithmetic that decides "45%" is testable.
func Badge(views []View) string {
	active, done, total := 0, int64(0), int64(0)
	for _, v := range views {
		if !v.Status.Active() {
			continue
		}
		active++
		done += v.Done
		total += v.Total
	}
	if active == 0 {
		return ""
	}
	label := fmt.Sprintf(" ⇅ %d transfer", active)
	if active > 1 {
		label += "s"
	}
	if total > 0 {
		pct := float64(done) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
		label += fmt.Sprintf(" · %.0f%% (%s)", pct, humanize.IBytes(uint64(done)))
	}
	return label + " "
}

// NoticeText renders a finished job as a one-line notice.
func NoticeText(v View) string {
	mark := "✓"
	if v.Status == Failed {
		mark = "✗"
	} else if v.Status == Canceled {
		mark = "⃠"
	}
	desc := v.Desc
	const maxDesc = 40
	if len([]rune(desc)) > maxDesc {
		desc = string([]rune(desc)[:maxDesc-1]) + "…"
	}
	if v.Failed > 0 {
		return fmt.Sprintf(" %s %s %s: %s (%d failed) ", mark, v.Kind, desc, v.Status, v.Failed)
	}
	return fmt.Sprintf(" %s %s %s: %s ", mark, v.Kind, desc, v.Status)
}
