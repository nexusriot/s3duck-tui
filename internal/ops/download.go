package ops

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// DownloadWorkers is how many objects transfer at once inside one download.
// Aggregate bandwidth is capped separately by the model's rate limiter, and the
// number of concurrent downloads by the job queue's gate; this is the width of
// a single one.
const DownloadWorkers = 4

// Download fetches a resolved set of objects into a local directory.
//
// It keeps the two-phase shape the flow has always had, and for the same
// reason: overwrite questions block on the user, so asking them from four
// workers at once would stack dialogs. Phase 1 walks every object, creates
// directory markers, and collects one decision per existing destination.
// Phase 2 transfers what survived, in parallel.
type Download struct {
	// Src names where the objects come from. SrcPrefix is the listing prefix
	// they were resolved under, which is what makes their local paths relative
	// to it rather than to the bucket root.
	Bucket    *model.Object
	SrcPrefix string
	// DestDir is the local directory to land in.
	DestDir string
	// Targets is the already-resolved object set (folders expanded).
	Targets []model.DownloadTarget
	// SelectedCount is how many items the user actually picked, before folder
	// expansion. Shown in the confirmation so "3 items / 412 objects" reads
	// the way the user thinks about it.
	SelectedCount int
	TotalBytes    int64

	// Verify asks for a post-transfer checksum comparison. A mismatch counts
	// as a failure: a silently corrupt file is the worst of the three outcomes.
	Verify bool

	// Workers is how many objects transfer at once. 0 means DownloadWorkers.
	Workers int

	Fetcher Fetcher
	Checker Checker
	FS      FS
	Gate    Gate
}

func (d *Download) Describe() Descriptor {
	return Descriptor{
		Kind:       KindDownload,
		Title:      fmt.Sprintf("%d obj → %s", len(d.Targets), d.DestDir),
		TotalBytes: d.TotalBytes,
		TotalItems: len(d.Targets),
		// A download writes to the filesystem only; the listing is unchanged.
		RefreshAfter: false,
		ConfirmText: fmt.Sprintf(
			"Download to %s\nSelected: %d item(s)\nResolved: %d object(s)\nTotal size: %s",
			d.DestDir, d.SelectedCount, len(d.Targets), humanize.IBytes(uint64(d.TotalBytes))),
	}
}

// resolved is one object that survived phase 1.
type resolved struct {
	target model.DownloadTarget
	// overwrite records the user's decision. The existing file is replaced
	// only once the transfer has fully succeeded (the model writes to a temp
	// file and renames), so a cancelled or failed run never destroys local
	// data — which is why this is a decision carried forward rather than a
	// deletion done now.
	overwrite bool
}

func (d *Download) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}

	todo, ok := d.plan(ctx, rep, led)
	if !ok || len(todo) == 0 {
		// Nothing survived phase 1 (all skipped, or only directory markers):
		// there are no bytes to move, so there is no reason to queue for a
		// transfer slot — and a cancel while queued would report a download
		// that had nothing left to do as cancelled.
		return d.finish(led)
	}

	var effectiveBytes int64
	for _, r := range todo {
		effectiveBytes += r.target.Size
	}

	// Wait for a transfer slot. Phase 1 is deliberately outside the gate: it
	// is user-interaction time, not bandwidth, and holding a slot through a
	// pile of overwrite dialogs would stall every other transfer.
	started := Reserve(ctx, d.Gate, rep, func() {
		d.transfer(ctx, rep, led, todo, effectiveBytes)
	})
	if !started {
		led.cancel()
	}
	return d.finish(led)
}

// plan is phase 1: expand directory markers, resolve local paths, and settle
// every overwrite question. It reports false when the user cancelled.
func (d *Download) plan(ctx context.Context, rep Reporter, led *ledger) ([]resolved, bool) {
	var todo []resolved
	overwriteAll, skipAll := false, false

	for _, t := range d.Targets {
		if ctx.Err() != nil {
			led.cancel()
			return nil, false
		}
		rep.Progress(Progress{Phase: "resolving", Items: len(todo), TotalItems: len(d.Targets)})

		if t.Key == "" {
			led.fail("", fmt.Errorf("object key is empty"))
			continue
		}

		dst, err := model.SafeLocalPath(d.DestDir, d.SrcPrefix, t.Key)
		if err != nil {
			led.fail(t.Key, err)
			continue
		}

		// Directory markers are created now and never queued: there are no
		// bytes to move, and a marker has to exist before the files under it
		// land.
		if strings.HasSuffix(t.Key, "/") {
			if err := d.FS.MkdirAll(dst); err != nil {
				led.fail(t.Key, err)
			} else {
				led.ok(0, false)
			}
			continue
		}

		overwrite := false
		if d.FS.Exists(dst) {
			switch {
			case skipAll:
				led.skip()
				continue
			case overwriteAll:
				overwrite = true
			default:
				switch rep.Ask(Question{Kind: QOverwrite, Path: dst}) {
				case AnswerNo:
					led.skip()
					continue
				case AnswerNoToAll:
					skipAll = true
					led.skip()
					continue
				case AnswerYes:
					overwrite = true
				case AnswerYesToAll:
					overwriteAll = true
					overwrite = true
				default: // AnswerCancel
					led.cancel()
					return nil, false
				}
			}
		}
		todo = append(todo, resolved{target: t, overwrite: overwrite})
	}
	return todo, true
}

