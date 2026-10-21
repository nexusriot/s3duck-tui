package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
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
//
// When both sides carry a comparable checksum (same algorithm), that decides
// it instead: equal sums mean no transfer whatever the timestamps say, and
// different sums mean a transfer whatever the sizes say. This is what closes
// the two documented holes in size+mtime comparison — a file edited in place
// to the same length is now seen, and a download-then-upload no longer
// re-sends every file because its local mtime is newer.
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
		// Content comparison wins when it is available on both sides.
		if sa, _ := model.SumAlgo(s.Sum); sa != "" {
			if da, _ := model.SumAlgo(d.Sum); da == sa {
				if s.Sum == d.Sum {
					continue // identical content: nothing to do
				}
				updates = append(updates, syncOp{
					Kind:   syncUpdate,
					Rel:    s.Rel,
					Bytes:  s.Size,
					Reason: fmt.Sprintf("content differs (%s)", sa),
				})
				continue
			}
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
	// checksum asks for content comparison instead of size+mtime. It costs a
	// request per remote object and a full read per local one, which is why
	// it is a choice and not the default.
	checksum bool
	// exclude holds glob/regex patterns (match.go syntax) applied to both
	// sides' relative paths before the diff. Without it sync could not be
	// pointed at any real working tree: .git/, node_modules/ and *.tmp would
	// all be mirrored.
	exclude []string
	// trash is the profile's safe-delete prefix ("" when off), relative to
	// the bucket root. Remote entries under it are dropped from both sides:
	// it is the app's own bookkeeping, and a delete-extraneous sync of the
	// bucket root used to plan — and run, as real deletes — the removal of
	// everything in the trash.
	trash string
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
	if c.remoteOnly("Sync") {
		return
	}
	if c.pane().Bucket() == nil {
		c.error("Sync", fmt.Errorf("open a bucket first"))
		return
	}

	bucket := c.pane().Bucket()
	prefix := model.NormalizePrefix(c.pane().Path())

	// In dual-pane mode the other pane is the obvious remote destination. Read
	// it here, on the UI goroutine: c.dual/c.active are pane-switch state the
	// goroutine below must not touch (the pane's own contents are locked, but
	// which pane is "other" is not).
	dstName, dstPrefix := *bucket.Key, prefix
	if c.dual {
		if o := c.other(); o.Bucket() != nil {
			dstName = *o.Bucket().Key
			dstPrefix = model.NormalizePrefix(o.Path())
		}
	}

	// The destination-bucket dropdown needs the bucket list, which is a network
	// call; fetch it off the UI goroutine and fall back to just this bucket.
	mdl := c.model
	go func() {
		bucketNames := []string{*bucket.Key}
		if list, err := mdl.ListBuckets(context.Background()); err == nil {
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

		c.shell.Do(func() {
			c.showSyncForm(bucket, prefix, bucketNames, dstName, dstPrefix)
		})
	}()
}

// showSyncForm builds and displays the sync dialog. Runs on the UI goroutine.
func (c *Controller) showSyncForm(bucket *model.Object, prefix string, bucketNames []string, dstName, dstPrefix string) {
	// lay is assigned at the bottom; the buttons below fire only after that.
	var lay *shell.Layer
	localDefault := strings.TrimSuffix(c.resolveDownloadDir(), string(os.PathSeparator))
	form := c.view.NewSyncForm(remoteLabel(bucket, prefix), localDefault,
		syncDirectionLabels(), bucketNames, dstName, dstPrefix)

	readSpec := func() (syncSpec, error) {
		dirIdx, _ := form.GetFormItemByLabel(widgets.FieldSyncDirection).(*tview.DropDown).GetCurrentOption()
		// "~" is expanded as everywhere else a directory is typed; left alone
		// it named a directory literally called "~" under the working
		// directory, which a download sync then created.
		localDir := expandHome(strings.TrimSpace(form.GetFormItemByLabel(widgets.FieldSyncLocalDir).(*tview.InputField).GetText()), c.params.HomeDir)
		_, dstBucketName := form.GetFormItemByLabel(widgets.FieldSyncDstBucket).(*tview.DropDown).GetCurrentOption()
		dstPfx := model.NormalizePrefix(form.GetFormItemByLabel(widgets.FieldSyncDstPrefix).(*tview.InputField).GetText())
		del := form.GetFormItemByLabel(widgets.FieldSyncDelete).(*tview.Checkbox).IsChecked()
		sum := form.GetFormItemByLabel(widgets.FieldSyncChecksum).(*tview.Checkbox).IsChecked()
		excl := parseExcludes(form.GetFormItemByLabel(widgets.FieldSyncExclude).(*tview.InputField).GetText())
		// A pattern that does not compile must stop the run, not quietly
		// match nothing: an exclude is often what keeps a delete-extraneous
		// sync away from a path, and a typo in it used to put that path in
		// the delete plan.
		for _, p := range excl {
			if _, err := newMatcher(p); err != nil {
				return syncSpec{}, fmt.Errorf("exclude pattern %q: %w", p, err)
			}
		}

		spec := syncSpec{dir: syncDirection(dirIdx), del: del, localDir: localDir, checksum: sum, exclude: excl, trash: c.trashPrefix()}
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
		lay.Close()
		if err != nil {
			c.error("Sync", err)
			return
		}
		c.previewSync(spec)
	})
	form.AddButton("Browse…", func() {
		// The picker is pushed as its own layer over this form; closing it —
		// chosen OR canceled — reveals the form with everything still typed
		// in. (It used to remove and conditionally re-add the form, so
		// canceling the picker discarded the whole form.)
		c.chooseDir(c.params.HomeDir, func(chosen string) {
			form.GetFormItemByLabel(widgets.FieldSyncLocalDir).(*tview.InputField).
				SetText(strings.TrimSuffix(chosen, string(os.PathSeparator)))
		})
	})
	form.AddButton("Cancel", func() { lay.Close() })

	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 82, syncFormHeight)
}

