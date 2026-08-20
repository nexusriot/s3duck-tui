package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

// syncDirection is which side of the sync is the source of truth.
type syncDirection int

// The constant order matches the dropdown in syncDirectionLabels, so the
// selected index is the direction.
const (
	syncUpload   syncDirection = iota // local dir → S3 prefix
	syncDownload                      // S3 prefix → local dir
	syncRemote                        // S3 prefix → S3 prefix (server-side copy)
)

func (d syncDirection) String() string {
	switch d {
	case syncDownload:
		return "remote → local"
	case syncRemote:
		return "remote → remote"
	default:
		return "local → remote"
	}
}

// syncOpKind is what a planned operation does to the destination.
type syncOpKind int

const (
	syncCreate syncOpKind = iota // not present at the destination
	syncUpdate                   // present but differs
	syncDelete                   // present at the destination only
)

func (k syncOpKind) String() string {
	switch k {
	case syncUpdate:
		return "update"
	case syncDelete:
		return "delete"
	default:
		return "create"
	}
}

// syncOp is one planned transfer or removal, addressed by the path relative to
// the sync roots. Bytes is what the transfer will move (0 for a delete).
type syncOp struct {
	Kind  syncOpKind
	Rel   string
	Bytes int64
	// Reason explains an update in the plan preview ("size 10 B → 12 B").
	Reason string
}

// syncModTolerance absorbs clock skew and the coarser timestamp granularity of
// some filesystems and S3 backends. A source file must be newer than the
// destination by more than this before a same-size file is re-transferred.
const syncModTolerance = 2 * time.Second

// planSync diffs src against dst and returns the operations that would make dst
// match src, ordered creates → updates → deletes, each group sorted by path so
// the plan is deterministic and reviewable.
//
// A file is transferred when it is missing at the destination, when the sizes
// differ, or when the sizes match but the source is more than syncModTolerance
// newer. Deletes are only emitted when del is set; without it, sync never
// removes anything.
func planSync(src, dst []model.SyncEntry, del bool) []syncOp {
	dstByRel := make(map[string]model.SyncEntry, len(dst))
	for _, e := range dst {
		dstByRel[e.Rel] = e
	}

	var creates, updates, deletes []syncOp
	srcSeen := make(map[string]bool, len(src))

	for _, s := range src {
		srcSeen[s.Rel] = true
		d, ok := dstByRel[s.Rel]
		if !ok {
			creates = append(creates, syncOp{Kind: syncCreate, Rel: s.Rel, Bytes: s.Size})
			continue
		}
		if s.Size != d.Size {
			updates = append(updates, syncOp{
				Kind:   syncUpdate,
				Rel:    s.Rel,
				Bytes:  s.Size,
				Reason: fmt.Sprintf("size %s → %s", humanize.IBytes(uint64(d.Size)), humanize.IBytes(uint64(s.Size))),
			})
			continue
		}
		if !s.Mod.IsZero() && !d.Mod.IsZero() && s.Mod.Sub(d.Mod) > syncModTolerance {
			updates = append(updates, syncOp{
				Kind:   syncUpdate,
				Rel:    s.Rel,
				Bytes:  s.Size,
				Reason: "source is newer",
			})
		}
	}

	if del {
		for _, d := range dst {
			if !srcSeen[d.Rel] {
				deletes = append(deletes, syncOp{Kind: syncDelete, Rel: d.Rel})
			}
		}
	}

	byRel := func(ops []syncOp) {
		sort.Slice(ops, func(i, j int) bool { return ops[i].Rel < ops[j].Rel })
	}
	byRel(creates)
	byRel(updates)
	byRel(deletes)

	out := make([]syncOp, 0, len(creates)+len(updates)+len(deletes))
	out = append(out, creates...)
	out = append(out, updates...)
	out = append(out, deletes...)
	return out
}

// syncStats is the rolled-up shape of a plan.
type syncStats struct {
	Creates, Updates, Deletes int
	Bytes                     int64
}

