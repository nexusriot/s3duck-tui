package ops

import (
	"context"
	"fmt"
	"sync"
)

// SyncWorkers is the ceiling on concurrent sync operations, matching the
// download pool. Aggregate bandwidth is still capped by the model's limiter.
const SyncWorkers = 4

// SyncWorkerCount is the pool size for a plan of n operations — never more
// workers than there is work, and always at least one.
func SyncWorkerCount(n int) int {
	if n < 1 {
		return 1
	}
	if n < SyncWorkers {
		return n
	}
	return SyncWorkers
}

// SyncUnit is one operation in a reviewed plan, reduced to what the runner
// needs to know: what to call it, how big it is, and whether it removes
// something. What it actually does is the caller's business — see Sync.Apply —
// which is what keeps this package free of the three sync directions and their
// filesystem and bucket plumbing.
type SyncUnit struct {
	Label string
	Bytes int64
	// Delete marks a removal at the destination. Deletes run only after every
	// write has finished; see Run.
	Delete bool
}

// Sync applies a reviewed plan: writes first, then deletes, with a bounded
// worker pool inside each phase.
type Sync struct {
	// Title is the job label ("upload  /home/u/src → b/dst/").
	Title string
	Units []SyncUnit
	// TotalBytes is the plan's size, used for the gauge. Summed from Units
	// when left at zero.
	TotalBytes int64
	// Workers overrides the pool size. 0 means SyncWorkerCount(len(Units)).
	Workers int
	// WritesRemote says the plan changes the bucket rather than only the local
	// filesystem, which is what decides whether the browser needs re-listing.
	WritesRemote bool

	// Apply performs unit i. onProgress reports the bytes that unit has
	// written so far; a server-side copy moves no bytes through this process
	// and simply never calls it.
	Apply func(ctx context.Context, i int, onProgress func(written int64)) error

	Gate Gate
}

func (s *Sync) Describe() Descriptor {
	total := s.TotalBytes
	if total == 0 {
		for _, u := range s.Units {
			total += u.Bytes
		}
	}
	return Descriptor{
		Kind:         KindSync,
		Title:        s.Title,
		TotalBytes:   total,
		TotalItems:   len(s.Units),
		RefreshAfter: s.WritesRemote,
	}
}

func (s *Sync) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}

	started := Reserve(ctx, s.Gate, func() { s.apply(ctx, rep, led) })
	if !started {
		led.cancel()
	}

	res := led.result()
	rep.Log("Sync: %d ok, %d failed", res.Done, res.Failed())
	return s.withRetry(res)
}

// apply runs the two phases. Writes complete before any delete starts: if the
// run is cancelled or fails partway, the destination has gained the new files
// but not yet lost the old ones, which is the safer of the two intermediate
// states.
//
// The same reasoning skips deletes entirely when any write failed — the
// destination is not the mirror the reviewed plan assumed, so removing
// anything from it is no longer covered by the user's approval.
func (s *Sync) apply(ctx context.Context, rep Reporter, led *ledger) {
	writes, deletes := SplitSyncPhases(s.Units)

	tracker := &syncProgress{total: s.Describe().TotalBytes, units: len(s.Units)}
	s.runPhase(ctx, rep, led, tracker, writes)

	writesFailed := len(led.result().Failures)
	switch {
	case ctx.Err() != nil:
		led.cancel()
	case writesFailed == 0:
		s.runPhase(ctx, rep, led, tracker, deletes)
	case len(deletes) > 0:
		led.fail(fmt.Sprintf("%d delete(s) skipped", len(deletes)),
			fmt.Errorf("%d write(s) failed, so the plan's deletes are no longer covered by your approval", writesFailed))
	}
	if ctx.Err() != nil {
		led.cancel()
	}
}