// syncFormHeight sizes the sync dialog.
const syncFormHeight = 21

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
func (c *Controller) collectSides(ctx context.Context, mdl *model.Model, spec syncSpec) (src, dst []model.SyncEntry, err error) {
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
		dst, err = mdl.ListRemoteEntries(ctx, spec.dstPrefix, spec.dstBucket)
	case syncDownload:
		if src, err = mdl.ListRemoteEntries(ctx, spec.srcPrefix, spec.srcBucket); err != nil {
			return nil, nil, err
		}
		dst, err = local()
	default: // syncRemote
		if src, err = mdl.ListRemoteEntries(ctx, spec.srcPrefix, spec.srcBucket); err != nil {
			return nil, nil, err
		}
		dst, err = mdl.ListRemoteEntries(ctx, spec.dstPrefix, spec.dstBucket)
	}
	if err != nil {
		return nil, nil, err
	}

	// Exclusions are applied before anything else looks at the entries, so
	// neither the diff nor a delete-extraneous pass can see a filtered path.
	src = applyExcludes(src, spec.exclude)
	dst = applyExcludes(dst, spec.exclude)
	switch spec.dir {
	case syncUpload:
		dst = dropTrash(dst, spec.dstPrefix, spec.trash)
	case syncDownload:
		src = dropTrash(src, spec.srcPrefix, spec.trash)
	default:
		src = dropTrash(src, spec.srcPrefix, spec.trash)
		dst = dropTrash(dst, spec.dstPrefix, spec.trash)
	}

	if spec.checksum {
		c.fillSums(ctx, mdl, spec, src, dst)
	}
	return src, dst, nil
}