// summarizeSync counts the operations by kind and totals the bytes to transfer.
func summarizeSync(ops []syncOp) syncStats {
	var s syncStats
	for _, op := range ops {
		switch op.Kind {
		case syncCreate:
			s.Creates++
		case syncUpdate:
			s.Updates++
		case syncDelete:
			s.Deletes++
		}
		s.Bytes += op.Bytes
	}
	return s
}

// syncPlanText renders the dry-run plan: the totals, then up to maxRows
// operations with the reason each was selected.
func syncPlanText(dir syncDirection, srcLabel, dstLabel string, ops []syncOp, maxRows int) string {
	st := summarizeSync(ops)

	var b strings.Builder
	fmt.Fprintf(&b, "Sync %s\n", dir)
	fmt.Fprintf(&b, "  from: %s\n", srcLabel)
	fmt.Fprintf(&b, "  to:   %s\n\n", dstLabel)

	if len(ops) == 0 {
		b.WriteString("Already in sync — nothing to do.")
		return b.String()
	}

	fmt.Fprintf(&b, "%d create, %d update, %d delete  (%s to transfer)\n\n",
		st.Creates, st.Updates, st.Deletes, humanize.IBytes(uint64(st.Bytes)))

	for i, op := range ops {
		if i == maxRows {
			fmt.Fprintf(&b, "  ...and %d more\n", len(ops)-maxRows)
			break
		}
		line := fmt.Sprintf("  %-6s %s", op.Kind, op.Rel)
		if op.Reason != "" {
			line += fmt.Sprintf("  (%s)", op.Reason)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// syncPlanRows is how many operations the preview lists before truncating.
const syncPlanRows = 40

// syncWorkers is the ceiling on concurrent sync operations, matching the
// download pool. Aggregate bandwidth is still capped by model.Limiter.
const syncWorkers = 4

// syncWorkerCount is the pool size for a plan of n operations — never more
// workers than there is work, and always at least one.
func syncWorkerCount(n int) int {
	if n < 1 {
		return 1
	}
	if n < syncWorkers {
		return n
	}
	return syncWorkers
}

// indexedOp pairs an operation with its position in the plan, so a worker can
// report progress against a stable slot while running out of order.
type indexedOp struct {
	index int
	op    syncOp
}

// splitSyncPhases separates the plan into the operations that write to the
// destination and the ones that remove from it. Deletes run only after every
// write has finished (see runSync), so a partial run never leaves the
// destination missing data it hasn't gained a replacement for.
func splitSyncPhases(ops []syncOp) (writes, deletes []indexedOp) {
	for i, op := range ops {
		if op.Kind == syncDelete {
			deletes = append(deletes, indexedOp{index: i, op: op})
			continue
		}
		writes = append(writes, indexedOp{index: i, op: op})
	}
	return writes, deletes
}

// syncSpec describes one sync run: which way it goes and what the two sides
// are. Introducing it replaced a growing parameter list — the local↔remote
// flow only ever needed one bucket, but a remote↔remote run has two, and
// threading both through every function was where mistakes would live.
//
// Only the fields the direction uses are populated:
//
//	syncUpload    localDir            → dstBucket/dstPrefix
//	syncDownload  srcBucket/srcPrefix → localDir
//	syncRemote    srcBucket/srcPrefix → dstBucket/dstPrefix
type syncSpec struct {
	dir       syncDirection
	localDir  string
	srcBucket *model.Object
	srcPrefix string
	dstBucket *model.Object
	dstPrefix string
	del       bool
}

// prefixesOverlap reports whether one normalized prefix contains the other
// (equality included). A same-bucket sync between overlapping prefixes is
// rejected: the source listing would include the destination's objects, so
// every run copies the destination into itself one level deeper — syncing a
// bucket root into "mirror/" produces mirror/mirror/… on the second run and
// grows without bound. Note "" (the bucket root) overlaps every prefix.
func prefixesOverlap(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// remoteLabel renders a bucket+prefix pair for display.
func remoteLabel(bucket *model.Object, prefix string) string {
	if bucket == nil || bucket.Key == nil {
		return "(no bucket)"
	}
	return fmt.Sprintf("%s/%s", *bucket.Key, prefix)
}

// srcLabel / dstLabel name the two sides of a spec for plan output.
func (s syncSpec) srcLabel() string {
	if s.dir == syncUpload {
		return s.localDir
	}
	return remoteLabel(s.srcBucket, s.srcPrefix)
}

func (s syncSpec) dstLabel() string {
	if s.dir == syncDownload {
		return s.localDir
	}
	return remoteLabel(s.dstBucket, s.dstPrefix)
}

// Sync opens the sync dialog for the current bucket + prefix, which is always
// the source (upload aside). The preview is always shown first — sync is the
// one operation here that can both overwrite and (optionally) delete, so it
// never runs unreviewed.
func (c *Controller) Sync() {
	if c.currentBucket == nil {
		go c.error("Sync", fmt.Errorf("open a bucket first"))
		return
	}

	bucket := c.currentBucket
	prefix := model.NormalizePrefix(c.currentPath)

	// In dual-pane mode the other pane is the obvious remote destination. Read
	// the pane state here, on the UI goroutine — the goroutine below must not
	// touch c.dual/c.panes/c.active, which the pane-switch handlers write.
	dstName, dstPrefix := *bucket.Key, prefix
	if c.dual {
		if other := c.panes[1-c.active]; other.currentBucket != nil {
			dstName = *other.currentBucket.Key
			dstPrefix = model.NormalizePrefix(other.currentPath)
		}
	}

	// The destination-bucket dropdown needs the bucket list, which is a network
	// call; fetch it off the UI goroutine and fall back to just this bucket.
	mdl := c.model
	go func() {
		bucketNames := []string{*bucket.Key}
		if list, err := mdl.ListBuckets(); err == nil {
			bucketNames = bucketNames[:0]
			for _, b := range list {
				if b != nil && b.Key != nil {
					bucketNames = append(bucketNames, *b.Key)
				}
			}
			if len(bucketNames) == 0 {
				bucketNames = []string{*bucket.Key}
			}
		}

		c.view.App.QueueUpdateDraw(func() {
			c.showSyncForm(bucket, prefix, bucketNames, dstName, dstPrefix)
		})
	}()
}

// showSyncForm builds and displays the sync dialog. Runs on the UI goroutine.
func (c *Controller) showSyncForm(bucket *model.Object, prefix string, bucketNames []string, dstName, dstPrefix string) {
	localDefault := strings.TrimSuffix(c.resolveDownloadDir(), string(os.PathSeparator))
	form := c.view.NewSyncForm(remoteLabel(bucket, prefix), localDefault,
		syncDirectionLabels(), bucketNames, dstName, dstPrefix)

	readSpec := func() (syncSpec, error) {
		dirIdx, _ := form.GetFormItemByLabel(view.FieldSyncDirection).(*tview.DropDown).GetCurrentOption()
		localDir := strings.TrimSpace(form.GetFormItemByLabel(view.FieldSyncLocalDir).(*tview.InputField).GetText())
		_, dstBucketName := form.GetFormItemByLabel(view.FieldSyncDstBucket).(*tview.DropDown).GetCurrentOption()
		dstPfx := model.NormalizePrefix(form.GetFormItemByLabel(view.FieldSyncDstPrefix).(*tview.InputField).GetText())
		del := form.GetFormItemByLabel(view.FieldSyncDelete).(*tview.Checkbox).IsChecked()

		spec := syncSpec{dir: syncDirection(dirIdx), del: del, localDir: localDir}
		switch spec.dir {
		case syncUpload:
			if localDir == "" {
				return spec, fmt.Errorf("local directory is required")
			}
			spec.dstBucket, spec.dstPrefix = bucket, prefix
		case syncDownload:
			if localDir == "" {
				return spec, fmt.Errorf("local directory is required")
			}
			spec.srcBucket, spec.srcPrefix = bucket, prefix
		case syncRemote:
			spec.srcBucket, spec.srcPrefix = bucket, prefix
			name := dstBucketName
			spec.dstBucket = &model.Object{Key: &name, Ot: model.Bucket}
			spec.dstPrefix = dstPfx
			if name == *bucket.Key && prefixesOverlap(prefix, dstPfx) {
				return spec, fmt.Errorf("source and destination prefixes overlap in the same bucket")
			}
		}
		return spec, nil
	}

	form.AddButton("Preview", func() {
		spec, err := readSpec()
		c.view.Pages.RemovePage("modal")
		if err != nil {
			go c.error("Sync", err)
			return
		}
		c.previewSync(spec)
	})
	form.AddButton("Browse…", func() {
		// The picker lives on its own page ("modal-dir") and simply overlays
		// this form; closing it — chosen OR canceled — reveals the form with
		// everything still typed in. (It used to remove and conditionally
		// re-add the form, so canceling the picker discarded the whole form.)
		c.chooseDir(c.params.HomeDir, func(chosen string) {
			form.GetFormItemByLabel(view.FieldSyncLocalDir).(*tview.InputField).
				SetText(strings.TrimSuffix(chosen, string(os.PathSeparator)))
		})
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 82, syncFormHeight), true, true)
}

// syncFormHeight sizes the sync dialog.
const syncFormHeight = 17

// syncDirectionLabels are the dropdown options, ordered to match the
// syncDirection constants so the selected index IS the direction.
func syncDirectionLabels() []string {
	return []string{
		"local → remote (upload)",
		"remote → local (download)",
		"remote → remote (bucket/prefix copy)",
	}
}

// collectSides gathers the entry lists for both sides of a spec. A missing
// local directory is fatal for an upload (nothing to send) but normal for a
// download's first run, where the transfer will create it.
func (c *Controller) collectSides(mdl *model.Model, spec syncSpec) (src, dst []model.SyncEntry, err error) {
	local := func() ([]model.SyncEntry, error) {
		entries, err := model.WalkLocal(spec.localDir)
		if err != nil && spec.dir == syncDownload && os.IsNotExist(err) {
			return nil, nil
		}
		return entries, err
	}

	switch spec.dir {
	case syncUpload:
		if src, err = local(); err != nil {
			return nil, nil, err
		}
		dst, err = mdl.ListRemoteEntries(spec.dstPrefix, spec.dstBucket)
	case syncDownload:
		if src, err = mdl.ListRemoteEntries(spec.srcPrefix, spec.srcBucket); err != nil {
			return nil, nil, err
		}
		dst, err = local()
	default: // syncRemote
		if src, err = mdl.ListRemoteEntries(spec.srcPrefix, spec.srcBucket); err != nil {
			return nil, nil, err
		}
		dst, err = mdl.ListRemoteEntries(spec.dstPrefix, spec.dstBucket)
	}
	if err != nil {
		return nil, nil, err
	}
	return src, dst, nil
}

// previewSync scans both sides off the UI goroutine and shows the plan with an
// Apply button. Scanning is a paginated listing (or a filesystem walk), so it
// runs behind a "Scanning…" modal.
func (c *Controller) previewSync(spec syncSpec) {
	mdl := c.model
	scanning := tview.NewModal().SetText("Scanning both sides...")
	c.view.Pages.AddPage("progress", scanning, true, true)

	go func() {
		src, dst, err := c.collectSides(mdl, spec)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress").SwitchToPage("main") })
			c.error("Sync scan failed", err)
			return
		}

		ops := planSync(src, dst, spec.del)
		text := syncPlanText(spec.dir, spec.srcLabel(), spec.dstLabel(), ops, syncPlanRows)

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
			c.showPlan(" Sync plan (dry run) ", text, func() { c.runSync(spec, ops) }, len(ops) > 0)
		})
	}()
}

// showPlan displays a plan in a scrollable modal. onApply is offered only when
// applicable is true, which is what makes the same widget serve both the sync
// preview and the read-only pane comparison.
func (c *Controller) showPlan(title, text string, onApply func(), applicable bool) {
	tv := tview.NewTextView().SetText(text).SetScrollable(true)
	tv.SetBorder(true).SetTitle(title)

	buttons := tview.NewForm()
	if applicable && onApply != nil {
		buttons.AddButton("Apply", func() {
			c.view.Pages.RemovePage("modal")
			onApply()
		})
	}
	buttons.AddButton("Close", func() { c.view.Pages.RemovePage("modal") })

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tv, 0, 1, false).
		AddItem(buttons, 3, 0, true)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage("modal")
			return nil
		}
		return event
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(flex, 86, 28), true, true)
}