// workers is the pool width in force: the configured one, else the default.
func (d *Download) workers() int {
	if d.Workers > 0 {
		return d.Workers
	}
	return DownloadWorkers
}

// transfer is phase 2: move the bytes, d.workers() at a time.
func (d *Download) transfer(ctx context.Context, rep Reporter, led *ledger, todo []resolved, totalBytes int64) {
	// live tracks the bytes moved so far by transfers that have not finished,
	// keyed by object, so the gauge stays honest while several run at once.
	var mu sync.Mutex
	live := make(map[string]int64)
	var doneBytes int64
	var doneCount int

	report := func() {
		mu.Lock()
		var inFlight int64
		names := make([]string, 0, len(live))
		for k, b := range live {
			inFlight += b
			names = append(names, path.Base(k))
		}
		p := Progress{
			Bytes:      doneBytes + inFlight,
			TotalBytes: totalBytes,
			Items:      doneCount,
			TotalItems: len(todo),
			Active:     names,
		}
		mu.Unlock()
		rep.Progress(p)
	}

	sem := make(chan struct{}, d.workers())
	var wg sync.WaitGroup

	for _, r := range todo {
		// Check the context before the select rather than only inside it: with
		// a free slot and a dead context both ready, select picks at random,
		// so a cancelled run could keep handing out work for several more
		// objects. Cancellation stops dispatch here and lets the transfers
		// already started drain.
		if ctx.Err() != nil {
			led.cancel()
			break
		}
		select {
		case <-ctx.Done():
			led.cancel()
		case sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				d.one(ctx, rep, led, r, &mu, live, &doneBytes, &doneCount, report)
			}()
			continue
		}
		break
	}

	wg.Wait()
}

// one transfers a single object and records its outcome.
func (d *Download) one(
	ctx context.Context,
	rep Reporter,
	led *ledger,
	r resolved,
	mu *sync.Mutex,
	live map[string]int64,
	doneBytes *int64,
	doneCount *int,
	report func(),
) {
	key := r.target.Key

	mu.Lock()
	live[key] = 0
	mu.Unlock()
	report()

	n, err := d.Fetcher.DownloadTarget(ctx, r.target, d.SrcPrefix, d.DestDir, d.Bucket.Key, r.overwrite,
		func(written, _ int64, _ string) {
			mu.Lock()
			live[key] = written
			mu.Unlock()
			report()
		})

	mu.Lock()
	delete(live, key)
	mu.Unlock()

	switch {
	case ctx.Err() != nil:
		led.cancel()
		return
	case err != nil:
		led.fail(key, err)
		return
	}

	if d.Verify {
		if !d.verify(ctx, led, r.target) {
			return
		}
	}

	mu.Lock()
	*doneBytes += n
	*doneCount++
	mu.Unlock()
	led.ok(n, r.overwrite)
	rep.Log("downloaded %s (%s)", key, humanize.IBytes(uint64(n)))
	report()
}

// verify compares the landed file against the object's own checksum. It
// reports false when the download must be counted as a failure.
func (d *Download) verify(ctx context.Context, led *ledger, t model.DownloadTarget) bool {
	local, err := model.SafeLocalPath(d.DestDir, d.SrcPrefix, t.Key)
	if err != nil {
		led.fail(t.Key, fmt.Errorf("downloaded, but verification failed: %w", err))
		return false
	}
	vr, err := d.Checker.VerifyLocalFile(ctx, d.Bucket, t.Key, local)
	switch {
	case err != nil && ctx.Err() != nil:
		// Cancelled mid-verify. The file is already downloaded and in place,
		// so this is neither a failure (a Retry would re-download a good
		// file) nor a verified copy: it is done, unverified, and the run
		// reports itself cancelled.
		led.cancel()
		led.countVerified(false)
		return true
	case err != nil:
		led.fail(t.Key, fmt.Errorf("downloaded, but verification failed: %w", err))
		return false
	case vr.Mismatch:
		led.fail(t.Key, fmt.Errorf("downloaded, but %s", vr.String()))
		return false
	}
	// Not every object can be verified — a multipart upload with no
	// whole-object checksum can only be compared by size — so an unverifiable
	// one is counted, not failed.
	led.countVerified(vr.Verified)
	return true
}

// finish stamps the result with a retry operation covering exactly the objects
// that failed. Their sizes are already known, so a retry gets a real progress
// total without re-resolving anything.
func (d *Download) finish(led *ledger) Result {
	res := led.result()
	if len(res.Failures) == 0 {
		return res
	}

	sizes := make(map[string]int64, len(d.Targets))
	for _, t := range d.Targets {
		sizes[t.Key] = t.Size
	}
	retry := make([]model.DownloadTarget, 0, len(res.Failures))
	var bytes int64
	for _, f := range res.Failures {
		size, known := sizes[f.Label]
		if !known {
			continue // a failure with no object behind it (an empty key)
		}
		retry = append(retry, model.DownloadTarget{Key: f.Label, Size: size})
		bytes += size
	}
	if len(retry) == 0 {
		return res
	}

	next := *d
	next.Targets = retry
	next.TotalBytes = bytes
	next.SelectedCount = len(retry)
	res.Retry = &next
	return res
}