// parseExcludes splits the form's comma-separated pattern list. Patterns keep
// match.go's syntax, so "node_modules" is a substring, "*.tmp" a glob and
// "re:^build/" a regex — the same three dialects the filter and the search
// use, learned once.
func parseExcludes(text string) []string {
	var out []string
	for _, p := range strings.Split(text, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// excluded reports whether a relative path matches any pattern. A path is also
// matched by a pattern naming one of its parent directories, so "node_modules"
// excludes everything under it — the behaviour anyone typing that expects,
// and the reason the test for it is explicit.
func excluded(rel string, patterns []matcher) bool {
	if len(patterns) == 0 {
		return false
	}
	segments := strings.Split(rel, "/")
	for _, m := range patterns {
		if m.match(rel) {
			return true
		}
		for i, seg := range segments {
			if m.match(seg) {
				return true
			}
			// Also try the directory prefix, so "re:^build/" works.
			if i > 0 && m.match(strings.Join(segments[:i+1], "/")+"/") {
				return true
			}
		}
	}
	return false
}

// applyExcludes filters one side of the diff. Filtering *before* the diff is
// what makes the exclusions safe: an excluded path is invisible to the
// planner, so a delete-extraneous run cannot decide it is extraneous.
func applyExcludes(entries []model.SyncEntry, patterns []string) []model.SyncEntry {
	if len(patterns) == 0 {
		return entries
	}
	ms := make([]matcher, 0, len(patterns))
	for _, p := range patterns {
		ms = append(ms, mustMatcher(p))
	}
	out := make([]model.SyncEntry, 0, len(entries))
	for _, e := range entries {
		if excluded(e.Rel, ms) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// withoutMtimes returns entries with their timestamps cleared, which makes
// planSync compare by name and size alone.
func withoutMtimes(entries []model.SyncEntry) []model.SyncEntry {
	out := make([]model.SyncEntry, len(entries))
	for i, e := range entries {
		e.Mod = time.Time{}
		out[i] = e
	}
	return out
}

// dropTrash removes the remote entries that live under the trash prefix.
// prefix is the side's sync prefix: entries are relative to it, the trash
// prefix to the bucket root.
func dropTrash(entries []model.SyncEntry, prefix, trash string) []model.SyncEntry {
	if trash == "" {
		return entries
	}
	out := entries[:0:0]
	for _, e := range entries {
		if strings.HasPrefix(prefix+e.Rel, trash) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// commonRels returns the paths present on both sides — the only ones a
// checksum comparison can say anything about. A path missing at the
// destination is already a create; hashing it would be work for no decision.
func commonRels(src, dst []model.SyncEntry) []string {
	in := make(map[string]bool, len(dst))
	for _, e := range dst {
		in[e.Rel] = true
	}
	var out []string
	for _, e := range src {
		if in[e.Rel] {
			out = append(out, e.Rel)
		}
	}
	sort.Strings(out)
	return out
}

// fillSums resolves content checksums for the paths present on both sides and
// writes them into the entry slices in place.
//
// Remote sums come from the object's stored checksum (or its ETag when the
// object is single-part); local ones are computed with whatever algorithm the
// remote side reported, because two sums are only comparable when the
// algorithms match. Anything that cannot be resolved is simply left empty and
// falls back to the size+mtime rule for that one path.
func (c *Controller) fillSums(ctx context.Context, mdl *model.Model, spec syncSpec, src, dst []model.SyncEntry) {
	rels := commonRels(src, dst)
	if len(rels) == 0 {
		return
	}

	remote := func(bucket *model.Object, prefix string) map[string]string {
		keys := make([]string, 0, len(rels))
		for _, rel := range rels {
			keys = append(keys, prefix+rel)
		}
		sums, _ := mdl.RemoteChecksums(ctx, bucket, keys)
		out := make(map[string]string, len(sums))
		for _, rel := range rels {
			if v, ok := sums[prefix+rel]; ok {
				out[rel] = v
			}
		}
		return out
	}

	localFor := func(want map[string]string) map[string]string {
		out := make(map[string]string, len(want))
		for _, rel := range rels {
			algo, _ := model.SumAlgo(want[rel])
			if algo == "" {
				continue // nothing comparable on the other side
			}
			if ctx.Err() != nil {
				return out
			}
			path := filepath.Join(spec.localDir, filepath.FromSlash(rel))
			v, err := model.LocalChecksum(path, algo)
			if err != nil {
				continue
			}
			out[rel] = algo + ":" + v
		}
		return out
	}

	var srcSums, dstSums map[string]string
	switch spec.dir {
	case syncUpload:
		dstSums = remote(spec.dstBucket, spec.dstPrefix)
		srcSums = localFor(dstSums)
	case syncDownload:
		srcSums = remote(spec.srcBucket, spec.srcPrefix)
		dstSums = localFor(srcSums)
	default: // syncRemote
		srcSums = remote(spec.srcBucket, spec.srcPrefix)
		dstSums = remote(spec.dstBucket, spec.dstPrefix)
	}

	for i := range src {
		if v, ok := srcSums[src[i].Rel]; ok {
			src[i].Sum = v
		}
	}
	for i := range dst {
		if v, ok := dstSums[dst[i].Rel]; ok {
			dst[i].Sum = v
		}
	}
}

// previewSync scans both sides off the UI goroutine and shows the plan with an
// Apply button. Scanning is a paginated listing (or a filesystem walk), so it
// runs behind a "Scanning…" modal.
func (c *Controller) previewSync(spec syncSpec) {
	mdl := c.model
	wait, _, ctx, cancel := c.cancellableWait("Scanning both sides...")

	go func() {
		defer cancel()
		src, dst, err := c.collectSides(ctx, mdl, spec)
		if err != nil {
			c.shell.Do(wait.Close)
			if ctx.Err() == nil {
				c.error("Sync scan failed", err)
			}
			return
		}

		ops := planSync(src, dst, spec.del)
		text := syncPlanText(spec.dir, spec.srcLabel(), spec.dstLabel(), ops, syncPlanRows)

		c.shell.Do(func() {
			wait.Close()
			c.showPlan(" Sync plan (dry run) ", text, func() { c.runSync(spec, ops) }, len(ops) > 0)
		})
	}()
}

// showPlan displays a plan in a scrollable modal. onApply is offered only when
// applicable is true, which is what makes the same widget serve both the sync
// preview and the read-only pane comparison.
func (c *Controller) showPlan(title, text string, onApply func(), applicable bool) {
	// lay is assigned at the bottom; every handler below runs after that.
	var lay *shell.Layer
	tv := tview.NewTextView().SetText(text).SetScrollable(true)
	tv.SetBorder(true).SetTitle(title)

	buttons := tview.NewForm()
	if applicable && onApply != nil {
		buttons.AddButton("Apply", func() {
			lay.Close()
			onApply()
		})
	}
	buttons.AddButton("Close", func() { lay.Close() })

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tv, 0, 1, false).
		AddItem(buttons, 3, 0, true)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			lay.Close()
			return nil
		}
		return event
	})
	lay = c.pushModal(flex, 86, 28)
}

// runSync hands a reviewed plan to the shared runner as an ops.Sync.
//
// The orchestration — the writes-then-deletes barrier, the worker pool,
// cancellation draining, the failure ledger and the retry plan — is ops.Sync,
// which knows nothing about the three sync directions or the terminal. What
// stays here is the part that genuinely needs both: turning plan entry i into
// an upload, a download or a server-side copy.
func (c *Controller) runSync(spec syncSpec, plan []syncOp) {
	// A remote→local sync only writes to the filesystem, so a read-only
	// profile may still run it; the other two directions write objects.
	if spec.dir != syncDownload && c.readOnlyBlocked("sync to remote storage") {
		return
	}
	// Runs on the UI goroutine: capture the client before spawning workers so
	// a profile switch can't retarget a queued/backgrounded sync.
	mdl := c.model
	st := summarizeSync(plan)

	c.runner().Start(&ops.Sync{
		Title:      fmt.Sprintf("%s  %s → %s", spec.dir, spec.srcLabel(), spec.dstLabel()),
		Units:      syncUnits(plan),
		TotalBytes: st.Bytes,
		// A remote→local sync changes nothing the browser is showing.
		WritesRemote: spec.dir != syncDownload,
		Gate:         c.queue,
		Apply: func(ctx context.Context, i int, onProgress func(written int64)) error {
			return c.applySyncOp(ctx, mdl, spec, plan[i], onProgress)
		},
	})
}

// syncUnits projects a plan onto what the runner needs: a label, a size, and
// whether the entry removes something (deletes run only after every write).
func syncUnits(plan []syncOp) []ops.SyncUnit {
	out := make([]ops.SyncUnit, 0, len(plan))
	for _, op := range plan {
		out = append(out, ops.SyncUnit{
			Label:  fmt.Sprintf("%s %s", op.Kind, op.Rel),
			Bytes:  op.Bytes,
			Delete: op.Kind == syncDelete,
		})
	}
	return out
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
		// No MkdirAll here: DownloadTarget creates the parent directories
		// itself, after SafeLocalPath has checked the key stays inside
		// localDir. Doing it first created directories outside localDir for
		// a hostile "../" key before the write was refused.
		//
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
		// The plan carries the source's size, so the copy needs no HEAD to
		// decide between the single-request and multipart paths.
		return mdl.CopyObjectSized(ctx, spec.srcBucket, spec.dstBucket, spec.srcPrefix+op.Rel, dstKey, op.Bytes)
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
		c.error("Compare", fmt.Errorf("compare needs dual-pane mode (Ctrl+O)"))
		return
	}
	// One read of each side, so the two halves of the comparison cannot come
	// from different moments.
	here, there := c.pane().Loc(), c.other().Loc()
	if here.Bucket == nil || there.Bucket == nil {
		c.error("Compare", fmt.Errorf("open a bucket in both panes first"))
		return
	}

	left := syncSpec{dir: syncRemote,
		srcBucket: here.Bucket, srcPrefix: model.NormalizePrefix(here.Path),
		dstBucket: there.Bucket, dstPrefix: model.NormalizePrefix(there.Path)}

	wait, _, ctx, cancel := c.cancellableWait("Comparing both panes...")

	mdl := c.model
	go func() {
		defer cancel()
		src, dst, err := c.collectSides(ctx, mdl, left)
		if err != nil {
			c.shell.Do(wait.Close)
			if ctx.Err() == nil {
				c.error("Compare failed", err)
			}
			return
		}
		// del=true so entries present only on the right are reported too; a
		// comparison must be symmetric even though the planner is directional.
		// For the same reason the timestamps are dropped: the planner flags a
		// same-size file only when the SOURCE is newer, so a newer copy on the
		// right compared as identical. The report promises names and sizes.
		ops := planSync(withoutMtimes(src), withoutMtimes(dst), true)
		text := comparePlanText(left.srcLabel(), left.dstLabel(), ops, syncPlanRows)

		c.shell.Do(func() {
			wait.Close()
			c.showPlan(" Pane comparison ", text, nil, false)
		})
	}()
}