// runSync applies a reviewed plan as a background-able transfer job.
// Failures are collected rather than aborting the run, so one unreadable file
// doesn't strand the rest of the sync.
func (c *Controller) runSync(spec syncSpec, ops []syncOp) {
	// Runs on the UI goroutine: capture the client before spawning workers so
	// a profile switch can't retarget a queued/backgrounded sync.
	mdl := c.model
	ctx, cancel := context.WithCancel(context.Background())
	st := summarizeSync(ops)

	job := c.addJob("sync", fmt.Sprintf("%s  %s → %s", spec.dir, spec.srcLabel(), spec.dstLabel()), st.Bytes, len(ops), cancel)

	progress := tview.NewModal().
		SetText("Starting sync...\n").
		AddButtons([]string{"Background", "Cancel"}).
		SetDoneFunc(func(_ int, buttonLabel string) {
			switch buttonLabel {
			case "Background":
				job.setBackgrounded()
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
				c.view.App.SetFocus(c.view.List)
			default:
				cancel()
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			}
		})
	c.view.Pages.AddPage("progress", progress, true, true)

	go func() {
		job.setStatus(jobQueued)
		select {
		case c.jobSem <- struct{}{}:
			defer func() { <-c.jobSem }()
		case <-ctx.Done():
			c.finalizeJob(job, true, 0)
			// RemovePage only: SwitchToPage("main") would also hide the
			// transfers panel the cancel was likely issued from.
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			return
		}
		job.setStatus(jobRunning)

		start := time.Now()

		// prog holds everything the workers share. inFlight tracks bytes moved
		// by operations that haven't finished yet, keyed by op index, so the
		// displayed total stays correct while several files transfer at once.
		var mu sync.Mutex
		var lastDraw time.Time
		var doneBytes int64
		var failed []string
		inFlight := map[int]int64{}
		okCount := 0
		doneCount := 0

		draw := func(i int, op syncOp, fileDone int64) {
			mu.Lock()
			if fileDone > 0 {
				inFlight[i] = fileDone
			}
			n := doneBytes
			for _, v := range inFlight {
				n += v
			}
			seen := doneCount
			now := time.Now()
			throttled := now.Sub(lastDraw) < 100*time.Millisecond
			if !throttled {
				lastDraw = now
			}
			mu.Unlock()

			job.setProgress(n, seen)
			if job.isBackgrounded() || throttled {
				return
			}
			pct := 0.0
			if st.Bytes > 0 {
				pct = float64(n) / float64(st.Bytes) * 100
			}
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf(
					"Syncing %s [%d workers]\n%d/%d op(s)\n%s/%s (%.1f%%)\n%s\n%s %s",
					spec.dir, syncWorkerCount(len(ops)), seen, len(ops),
					humanize.IBytes(uint64(n)), humanize.IBytes(uint64(st.Bytes)), pct,
					byteRateETA(n, st.Bytes, time.Since(start)),
					op.Kind, op.Rel,
				))
			})
		}

		// runPhase applies one group of operations through a bounded worker
		// pool. Operations within a phase never touch the same path (a rel is
		// either present at the source or not), so they are safe to interleave.
		runPhase := func(phase []indexedOp) {
			sem := make(chan struct{}, syncWorkerCount(len(ops)))
			var wg sync.WaitGroup

			// wg.Wait runs even when dispatch stops on cancellation — the
			// caller reads the shared counters right after runPhase returns,
			// so returning with workers still running would race them and
			// under-report whatever the stragglers did.
			defer wg.Wait()

			for _, item := range phase {
				select {
				case <-ctx.Done():
					return
				case sem <- struct{}{}:
				}

				wg.Add(1)
				go func(it indexedOp) {
					defer wg.Done()
					defer func() { <-sem }()

					draw(it.index, it.op, 0)
					err := c.applySyncOp(ctx, mdl, spec, it.op, func(written int64) {
						draw(it.index, it.op, written)
					})

					mu.Lock()
					delete(inFlight, it.index)
					doneCount++
					if err != nil {
						failed = append(failed, fmt.Sprintf("%s %s: %v", it.op.Kind, it.op.Rel, err))
					} else {
						okCount++
						doneBytes += it.op.Bytes
					}
					mu.Unlock()
				}(item)
			}
		}

		// Writes complete before any delete runs: if the transfer is cancelled
		// or fails partway, the destination has gained the new files but not yet
		// lost the old ones, which is the safer of the two intermediate states.
		// The same reasoning skips deletes entirely when any write FAILED — the
		// destination is not the mirror the reviewed plan assumed, so removing
		// anything from it is no longer covered by the user's approval.
		writes, deletes := splitSyncPhases(ops)
		runPhase(writes)
		mu.Lock()
		writesFailed := len(failed)
		mu.Unlock()
		if ctx.Err() == nil && writesFailed == 0 {
			runPhase(deletes)
		} else if writesFailed > 0 && len(deletes) > 0 {
			mu.Lock()
			failed = append(failed, fmt.Sprintf("%d delete(s) skipped: %d write(s) failed", len(deletes), writesFailed))
			mu.Unlock()
		}
		canceled := ctx.Err() != nil

		c.logActivity("Sync %s: %d ok, %d failed (%s)", spec.dir, okCount, len(failed), humanize.IBytes(uint64(doneBytes)))
		c.finalizeJob(job, canceled, len(failed))

		if canceled || job.isBackgrounded() {
			c.refreshAfterSync(spec)
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			// Re-check ON the UI goroutine: a Background press racing job
			// completion may have removed the progress page after the check
			// above — mutating and focusing a detached modal would route all
			// input into an invisible widget with no way back.
			if job.isBackgrounded() || !c.view.Pages.HasPage("progress") {
				return
			}
			status := "Sync complete."
			if len(failed) > 0 {
				status = "Sync finished with errors."
			}
			msg := fmt.Sprintf("%s\n\nApplied: %d\nFailed: %d\nTransferred: %s",
				status, okCount, len(failed), humanize.IBytes(uint64(doneBytes)))
			for i, f := range failed {
				if i == 8 {
					msg += fmt.Sprintf("\n  ...and %d more", len(failed)-8)
					break
				}
				msg += "\n  - " + f
			}
			msg += "\n\nPress Done to return."

			progress.ClearButtons()
			progress.SetText(msg)
			progress.AddButtons([]string{"Done"})
			progress.SetDoneFunc(func(_ int, _ string) {
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			})
			c.view.App.SetFocus(progress)
		})

		c.refreshAfterSync(spec)
	}()
}