// runPhase applies one group through a bounded pool. Operations within a phase
// never touch the same path (a relative path is either present at the source or
// not), so they are safe to interleave.
func (s *Sync) runPhase(ctx context.Context, rep Reporter, led *ledger, tracker *syncProgress, phase []IndexedUnit) {
	if len(phase) == 0 {
		return
	}
	workers := s.Workers
	if workers <= 0 {
		workers = SyncWorkerCount(len(s.Units))
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	// Wait even when dispatch stops on cancellation: the caller reads the
	// ledger as soon as this returns, so leaving workers running would race
	// them and under-report whatever the stragglers did.
	defer wg.Wait()

	for _, item := range phase {
		// Checked before the select: with a free slot and a dead context both
		// ready, select picks at random, so a cancelled run could keep handing
		// out work.
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(it IndexedUnit) {
			defer wg.Done()
			defer func() { <-sem }()

			rep.Progress(tracker.sample(it, 0))
			err := s.Apply(ctx, it.Index, func(written int64) {
				rep.Progress(tracker.sample(it, written))
			})

			tracker.finish(it.Index)
			if err != nil {
				tracker.done(0)
				if ctx.Err() != nil {
					// A cancel, not a failure: apply marks the run cancelled.
					// Ledgering the in-flight units as failed offered a "Retry
					// failed" covering only them, silently dropping every unit
					// never dispatched and every delete.
					return
				}
				led.fail(it.Unit.Label, err)
				return
			}
			led.ok(it.Unit.Bytes, false)
			tracker.done(it.Unit.Bytes)
			rep.Progress(tracker.sample(it, 0))
		}(item)
	}
}

// withRetry stamps the result with a plan covering exactly the units that
// failed. Re-running only those is the point of keeping them: a 5000-file sync
// that lost twelve objects to a flaky link should cost twelve operations, not
// another full plan.
func (s *Sync) withRetry(res Result) Result {
	if len(res.Failures) == 0 {
		return res
	}
	byLabel := make(map[string]int, len(s.Units))
	for i, u := range s.Units {
		byLabel[u.Label] = i
	}

	var units []SyncUnit
	var origin []int // new index -> index in the original plan
	for _, f := range res.Failures {
		i, ok := byLabel[f.Label]
		if !ok {
			continue // the synthetic "deletes skipped" entry has no unit
		}
		units = append(units, s.Units[i])
		origin = append(origin, i)
	}
	if len(units) == 0 {
		return res
	}

	apply := s.Apply
	next := *s
	next.Units = units
	next.TotalBytes = 0 // recomputed from the surviving units
	next.Apply = func(ctx context.Context, i int, onProgress func(int64)) error {
		return apply(ctx, origin[i], onProgress)
	}
	res.Retry = &next
	return res
}

// IndexedUnit pairs a unit with its position in the plan, so a worker can
// report against a stable slot while running out of order.
type IndexedUnit struct {
	Index int
	Unit  SyncUnit
}

// SplitSyncPhases separates the plan into the units that write to the
// destination and the ones that remove from it.
func SplitSyncPhases(units []SyncUnit) (writes, deletes []IndexedUnit) {
	for i, u := range units {
		if u.Delete {
			deletes = append(deletes, IndexedUnit{Index: i, Unit: u})
			continue
		}
		writes = append(writes, IndexedUnit{Index: i, Unit: u})
	}
	return writes, deletes
}

// syncProgress is the shared gauge. inFlight holds the bytes moved by units
// that have not finished, keyed by plan index, so the displayed total stays
// correct while several transfer at once.
type syncProgress struct {
	mu        sync.Mutex
	inFlight  map[int]int64
	doneBytes int64
	seen      int
	total     int64
	units     int
}

func (p *syncProgress) sample(it IndexedUnit, written int64) Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	if written > 0 {
		if p.inFlight == nil {
			p.inFlight = map[int]int64{}
		}
		p.inFlight[it.Index] = written
	}
	n := p.doneBytes
	for _, v := range p.inFlight {
		n += v
	}
	return Progress{
		Bytes:      n,
		TotalBytes: p.total,
		Items:      p.seen,
		TotalItems: p.units,
		Active:     []string{it.Unit.Label},
	}
}

func (p *syncProgress) finish(index int) {
	p.mu.Lock()
	delete(p.inFlight, index)
	p.mu.Unlock()
}

func (p *syncProgress) done(bytes int64) {
	p.mu.Lock()
	p.doneBytes += bytes
	p.seen++
	p.mu.Unlock()
}