// refreshAfterSync re-lists the browser only when the run could have changed
// what it is showing — that is, whenever the destination was remote.
func (c *Controller) refreshAfterSync(spec syncSpec) {
	if spec.dir != syncDownload {
		c.updateList()
	}
}

// applySyncOp performs one planned operation. Which side is written follows
// from the direction: an upload writes to (and deletes from) S3, a download
// writes to the local tree, and a remote→remote run copies server-side between
// the two prefixes.
func (c *Controller) applySyncOp(ctx context.Context, mdl *model.Model, spec syncSpec, op syncOp, onProgress func(written int64)) error {
	switch spec.dir {
	case syncUpload:
		key := spec.dstPrefix + op.Rel
		if op.Kind == syncDelete {
			return mdl.DeleteKey(ctx, key, spec.dstBucket)
		}
		localPath := filepath.Join(spec.localDir, filepath.FromSlash(op.Rel))
		return mdl.UploadFile(ctx, localPath, key, spec.dstBucket, func(written, _ int64) {
			onProgress(written)
		})

	case syncDownload:
		localPath := filepath.Join(spec.localDir, filepath.FromSlash(op.Rel))
		if op.Kind == syncDelete {
			return os.Remove(localPath)
		}
		if err := os.MkdirAll(filepath.Dir(localPath), 0760); err != nil {
			return err
		}
		// The plan already said this file must be replaced. overwrite=true
		// lets the model swap it atomically once the download succeeded —
		// pre-removing it here used to destroy the local copy even when the
		// transfer then failed or was canceled.
		_, err := mdl.DownloadTarget(ctx, model.DownloadTarget{Key: spec.srcPrefix + op.Rel, Size: op.Bytes},
			spec.srcPrefix, spec.localDir, spec.srcBucket.Key, true, func(written, _ int64, _ string) {
				onProgress(written)
			})
		return err

	default: // syncRemote
		dstKey := spec.dstPrefix + op.Rel
		if op.Kind == syncDelete {
			return mdl.DeleteKey(ctx, dstKey, spec.dstBucket)
		}
		// A server-side copy moves no bytes through this process, so there is no
		// progress to report — the op counter is the only thing that advances.
		return mdl.CopyObject(ctx, spec.srcBucket, spec.dstBucket, spec.srcPrefix+op.Rel, dstKey)
	}
}

// comparePlanText renders a pane comparison. It reuses the sync planner but
// re-words the three kinds: a comparison has no notion of "create" or "delete",
// only of what each side holds.
func comparePlanText(leftLabel, rightLabel string, ops []syncOp, maxRows int) string {
	st := summarizeSync(ops)

	var b strings.Builder
	fmt.Fprintf(&b, "Compare\n  left:  %s\n  right: %s\n\n", leftLabel, rightLabel)

	if len(ops) == 0 {
		b.WriteString("Identical — same names and sizes on both sides.")
		return b.String()
	}

	fmt.Fprintf(&b, "%d only on the left, %d differing, %d only on the right\n\n",
		st.Creates, st.Updates, st.Deletes)

	for i, op := range ops {
		if i == maxRows {
			fmt.Fprintf(&b, "  ...and %d more\n", len(ops)-maxRows)
			break
		}
		line := fmt.Sprintf("  %-11s %s", compareLabel(op.Kind), op.Rel)
		if op.Reason != "" {
			line += fmt.Sprintf("  (%s)", op.Reason)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nComparison is by name and size only; contents are not read.")
	return b.String()
}

// compareLabel maps a plan op kind to comparison wording.
func compareLabel(k syncOpKind) string {
	switch k {
	case syncUpdate:
		return "differs"
	case syncDelete:
		return "right-only"
	default:
		return "left-only"
	}
}

// ComparePanes diffs the two dual-pane locations without changing anything.
// It answers "are these two prefixes the same?", which the browser otherwise
// cannot express — and it is the same planner the sync preview uses, so the two
// can never disagree about what counts as a difference.
func (c *Controller) ComparePanes() {
	if !c.dual {
		go c.error("Compare", fmt.Errorf("compare needs dual-pane mode (Ctrl+O)"))
		return
	}
	other := c.panes[1-c.active]
	if c.currentBucket == nil || other.currentBucket == nil {
		go c.error("Compare", fmt.Errorf("open a bucket in both panes first"))
		return
	}

	left := syncSpec{dir: syncRemote,
		srcBucket: c.currentBucket, srcPrefix: model.NormalizePrefix(c.currentPath),
		dstBucket: other.currentBucket, dstPrefix: model.NormalizePrefix(other.currentPath)}

	scanning := tview.NewModal().SetText("Comparing both panes...")
	c.view.Pages.AddPage("progress", scanning, true, true)

	mdl := c.model
	go func() {
		src, dst, err := c.collectSides(mdl, left)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress").SwitchToPage("main") })
			c.error("Compare failed", err)
			return
		}
		// del=true so entries present only on the right are reported too; a
		// comparison must be symmetric even though the planner is directional.
		ops := planSync(src, dst, true)
		text := comparePlanText(left.srcLabel(), left.dstLabel(), ops, syncPlanRows)

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
			c.showPlan(" Pane comparison ", text, nil, false)
		})
	}()
}
