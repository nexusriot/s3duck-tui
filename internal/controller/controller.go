package controller

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/jobs"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/flows"
	"github.com/nexusriot/s3duck-tui/internal/ui/screens"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
	u "github.com/nexusriot/s3duck-tui/internal/utils"
)

type sortKey int

const (
	sortName sortKey = iota
	sortSize
	sortDate
)

func (s sortKey) String() string {
	switch s {
	case sortSize:
		return "size"
	case sortDate:
		return "date"
	default:
		return "name"
	}
}

// next cycles name → size → date → name.
func (s sortKey) next() sortKey {
	if s >= sortDate {
		return sortName
	}
	return s + 1
}

func (c *Controller) selectedCount() int { return c.pane().SelectedCount() }

type Controller struct {
	view *widgets.View
	// shell owns every overlay and every UI update made off the UI
	// goroutine. See pkg/ui/shell: Do is safe from anywhere, so no caller
	// has to know which goroutine it is on.
	shell  *shell.Shell
	model  *model.Model
	params *cfg.Params
	// activeConfig is the profile currently being browsed (nil on the
	// profiles screen). Used to resolve the per-profile download directory.
	activeConfig *cfg.Config

	// panes holds both browser panes and active says which one has focus.
	// Unlike the old arrangement — the live pane in the controller's own
	// fields, the other snapshotted into an array — a pane is addressed, not
	// mirrored, so a swap is one XOR and a navigation goroutine that captured
	// its pane cannot be redirected into the other one mid-flight.
	panes  [2]*session.Pane
	active int
	dual   bool
	// pane1Init guards the one-time seeding of pane 1.
	pane1Init bool

	// sortBy / sortDesc order the listing. Shared by both panes (unlike the
	// filter, an ordering preference is not location-specific) and guarded by
	// mu because renderList reads them from background goroutines.
	sortBy   sortKey
	sortDesc bool

	// filterSuppress stops the filter field's change handler from re-rendering
	// while we reset the field programmatically (navigation, reveal). Only
	// touched on the UI goroutine, where SetText fires the handler inline.
	filterSuppress bool

	// cols is the column layout resolved from the pane width on the last
	// render. Only touched inside renderList's shell.Do closure and by
	// labelForList, which that closure calls — so it stays on the UI goroutine
	// and needs no mutex.
	cols listColumns
	// lastWidth is the active list's width at the last render, used to detect
	// that a resize or a layout change invalidated the column layout. UI
	// goroutine only (set from the after-draw hook and from renderList).
	lastWidth int
	// browsing is true while the browser screen owns the shared list widget
	// and false on the profiles screen. renderList and the resize hook check
	// it so a stale render can never clobber the profiles list — the two
	// screens share one tview.List.
	browsing bool

	// keys is the resolved keymap (defaults plus ~/.config/s3duck-tui/keys.json).
	keys keymap
	// leaderArmed is true between the leader key and the key it prefixes. UI
	// goroutine only, like every other input-handler field.
	leaderArmed bool

	// listing tracks the in-flight navigation listing so Esc can cancel it.
	listing listingState

	// activity is a capped, in-session log of operations shown via the palette.
	activity   []activityEntry
	activityMu sync.Mutex
	// auditPath is the persistent operation log. Empty disables it.
	auditPath string

	// clip is the object clipboard (yank/cut → paste).
	clip clipboard

	// lastUndo is the one-step undo of the last move/rename (reverse ops, ready
	// to apply). Guarded by undoMu since it is written from transfer goroutines.
	lastUndo *undoOp
	undoMu   sync.Mutex

	// run drives ops.Operations: confirmation, job, progress modal, summary.
	// Built lazily by runner() because its callbacks close over the
	// controller.
	run *flows.Runner

	// queue is the background transfer queue: the jobs shown in the transfers
	// panel, the header notice, and the gate capping concurrent byte-transfer
	// phases (aggregate bandwidth is separately capped by model.Limiter).
	// It owns its own locking.
	queue *jobs.Queue
	// transfers is the background-transfer panel (internal/ui/screens). It
	// owns its own widgets for as long as it is open.
	transfers *screens.Transfers

	// mu guards sortBy/sortDesc, the only listing state left on the controller
	// now that each pane owns its own (the panes lock themselves). Read on the
	// UI goroutine, written from background refresh goroutines.
	mu sync.Mutex
	// refreshMu serializes updateList so overlapping refreshes can't stack
	// network calls or race on the object map.
	refreshMu sync.Mutex
}

// activityEntry is one line in the in-session operation log.
type activityEntry struct {
	when time.Time
	msg  string
}

// appendCapped appends e to list, keeping at most max entries (oldest dropped).
func appendCapped(list []activityEntry, e activityEntry, max int) []activityEntry {
	list = append(list, e)
	if max > 0 && len(list) > max {
		list = list[len(list)-max:]
	}
	return list
}

func NewController() *Controller {

	v := widgets.NewView()
	params := cfg.NewParams()

	c := &Controller{
		view:   v,
		model:  nil,
		params: params,
		panes:  [2]*session.Pane{session.New(), session.New()},
	}
	c.shell = shell.New(v.App, v.Pages)
	c.shell.SetBaseFocus(func() tview.Primitive { return c.view.List })
	// At most 2 concurrent byte transfers. The queue logs finished jobs into
	// the activity log and rings the bell for backgrounded outcomes.
	c.queue = jobs.NewQueue(2, c.logActivity, func() { c.shell.Do(c.view.Beep) })
	c.transfers = screens.NewTransfers(c.shell, c.queue, c.pushModal)
	c.keys = loadKeymap(KeysPath(params.HomeDir))
	c.auditPath = AuditPath(params.HomeDir)
	c.wireFilter(v.PaneFilter(0))
	c.wireFilter(v.PaneFilter(1))
	return c
}

// runner returns the shared operation driver, building it on first use. Every
// long-running job goes through it, so the confirmation, the job registration,
// the Background/Cancel modal, the progress throttle and the summary exist in
// exactly one place.
func (c *Controller) runner() *flows.Runner {
	if c.run != nil {
		return c.run
	}
	c.run = &flows.Runner{
		Shell:        c.shell,
		Queue:        c.queue,
		Confirm:      c.confirmThen,
		AskOverwrite: c.askOverwrite,
		Log:          c.logActivity,
		Done:         func() { go c.updateList() },
		CopyText:     u.CopyToClipboard,
		ExportFailures: func(name string, failures []ops.Failure) (string, error) {
			res := newOpResult(name)
			for _, f := range failures {
				res.fail(retryItem{label: f.Label}, f.Err)
			}
			return res.writeFailureReport(c.resolveDownloadDir(), time.Now())
		},
	}
	return c.run
}

// confirmThen asks a yes/no question and runs onYes if the answer is yes. It is
// the runner's pre-flight confirmation; the runner owns no widgets of its own.
func (c *Controller) confirmThen(text string, onYes func()) {
	var lay *shell.Layer
	confirm := c.view.NewConfirm()
	confirm.SetText(text).SetDoneFunc(func(_ int, label string) {
		lay.Close()
		if label == widgets.ConfirmOK {
			onYes()
		}
	})
	lay = c.pushRaw(confirm)
}

// pane is the pane with focus. Every per-location read and write goes through
// it; a flow that must survive a Tab captures the *session.Pane once at entry
// instead of calling this again later.
func (c *Controller) pane() *session.Pane { return c.panes[c.active] }

// other is the pane without focus.
func (c *Controller) other() *session.Pane { return c.panes[c.active^1] }

// wireFilter installs a filter box's live-update and key handlers. Called once
// per pane at startup. The handlers act on the active pane via the c.view.*
// fields, so both panes' boxes share the same logic. Typing narrows the current
// listing live; Enter keeps the filter and returns to the list, Esc clears it.
func (c *Controller) wireFilter(filter *tview.InputField) {
	filter.SetChangedFunc(func(text string) {
		if c.filterSuppress {
			return
		}
		c.pane().SetFilter(text)
		// Still a goroutine, though no longer to avoid a deadlock: renderList
		// reads the pane and builds the rows before it queues the draw, and
		// this handler runs inside tview's event loop. Doing that work off the
		// loop keeps typing in the filter responsive on a large listing.
		go c.renderList()
	})
	filter.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEnter:
			c.view.App.SetFocus(c.view.List)
			return nil
		case tcell.KeyEsc:
			c.clearFilterUI()
			c.view.App.SetFocus(c.view.List)
			return nil
		}
		return event
	})
}

// clearFilterUI resets the active filter and empties the filter box without
// triggering a re-render (callers that clear the filter re-render themselves,
// e.g. via navigation).
func (c *Controller) clearFilterUI() {
	c.pane().SetFilter("")
	c.filterSuppress = true
	c.view.Filter.SetText("")
	c.filterSuppress = false
}

// focusFilter moves keyboard focus to the filter box.
func (c *Controller) focusFilter() {
	c.view.App.SetFocus(c.view.Filter)
}

// askOverwrite puts the per-file overwrite question on screen and blocks on
// the answer. It is what an ops.Operation's Reporter.Ask resolves to: the
// operation hands over a value and waits, so it never holds a modal handle.
//
// Called from an operation's goroutine, never the UI one — it would wait on
// itself.
func (c *Controller) askOverwrite(path string) ops.Answer {
	ch := make(chan ops.Answer, 1)

	c.shell.Do(func() {
		var lay *shell.Layer
		m := tview.NewModal().
			SetText(fmt.Sprintf("File already exists:\n%s\n\nWhat do you want to do?", path)).
			AddButtons([]string{"Overwrite", "Skip", "Overwrite All", "Skip All", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				lay.Close()
				switch label {
				case "Overwrite":
					ch <- ops.AnswerYes
				case "Skip":
					ch <- ops.AnswerNo
				case "Overwrite All":
					ch <- ops.AnswerYesToAll
				case "Skip All":
					ch <- ops.AnswerNoToAll
				default:
					ch <- ops.AnswerCancel
				}
			})

		lay = c.pushModal(m, 80, 10)
		c.view.App.SetFocus(m)
	})

	return <-ch
}

// objKey is the unique identity used as the list's secondary text, the pane's object
// map key, and the selection-set key. It is the full S3 key (FullPath) for
// files/folders, or the bucket name. Using the full key instead of the short
// display name avoids collisions between, e.g., a file "x" and a folder "x/"
// living under the same prefix.
func objKey(o *model.Object) string {
	if o == nil || o.Key == nil {
		return ""
	}
	if o.Ot == model.Bucket || o.FullPath == nil {
		return *o.Key
	}
	return *o.FullPath
}

func (c *Controller) getSelectedObjectName() string {
	// GetCurrentItem returns 0 even for an empty list, and GetItemText indexes
	// unchecked — an empty buckets screen (no buckets, or no list permission)
	// would panic. Callers all tolerate "".
	if c.view.List.GetItemCount() == 0 {
		return ""
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	return strings.TrimSpace(cur)
}

// deleteTarget is one resolved delete: its display name (the objKey used for
// selection bookkeeping), the S3 key to remove, and the scan result. For a
// folder, key is the prefix and objects/bytes are what the recursive listing
// found; for a file, objects is 1 and bytes its size.
type deleteTarget struct {
	name     string
	key      string
	isBucket bool
	isFolder bool
	objects  int
	bytes    int64
	scanErr  error
}

// deleteConfirmText renders the confirmation for a delete of targets. It names
// each target when there are few, and always states the total object count and
// byte size so a recursive folder delete can't be confirmed blind.
func deleteConfirmText(targets []deleteTarget) string {
	var objects int
	var bytes int64
	var scanFailed int
	for _, t := range targets {
		objects += t.objects
		bytes += t.bytes
		if t.scanErr != nil {
			scanFailed++
		}
	}

	var b strings.Builder
	if len(targets) == 1 {
		fmt.Fprintf(&b, "Delete %s?\n\n", targets[0].key)
	} else {
		fmt.Fprintf(&b, "Delete %d item(s)?\n\n", len(targets))
		const shown = 6
		for i, t := range targets {
			if i == shown {
				fmt.Fprintf(&b, "  ...and %d more\n", len(targets)-shown)
				break
			}
			fmt.Fprintf(&b, "  - %s\n", t.key)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "This removes %d object(s), %s.", objects, humanize.IBytes(uint64(bytes)))
	if scanFailed > 0 {
		fmt.Fprintf(&b, "\n\n[red]%d item(s) could not be sized; the totals above are incomplete.", scanFailed)
	}
	return b.String()
}

// Delete removes the marked objects — or the highlighted one when nothing is
// marked — after a confirmation that states how many objects and bytes will go.
// On the buckets screen it deletes the highlighted bucket (buckets can't be
// marked). Sizing a folder needs a recursive listing, so the scan runs off the
// UI goroutine behind a "Calculating…" modal.
func (c *Controller) Delete() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	if c.remoteOnly("Delete") {
		return
	}
	if c.readOnlyBlocked("delete objects") {
		return
	}

	names := c.selectedNames()
	if len(names) == 0 {
		n := c.getSelectedObjectName()
		if n == "" || n == ".." {
			return
		}
		names = []string{n}
	}

	var targets []deleteTarget
	for _, n := range names {
		o, ok := c.pane().Lookup(n)
		if !ok {
			continue
		}
		switch o.Ot {
		case model.Bucket:
			targets = append(targets, deleteTarget{name: n, key: *o.Key, isBucket: true})
		case model.Folder:
			targets = append(targets, deleteTarget{name: n, key: *o.FullPath, isFolder: true})
		case model.File:
			t := deleteTarget{name: n, key: *o.FullPath, objects: 1}
			if o.Size != nil {
				t.bytes = *o.Size
			}
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return
	}

	// The scan is a recursive listing per folder target, which on a large
	// prefix is the slowest part of the whole flow — so it is cancellable,
	// and cancelling it abandons the delete rather than confirming against
	// half-counted totals.
	scanWait, _, scanCtx, scanCancel := c.cancellableWait("Calculating delete size...")

	bucket := c.pane().Bucket()
	mdl := c.model
	go func() {
		// The cancel belongs to the scan, not to Delete: deferring it in the
		// caller tore the context down the moment Delete returned — which is
		// immediately — so every folder scan failed with context.Canceled and
		// the confirmation always claimed the totals were incomplete.
		defer scanCancel()
		for i := range targets {
			if !targets[i].isFolder && !targets[i].isBucket {
				continue
			}
			key := targets[i].key
			if targets[i].isBucket {
				key = ""
			}
			scanBucket := bucket
			if targets[i].isBucket {
				b := targets[i].key
				scanBucket = &model.Object{Key: &b, Ot: model.Bucket}
			}
			objs, err := mdl.ListObjects(scanCtx, key, scanBucket)
			if err != nil {
				targets[i].scanErr = err
				continue
			}
			for _, o := range objs {
				targets[i].objects++
				targets[i].bytes += o.Size
			}
		}

		// Cancelling the sizing abandons the delete: a confirmation built on
		// half-counted totals is not one the user can answer.
		if scanCtx.Err() != nil {
			c.shell.Do(scanWait.Close)
			return
		}

		c.shell.Do(func() {
			scanWait.Close()
			var confirmLay *shell.Layer
			confirm := c.view.NewConfirm()
			confirm.SetText(deleteConfirmText(targets)).
				SetDoneFunc(func(_ int, buttonLabel string) {
					confirmLay.Close()
					if buttonLabel != widgets.ConfirmOK {
						return
					}
					c.runDelete(targets, bucket)
				})
			confirmLay = c.pushRaw(confirm)
		})
	}()
}

// runDelete executes the confirmed deletes sequentially behind a progress
// modal, then refreshes the listing and reports any failures.
func (c *Controller) runDelete(targets []deleteTarget, bucket *model.Object) {
	// Capture the pane alongside the bucket: a Tab mid-delete must not make
	// the selection bookkeeping land in the other pane.
	pane := c.pane()
	// A folder delete lists recursively before it removes anything, so a
	// delete over a big prefix is long enough to want out of. Cancelling
	// stops before the next target: the ones already removed are reported,
	// not silently forgotten.
	lay, progress, ctx, cancel := c.cancellableWait("Deleting...")

	mdl := c.model
	trash := c.trashPrefix()
	go func() {
		defer cancel()
		res := newOpResult("Delete")
		canceled := false

		for i, t := range targets {
			if ctx.Err() != nil {
				canceled = true
				break
			}
			i, t := i, t
			c.shell.Do(func() {
				progress.SetText(fmt.Sprintf("Deleting\n%d/%d\n%s", i+1, len(targets), t.key))
			})

			var err error
			switch {
			case t.isBucket:
				// The confirm promised the bucket's objects would go, and S3
				// only deletes empty buckets — so empty it first.
				name := t.key
				b := &model.Object{Key: &name, Ot: model.Bucket}
				if err = mdl.EmptyBucket(ctx, b); err == nil {
					err = mdl.DeleteBucket(&name)
				}
			case trash != "":
				// Safe delete: move the object (or the whole prefix) under the
				// profile's trash prefix instead of removing it.
				err = c.trashTarget(ctx, mdl, bucket, t, trash)
			default:
				key := t.key
				err = mdl.Delete(ctx, &key, bucket)
			}
			if err != nil {
				res.fail(retryItem{label: t.key, target: t}, err)
				continue
			}
			res.ok()

			// Drop the deleted item from the selection set so the
			// "Selected: N" count and markers stay accurate.
			pane.Unselect(t.name)
		}

		verb := "Delete"
		if trash != "" {
			verb = "Trash"
		}
		c.logActivity("%s: %d ok, %d failed", verb, res.okCount, len(res.failures))
		c.reportOpResult(lay, progress, res, canceled, func(items []retryItem) {
			retry := make([]deleteTarget, 0, len(items))
			for _, it := range items {
				if t, ok := it.target.(deleteTarget); ok {
					retry = append(retry, t)
				}
			}
			if len(retry) > 0 {
				c.runDelete(retry, bucket)
			}
		})

		c.updateList()
		c.clearDetailsIfNoSelection()
	}()
}

// resolveDownloadDir returns the destination directory for downloads,
// always terminated with a path separator. It uses the active profile's
// DownloadDir, expands a leading "~" to the user's home, and falls back to
// ~/Downloads when unset.
func (c *Controller) resolveDownloadDir() string {
	dir := ""
	if c.activeConfig != nil {
		dir = strings.TrimSpace(c.activeConfig.DownloadDir)
	}
	if dir == "" {
		dir = filepath.Join(c.params.HomeDir, "Downloads")
	} else if dir == "~" {
		dir = c.params.HomeDir
	} else if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(c.params.HomeDir, dir[2:])
	}
	if !strings.HasSuffix(dir, string(os.PathSeparator)) {
		dir += string(os.PathSeparator)
	}
	return dir
}

func (c *Controller) Download() {
	if c.remoteOnly("Download") {
		return
	}
	if c.view.List.GetItemCount() == 0 || c.pane().Bucket() == nil {
		return
	}

	// Capture the source location AND the client now, on the UI goroutine.
	// The byte phase can run long after Background is pressed — reading
	// c.pane().Bucket() there would panic once the user backs out to the
	// buckets screen, a pane swap would silently retarget the remaining
	// objects at the other pane's bucket, and a profile switch would swap
	// c.model under a queued transfer, pointing it at the wrong endpoint.
	srcBucket := c.pane().Bucket()
	srcPath := c.pane().Path()
	mdl := c.model

	names := c.selectedNames()
	if len(names) == 0 {
		// current item only
		cur := c.getSelectedObjectName()
		names = []string{cur}
	}
	var sel []*model.Object
	for _, name := range names {
		val, ok := c.pane().Lookup(name)
		if !ok || (val.Ot != model.File && val.Ot != model.Folder) {
			continue
		}
		sel = append(sel, val)
	}
	if len(sel) == 0 {
		return
	}

	// Folder resolution pages through the whole prefix over the network.
	// Behind a modal, off the UI goroutine — inline it used to freeze the
	// entire TUI for the length of the listing, and swallow its errors.
	resolveWait, _, resolveCtx, resolveCancel := c.cancellableWait("Resolving objects...")

	go func() {
		defer resolveCancel()
		var allObjects []model.DownloadTarget
		var totalSize int64
		for _, val := range sel {
			key := *val.FullPath
			objs, size, err := mdl.ResolveDownloadObjects(
				resolveCtx,
				key,
				val.Ot == model.Folder,
				val.Size,
				srcBucket,
			)
			if err != nil {
				c.shell.Do(resolveWait.Close)
				// A cancel is the user's own decision, not an error to report.
				if resolveCtx.Err() == nil {
					c.error("Download: resolving objects failed", err)
				}
				return
			}
			allObjects = append(allObjects, objs...)
			totalSize += size
		}

		c.shell.Do(func() {
			resolveWait.Close()
			if len(allObjects) == 0 {
				return
			}
			proceed := func(cwd string) {
				c.runDownload(mdl, srcBucket, srcPath, len(names), allObjects, totalSize, cwd)
			}
			switch {
			case c.otherPaneLocalDir() != "":
				// A local pane is open: land the files where the user is
				// looking rather than in the profile's download directory.
				proceed(c.otherPaneLocalDir() + string(os.PathSeparator))
			case c.activeConfig != nil && strings.TrimSpace(c.activeConfig.DownloadDir) != "":
				proceed(c.resolveDownloadDir())
			default:
				c.chooseDir(c.params.HomeDir, proceed)
			}
		})
	}()
}

// runDownload confirms and executes the byte phase for already-resolved
// objects. Runs on the UI goroutine.
//
// The pipeline itself — two-phase resolve, the overwrite decisions, the
// four-worker pool, cancellation draining, verification, the retry set — is
// ops.Download, which has no idea a terminal exists and is unit-tested against
// a fake store. This is only the wiring.
func (c *Controller) runDownload(mdl *model.Model, srcBucket *model.Object, srcPath string, selectedCount int, allObjects []model.DownloadTarget, totalSize int64, cwd string) {
	c.runner().Start(&ops.Download{
		Bucket:        srcBucket,
		SrcPrefix:     srcPath,
		DestDir:       cwd,
		Targets:       allObjects,
		SelectedCount: selectedCount,
		TotalBytes:    totalSize,
		// Read on the UI goroutine and carried into the transfer: a profile
		// switch mid-download must not change the rules a running transfer is
		// playing by.
		Verify:  c.verifyDownloads(),
		Fetcher: mdl,
		Checker: mdl,
		FS:      ops.OSFS{},
		Gate:    c.queue,
	})
}

func (c *Controller) labelForList(o *model.Object) (primary string, secondary string) {
	if o == nil || o.Key == nil {
		return "", ""
	}
	key := objKey(o)

	selected := false
	if c.pane().Bucket() != nil && (o.Ot == model.File || o.Ot == model.Folder) {
		selected = c.pane().IsSelected(key)
	}

	return listRow(o, selected, c.cols), key
}

func (c *Controller) ToggleSelectCurrent() {
	if c.pane().Bucket() == nil || c.view.List.GetItemCount() == 0 {
		return
	}

	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	cur = strings.TrimSpace(cur)

	// ignore special entry
	if cur == "" || cur == ".." {
		return
	}

	obj, ok := c.pane().Lookup(cur)
	if !ok || (obj.Ot != model.File && obj.Ot != model.Folder) {
		return
	}

	c.pane().ToggleSelected(cur)

	// re-render from the cached object map (no network); keeps the selection
	// marker and the "Selected: N" title accurate.
	go c.renderList()
}

// updateList fetches the current bucket/prefix from S3 (network I/O) and then
// re-renders the list. Use renderList directly when the object set is unchanged
// (filtering, selection toggles) to avoid a redundant round-trip.
func (c *Controller) updateList() error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if c.localMode() {
		// A local listing is one ReadDir: no pagination, no cancellation to
		// arrange, nothing to stream.
		if err := c.makeLocalObjectMap(); err != nil {
			c.error("Cannot read the directory", err)
			return err
		}
		c.pane().SetPartial(false)
		c.renderList()
		return nil
	}

	ctx, gen := c.listing.begin()
	defer c.listing.end(gen)

	// The pane and title base are captured before the fetch: a Tab during a
	// slow listing must not redirect the progress counter into the other pane.
	target, base := c.view.List, c.listTitleBase()
	partial, err := c.makeObjectMap(ctx, c.listProgressReporter(target, base))
	if err != nil {
		c.error("Failed to fetch folder", err)
		return err
	}
	c.pane().SetPartial(partial)
	c.renderList()
	return nil
}

// renderList repopulates the list widget from the in-memory object map,
// applying the active filter and sort and resolving the column layout for the
// pane's current width. It performs no network I/O, so it is cheap enough to
// call on every keystroke (live filter) or selection toggle. All widget access is marshalled onto the UI goroutine,
// which also fixes the previous race of reading list state off-goroutine.
func (c *Controller) renderList() {
	title, fText := c.listChrome()
	sk, sd := c.getSort()
	objs := filterSortObjects(c.pane().Snapshot(), c.pane().Filter(), sk, sd)

	c.shell.Do(func() {
		// A refresh that lands after the user returned to the profiles screen
		// must not rebuild the (shared) list widget from browser state.
		if !c.browsing {
			return
		}
		// Preserve the cursor across the rebuild. Reading the widget here (on
		// the UI goroutine) avoids racing tview's event loop.
		keepCur := ""
		if keepIdx := c.view.List.GetCurrentItem(); keepIdx >= 0 && c.view.List.GetItemCount() > 0 {
			_, t := c.view.List.GetItemText(keepIdx)
			keepCur = strings.TrimSpace(t)
		}
		// Read-and-clear in one step: two reads could straddle a navigation
		// that set a new target, and one listing would consume another's.
		want := keepCur
		if pending := c.pane().TakeRestoreNext(); pending != "" {
			want = pending
		}

		// Column widths follow the pane's current width, so a narrow dual-pane
		// drops columns instead of truncating names into uselessness. The rect
		// is zero before the first draw; planColumns falls back for that case.
		listX, _, listWidth, _ := c.view.List.GetInnerRect()
		cols := planColumns(listWidth)
		c.cols = cols
		c.lastWidth = listWidth

		// Indent the header to wherever the list's content actually begins,
		// rather than assuming a border width: the offset is then correct
		// whatever padding or decoration either widget grows later.
		headerX, _, _, _ := c.view.Header.GetInnerRect()
		indent := listX - headerX
		if indent < 0 {
			indent = 0
		}
		c.view.Header.SetText(strings.Repeat(" ", indent) + listHeader(cols))

		c.view.List.Clear()
		c.view.List.SetTitle(title)
		c.view.SetFrameText(fText)

		offset := 0
		if c.pane().Bucket() != nil {
			c.view.List.AddItem("[..]", "..", 0, func() { c.Up() })
			offset = 1
		}

		var keys []string
		for _, o := range objs {
			if o.Key == nil {
				continue
			}

			primary, raw := c.labelForList(o)
			c.view.List.AddItem(primary, raw, 0, func() {
				i := c.view.List.GetCurrentItem()
				_, cur := c.view.List.GetItemText(i)
				cur = strings.TrimSpace(cur)

				if cur == ".." {
					c.Up()
					return
				}

				if val, ok := c.pane().Lookup(cur); ok {
					if val.Ot == model.Folder || val.Ot == model.Bucket {
						if val.Ot == model.Bucket {
							c.pane().SetBucketPos(c.view.List.GetCurrentItem())
						}
						c.Down(cur)
					}
				}
			})

			keys = append(keys, raw)
		}

		if want != "" {
			target := -1
			if want == ".." && offset == 1 {
				target = 0
			} else {
				for i := 0; i < len(keys); i++ {
					if keys[i] == want {
						target = i + offset
						break
					}
				}
			}
			if target >= 0 && target < c.view.List.GetItemCount() {
				c.view.List.SetCurrentItem(target)
			}
		}
	})
}

// listTitleBase is the location part of the list title, without the volatile
// decorations (selection count, filter, sort, listing progress). The listing
// progress reporter needs it to rewrite the title while a fetch is running.
func (c *Controller) listTitleBase() string {
	if c.localMode() {
		return c.localTitle()
	}
	if c.pane().Bucket() == nil {
		return "(buckets)"
	}
	return fmt.Sprintf("(%s)/%s", *c.pane().Bucket().Key, c.pane().Path())
}

// listChrome builds the list title and the frame help line for the current
// screen (buckets vs objects), including the selection count and active filter.
func (c *Controller) listChrome() (title, fText string) {
	var suff string
	title = c.listTitleBase()
	if c.localMode() {
		if n := c.selectedCount(); n > 0 {
			title = fmt.Sprintf("%s  [green]Selected: %d", title, n)
		}
		if f := c.pane().Filter(); f != "" {
			m := mustMatcher(f)
			title = fmt.Sprintf("%s  [yellow]filter:%s (%s)", title, f, m.matchLabel())
		}
		return title, localBrowseHelp()
	}
	if c.pane().Bucket() != nil {
		if n := c.selectedCount(); n > 0 {
			title = fmt.Sprintf("%s  [green]Selected: %d", title, n)
		}
		suff = "[::b][Ctrl+D[][::-]Download [::b][Ctrl+U[][::-]Upload [::b][Ctrl+F][::-]Search "
	}
	title += partialNote(c.pane().Partial(), len(c.pane().Snapshot()))
	if f := c.pane().Filter(); f != "" {
		// Naming the mode is what makes the three-dialect query legible: a
		// user who typed "*.log" can see it was read as a glob, and a
		// half-finished "re:[" says so instead of just emptying the list.
		m := mustMatcher(f)
		colour := "yellow"
		if m.broken() {
			colour = "red"
		}
		title = fmt.Sprintf("%s  [%s]filter:%s (%s)", title, colour, f, m.matchLabel())
	}
	title = fmt.Sprintf("%s  [blue]%s", title, sortLabel(c.getSort()))
	fText = fmt.Sprintf("[::b][↓,↑][::-]D/U [::b][Ent/Bck][::-]L/U %s[::b][/][::-]Filter [::b][Del[][::-]Delete [::b][Ctrl+N][::-]Create [::b][Ctrl+P][::-]Profiles [::b][Ctrl+L][::-]Properties [::b][Ctrl+H][::-]Hotkeys [::b][Ctrl+Q][::-]Quit", suff)
	return title, fText
}

// sortLabel renders the active ordering for the list title, e.g. "sort:size↓".
func sortLabel(key sortKey, desc bool) string {
	arrow := "↑"
	if desc {
		arrow = "↓"
	}
	return fmt.Sprintf("sort:%s%s", key, arrow)
}

// getSort returns the active sort key and direction.
func (c *Controller) getSort() (sortKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sortBy, c.sortDesc
}

// CycleSort advances the sort key (name → size → date) and re-renders. No
// network round-trip: the ordering is applied to the in-memory object map.
func (c *Controller) CycleSort() {
	c.mu.Lock()
	c.sortBy = c.sortBy.next()
	c.mu.Unlock()
	go c.renderList()
}

// ToggleSortDir flips between ascending and descending and re-renders.
func (c *Controller) ToggleSortDir() {
	c.mu.Lock()
	c.sortDesc = !c.sortDesc
	c.mu.Unlock()
	go c.renderList()
}

// Refresh re-fetches the current listing from S3. Runs on its own goroutine so
// a slow endpoint can't block the tview event loop.
func (c *Controller) Refresh() {
	go c.updateList()
}

// filterSortObjects returns the objects whose display name (o.Key) contains
// filter (case-insensitive), ordered by the given sort key. Folders/buckets
// always come before files regardless of the key or direction, so the listing
// keeps its navigable shape. An empty filter returns every object. The input
// slice is not mutated.
func filterSortObjects(objs []*model.Object, filter string, key sortKey, desc bool) []*model.Object {
	// A broken pattern degrades to matching nothing rather than to matching
	// everything: an empty listing with "bad pattern" in the title is honest,
	// while silently ignoring the query would look like the filter had been
	// applied and found everything.
	m := mustMatcher(filter)
	out := make([]*model.Object, 0, len(objs))
	for _, o := range objs {
		if o == nil || o.Key == nil {
			continue
		}
		if !m.empty() && !m.match(*o.Key) {
			continue
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Ot != b.Ot {
			return a.Ot > b.Ot
		}
		less, tie := lessBySortKey(a, b, key)
		if tie {
			// Name is the tiebreaker and is always ascending, so equal sizes or
			// timestamps produce a stable, predictable order in both directions.
			return *a.Key < *b.Key
		}
		if desc {
			return !less
		}
		return less
	})
	return out
}

// lessBySortKey compares two objects on the active sort key. tie reports that
// the two are equal on that key (or that the key is unavailable, as for folders
// which carry no size or timestamp), in which case the caller falls back to
// name ordering.
func lessBySortKey(a, b *model.Object, key sortKey) (less, tie bool) {
	switch key {
	case sortSize:
		as, bs := int64(0), int64(0)
		if a.Size != nil {
			as = *a.Size
		}
		if b.Size != nil {
			bs = *b.Size
		}
		if as == bs {
			return false, true
		}
		return as < bs, false
	case sortDate:
		if a.LastModified == nil || b.LastModified == nil || a.LastModified.Equal(*b.LastModified) {
			return false, true
		}
		return a.LastModified.Before(*b.LastModified), false
	default:
		if *a.Key == *b.Key {
			return false, true
		}
		return *a.Key < *b.Key, false
	}
}

// findBucketByName resolves a bucket for navigation. A listing failure is
// reported rather than swallowed — silently returning nil made a transient
// error look like navigation being dead. Both callers run on background
// goroutines, so c.error is called directly.
func (c *Controller) findBucketByName(ctx context.Context, name string) *model.Object {
	list, err := c.model.ListBuckets(ctx)
	if err != nil {
		c.error("Failed to list buckets", err)
		return nil
	}
	c.pane().SetBuckets(list)
	for _, v := range c.pane().Buckets() {
		if name == *v.Key {
			return v
		}
	}
	return nil
}

// Down enters the bucket or folder identified by name (a unique objKey: the
// bucket name when on the buckets screen, otherwise the full folder prefix).
func (c *Controller) Down(name string) {
	if c.localMode() {
		c.localDown(name)
		return
	}
	c.view.Details.Clear()
	c.clearFilterUI() // a filter is scoped to one listing; reset on navigation
	c.recordHistory()

	// Capture the pane on the UI goroutine. The goroutine below belongs to
	// THIS pane; a Tab while a slow bucket opens no longer lands the result
	// in the other one.
	pane := c.pane()

	if pane.Bucket() == nil {
		// Entering a bucket triggers network calls (ListBuckets +
		// GetBucketLocation); run them off the UI goroutine so the TUI
		// doesn't freeze on a slow or unreachable endpoint.
		go func() {
			bucket := c.findBucketByName(context.Background(), name)
			if bucket == nil {
				return
			}
			if err := c.model.RefreshClient(&name); err != nil {
				c.error("Failed to resolve bucket region", err)
			}
			// Bucket and cursor land together, under the pane's lock.
			pane.Enter(bucket, pane.Path(), "..")
			c.updateList()
		}()
		return
	}

	// name is the full folder prefix (already ends with "/").
	pane.SetPath(name)
	go c.updateList()
}

func (c *Controller) Up() {
	if c.localMode() {
		c.localUp()
		return
	}
	c.view.Details.Clear()
	c.clearFilterUI() // a filter is scoped to one listing; reset on navigation
	c.recordHistory()

	pane := c.pane()
	loc := pane.Loc()

	if loc.Path == "" {
		// Leaving a bucket: restore the cursor onto it in the buckets list.
		restore := ""
		if loc.Bucket != nil {
			restore = *loc.Bucket.Key
		}
		pane.Enter(nil, "", restore)
		go c.updateList()
		return
	}

	// Restore the cursor onto the folder we're leaving (its unique objKey is
	// the full prefix, which equals the current path), and step up one level.
	//
	// parentPrefix trims exactly one trailing segment. Splitting on "/" and
	// re-joining would collapse empty segments, so leaving a folder with an
	// empty-named component ("a//") would jump two levels instead of one.
	pane.Enter(loc.Bucket, parentPrefix(loc.Path), loc.Path)
	go c.updateList()
}

func (c *Controller) Stop() {
	// Record where this profile was before the process goes away, so the
	// next run reopens here.
	c.rememberLocation()
	c.persistProfiles()
	c.logActivity("session ended")
	c.view.App.Stop()
}

func (c *Controller) create(isBucket bool) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	if c.remoteOnly("Create") {
		return
	}
	if c.readOnlyBlocked("create buckets or folders") {
		return
	}
	var oTp string
	var disableBool bool
	if isBucket {
		oTp = "bucket"
		disableBool = true
	} else {
		oTp = "folder"
		disableBool = false
	}

	cForm := c.view.NewCreateForm(fmt.Sprintf("Create %s", oTp), disableBool)
	cForm.AddButton("Save", func() {
		var err error
		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		if name == "" {
			return
		}
		// path.Join would silently retarget "." / ".." / "a/b" (".." creates
		// the PARENT prefix's marker); reject them like rename does.
		if !isBucket {
			if verr := validateEntryName(name); verr != nil {
				lay.Close()
				c.error("Create folder", verr)
				return
			}
		}

		// restore is the new object's unique objKey, so the cursor lands on it
		// after the refresh: the bucket name, or the full folder prefix.
		restore := name
		if isBucket {
			public := cForm.GetFormItem(1).(*tview.Checkbox).IsChecked()
			err = c.model.CreateBucket(&name, public)
		} else {
			key := path.Join(c.pane().Path(), name) + "/"
			err = c.model.CreateFolder(&key, c.pane().Bucket())
			restore = key
		}

		if err != nil {
			lay.Close()
			c.error("Error creating object", err)
			return
		}

		lay.Close()
		c.pane().SetRestoreNext(restore)
		go c.updateList()
	})

	cForm.AddButton("Cancel", func() {
		lay.Close()
	})

	cForm.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(cForm, 65, 9)
}

func (c *Controller) Create() {
	if c.pane().Bucket() == nil {
		c.create(true)
		return
	}
	c.create(false)
}

func (c *Controller) ClearSelection() {
	c.pane().ClearSelected()
	go c.renderList()
}

// SelectAllVisible selects every file/folder currently shown — i.e. matching
// the active filter, not the whole prefix — so "select all" respects a filter.
func (c *Controller) SelectAllVisible() {
	if c.pane().Bucket() == nil {
		return
	}
	sk, sd := c.getSort()
	var names []string
	for _, obj := range filterSortObjects(c.pane().Snapshot(), c.pane().Filter(), sk, sd) {
		if obj.Ot == model.File || obj.Ot == model.Folder {
			names = append(names, objKey(obj))
		}
	}
	c.pane().SelectNames(names)
	go c.renderList()
}

func (c *Controller) selectedNames() []string {
	sel := c.pane().SelectedSet()
	if len(sel) == 0 {
		return nil
	}
	out := make([]string, 0, len(sel))
	for name, ok := range sel {
		if ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// currentObject returns the highlighted file/folder (ignores "..", buckets).
func (c *Controller) currentObject() (string, *model.Object, bool) {
	if c.pane().Bucket() == nil || c.view.List.GetItemCount() == 0 {
		return "", nil, false
	}
	name := c.getSelectedObjectName()
	if name == "" || name == ".." {
		return "", nil, false
	}
	o, ok := c.pane().Lookup(name)
	if !ok || (o.Ot != model.File && o.Ot != model.Folder) {
		return "", nil, false
	}
	return name, o, true
}

// Rename renames within the current folder. With two or more marked items it
// opens the batch/pattern form; otherwise it renames the highlighted object.
func (c *Controller) Rename() {
	if c.remoteOnly("Rename") {
		return
	}
	if c.readOnlyBlocked("rename objects") {
		return
	}
	if c.pane().Bucket() == nil {
		return
	}
	if names := c.selectedNames(); len(names) >= 2 {
		c.batchRename(names)
		return
	}
	c.renameSingle()
}

// validateEntryName rejects names that key derivation or navigation would
// reinterpret: separators (they'd nest), and the dot-dirs ("." is the current
// prefix, ".." — after path.Join's cleaning — its parent).
func validateEntryName(name string) error {
	if strings.Contains(name, "/") {
		return fmt.Errorf("name must not contain '/'")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%q is not a usable name", name)
	}
	return nil
}

// renameSingle renames the highlighted object (server-side copy + delete;
// recursive for folders).
func (c *Controller) renameSingle() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	_, obj, ok := c.currentObject()
	if !ok {
		return
	}
	isFolder := obj.Ot == model.Folder
	short := *obj.Key       // display name shown in the form
	srcKey := *obj.FullPath // full key; already ends with "/" for folders

	form := c.view.NewInputForm("Rename", "New name", short)
	form.AddButton("Save", func() {
		newName := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		lay.Close()

		if newName == "" || newName == short {
			return
		}
		if err := validateEntryName(newName); err != nil {
			c.error("Invalid name", err)
			return
		}

		dstKey := c.pane().Path() + newName
		if isFolder {
			dstKey += "/"
		}

		bucket := c.pane().Bucket()
		mdl := c.model
		item := copyMoveItem{shortName: newName, srcKey: srcKey, isFolder: isFolder}
		c.confirmOverwrites(mdl, bucket, "Rename",
			plannedCopyKeys(mdl, bucket, bucket, []copyMoveItem{item}, c.pane().Path()),
			func(skip map[string]bool) {
				go func() {
					n, err := mdl.MoveKeys(context.Background(), bucket, bucket, srcKey, dstKey, isFolder, skip, nil)
					if err != nil {
						c.error("Rename failed", err)
						return
					}
					if n == 0 {
						c.success("Nothing renamed")
						return
					}
					c.setUndo(&undoOp{
						items: invertOps([]transferPair{{srcBucket: bucket, dstBucket: bucket, srcKey: srcKey, dstKey: dstKey, isFolder: isFolder}}),
						desc:  "rename " + short,
					})
					c.logActivity("renamed %s → %s", short, newName)
					c.pane().SetRestoreNext(dstKey) // unique objKey of the renamed item
					c.updateList()
					c.success("Renamed")
				}()
			})
	})
	form.AddButton("Cancel", func() {
		lay.Close()
	})

	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 65, 9)
}

// renameItem is one object selected for batch rename.
type renameItem struct {
	shortName string
	srcKey    string
	isFolder  bool
}

// renameOp is a resolved batch-rename operation (src → dst within one bucket).
type renameOp struct {
	srcKey   string
	dstKey   string
	label    string
	isFolder bool
}

// applyRenamePattern computes a new name from orig using pattern tokens:
//
//	{name}  original base name (without extension)
//	{ext}   extension including the dot (empty if none)
//	{n}     1-based index, zero-padded to the width of total
//
// A non-empty find is substring-replaced with replace in {name} first. index is
// 0-based.
func applyRenamePattern(orig, pattern, find, replace string, index, total int) string {
	ext := path.Ext(orig)
	base := strings.TrimSuffix(orig, ext)
	if find != "" {
		base = strings.ReplaceAll(base, find, replace)
	}
	width := len(strconv.Itoa(total))
	if width < 1 {
		width = 1
	}
	num := fmt.Sprintf("%0*d", width, index+1)
	return strings.NewReplacer("{name}", base, "{ext}", ext, "{n}", num).Replace(pattern)
}

// planBatchRename computes and validates the rename operations. It skips no-op
// renames (unchanged name) and errors on invalid names ('/' or empty),
// duplicate targets, or a target that would clobber another item's source.
func planBatchRename(items []renameItem, currentPath, pattern, find, replace string) ([]renameOp, error) {
	srcSet := make(map[string]bool, len(items))
	for _, it := range items {
		srcSet[it.srcKey] = true
	}
	dstSet := make(map[string]bool, len(items))
	var ops []renameOp
	total := len(items)
	for i, it := range items {
		newName := applyRenamePattern(it.shortName, pattern, find, replace, i, total)
		if newName == "" {
			return nil, fmt.Errorf("empty result name for %q", it.shortName)
		}
		if err := validateEntryName(newName); err != nil {
			return nil, fmt.Errorf("%v: %q", err, newName)
		}
		if newName == it.shortName {
			continue // no-op
		}
		dstKey := currentPath + newName
		if it.isFolder {
			dstKey += "/"
		}
		if dstSet[dstKey] {
			return nil, fmt.Errorf("duplicate target name %q", newName)
		}
		if srcSet[dstKey] {
			return nil, fmt.Errorf("target %q collides with another selected item", newName)
		}
		dstSet[dstKey] = true
		ops = append(ops, renameOp{
			srcKey:   it.srcKey,
			dstKey:   dstKey,
			label:    fmt.Sprintf("%s → %s", it.shortName, newName),
			isFolder: it.isFolder,
		})
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("no names changed")
	}
	return ops, nil
}

// batchRename prompts for a pattern and renames every marked file/folder.
func (c *Controller) batchRename(names []string) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	var items []renameItem
	for _, n := range names {
		o, ok := c.pane().Lookup(n)
		if !ok || (o.Ot != model.File && o.Ot != model.Folder) {
			continue
		}
		items = append(items, renameItem{shortName: *o.Key, srcKey: *o.FullPath, isFolder: o.Ot == model.Folder})
	}
	if len(items) == 0 {
		return
	}
	// Stable order so {n} numbering is predictable.
	sort.Slice(items, func(i, j int) bool { return items[i].shortName < items[j].shortName })

	prefix := c.pane().Path()
	form := c.view.NewBatchRenameForm(fmt.Sprintf("Batch rename %d items", len(items)))
	form.AddButton("Rename", func() {
		pattern := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		find := form.GetFormItem(1).(*tview.InputField).GetText()
		replace := form.GetFormItem(2).(*tview.InputField).GetText()
		lay.Close()

		ops, err := planBatchRename(items, prefix, pattern, find, replace)
		if err != nil {
			c.error("Batch rename", err)
			return
		}

		bucket := c.pane().Bucket()
		mdl := c.model
		c.confirmOverwrites(mdl, bucket, "Batch rename",
			plannedCopyKeys(mdl, bucket, bucket, renameItemsToCopyItems(ops), prefix),
			func(skip map[string]bool) {
				kept := keepUnskippedRenames(ops, skip)
				if len(kept) == 0 {
					c.success("Nothing to rename")
					return
				}
				c.runBatchRename(kept, skip)
			})
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 70, 11)
}

// renameItemsToCopyItems views rename operations as copy items, so the
// overwrite scan can expand their destinations the same way a copy's are.
// The "short name" is the destination's own last segment, since that is what
// plannedCopyKeys appends to the prefix.
func renameItemsToCopyItems(ops []renameOp) []copyMoveItem {
	items := make([]copyMoveItem, 0, len(ops))
	for _, op := range ops {
		items = append(items, copyMoveItem{
			shortName: path.Base(strings.TrimSuffix(op.dstKey, "/")),
			srcKey:    op.srcKey,
			isFolder:  op.isFolder,
		})
	}
	return items
}

// keepUnskippedRenames drops the operations whose only destination the user
// chose to keep. A folder rename survives as long as anything under it is
// still due to move — the per-object skips are applied inside MoveKeys.
func keepUnskippedRenames(ops []renameOp, skip map[string]bool) []renameOp {
	if len(skip) == 0 {
		return ops
	}
	kept := make([]renameOp, 0, len(ops))
	for _, op := range ops {
		if !op.isFolder && skip[op.dstKey] {
			continue
		}
		kept = append(kept, op)
	}
	return kept
}

// runBatchRename executes the resolved rename operations behind a cancellable
// progress modal (server-side copy + delete per item).
func (c *Controller) runBatchRename(ops []renameOp, skip map[string]bool) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	ctx, cancel := context.WithCancel(context.Background())
	progress := tview.NewModal().
		SetText("Starting rename...\n").
		AddButtons([]string{"Cancel"}).
		SetDoneFunc(func(_ int, _ string) {
			cancel()
			lay.Close()
		})
	lay = c.pushRaw(progress)

	bucket := c.pane().Bucket()
	mdl := c.model
	go func() {
		// failures keep the item descriptor, so the report can re-run exactly
		// the ones that failed instead of the whole selection.
		var failed []opFailure
		var moved []transferPair
		okCount := 0
		canceled := false

	loop:
		for i, op := range ops {
			select {
			case <-ctx.Done():
				canceled = true
				break loop
			default:
			}
			// Separate names, deliberately: `n, err :=` below would ASSIGN to an
			// `n` declared here (same scope, only err is new), so the queued
			// closure — which the UI goroutine runs later — would read the
			// object count MoveKeys returned instead of the index. A wrong
			// number on screen, and a data race on every item but the last.
			idx, label := i+1, op.label
			c.shell.Do(func() {
				progress.SetText(fmt.Sprintf("Renaming %d/%d\n%s", idx, len(ops), label))
			})
			written, err := mdl.MoveKeys(ctx, bucket, bucket, op.srcKey, op.dstKey, op.isFolder, skip, nil)
			if err != nil {
				failed = append(failed, opFailure{item: retryItem{label: op.label, target: op}, err: err})
				continue
			}
			if written == 0 {
				// Every object under this op was kept, so nothing moved.
				// Recording it would put an undo entry on the stack whose
				// destination does not exist.
				continue
			}
			okCount++
			moved = append(moved, transferPair{srcBucket: bucket, dstBucket: bucket, srcKey: op.srcKey, dstKey: op.dstKey, isFolder: op.isFolder})
		}

		if len(moved) > 0 {
			c.setUndo(&undoOp{items: invertOps(moved), desc: fmt.Sprintf("batch rename of %d item(s)", len(moved))})
			c.logActivity("batch renamed %d item(s)", len(moved))
		}

		if canceled {
			c.updateList()
			return
		}
		c.pane().ClearSelected()

		res := &opResult{name: "Rename", okCount: okCount, failures: failed}
		c.reportOpResult(lay, progress, res, false, func(items []retryItem) {
			again := make([]renameOp, 0, len(items))
			for _, it := range items {
				if op, ok := it.target.(renameOp); ok {
					again = append(again, op)
				}
			}
			if len(again) > 0 {
				c.runBatchRename(again, skip)
			}
		})
		c.updateList()
	}()
}

// copyMoveItem is one object queued for a copy/move: its display name (appended
// to the destination prefix), full source key, and folder flag.
type copyMoveItem struct {
	shortName string
	srcKey    string
	isFolder  bool
	// size is captured when the item is built, on the UI goroutine — the
	// cross-profile copy needs it for progress totals, and the listing it
	// came from may be long gone by the time the transfer runs.
	size int64
}

// clipboard holds objects yanked (copy) or cut (move) for a later paste.
type clipboard struct {
	op     string // "copy" or "cut"; "" = empty
	bucket *model.Object
	prefix string // path the items were yanked from (same-destination guard)
	scope  string // selection-scope key at yank time (marks cleared after a cut-paste)
	items  []copyMoveItem
}

// transferPair is one src→dst object move (used for undo bookkeeping).
type transferPair struct {
	srcBucket, dstBucket *model.Object
	srcKey, dstKey       string
	isFolder             bool
}

// undoOp is a one-step undo: reverse moves (already inverted) plus a label.
type undoOp struct {
	items []transferPair
	desc  string
}

// invertOps returns the reverse of each move (swap src/dst) so applying the
// result undoes the originals.
func invertOps(ops []transferPair) []transferPair {
	out := make([]transferPair, len(ops))
	for i, o := range ops {
		out[i] = transferPair{
			srcBucket: o.dstBucket, dstBucket: o.srcBucket,
			srcKey: o.dstKey, dstKey: o.srcKey,
			isFolder: o.isFolder,
		}
	}
	return out
}

func (c *Controller) setUndo(op *undoOp) {
	c.undoMu.Lock()
	c.lastUndo = op
	c.undoMu.Unlock()
}

func (c *Controller) takeUndo() *undoOp {
	c.undoMu.Lock()
	defer c.undoMu.Unlock()
	return c.lastUndo
}

// copyOrMove copies (isMove=false) or moves (isMove=true) the marked items, or
// the highlighted one when nothing is marked, to a destination bucket + prefix.
// The destination bucket defaults to the current one but may be any bucket
// (cross-bucket transfer).
func (c *Controller) copyOrMove(isMove bool) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	// With a local pane, the copy keys read in the direction the panes point:
	// from the local side they upload, from the remote side with a local
	// pane opposite they download. A move is not offered across the boundary
	// — deleting the source after a transfer is a different promise, and one
	// the local pane deliberately does not make.
	if c.localMode() {
		if isMove {
			c.error("Move", fmt.Errorf(
				"the local pane does not delete: use Ctrl+Y to upload, then remove the local copy yourself"))
			return
		}
		c.uploadFromLocalPane()
		return
	}
	if !isMove && c.otherPaneLocalDir() != "" {
		c.Download()
		return
	}
	if c.readOnlyBlocked("copy or move objects") {
		return
	}
	if c.pane().Bucket() == nil || c.view.List.GetItemCount() == 0 {
		return
	}

	title := "Copy"
	if isMove {
		title = "Move"
	}

	names := c.selectedNames()
	if len(names) == 0 {
		n := c.getSelectedObjectName()
		if n == "" || n == ".." {
			return
		}
		names = []string{n}
	}

	var items []copyMoveItem
	for _, n := range names {
		o, ok := c.pane().Lookup(n)
		if !ok || (o.Ot != model.File && o.Ot != model.Folder) {
			continue
		}
		items = append(items, copyMoveItem{
			shortName: *o.Key,
			srcKey:    *o.FullPath,
			isFolder:  o.Ot == model.Folder,
		})
	}
	if len(items) == 0 {
		return
	}

	srcBucketName := *c.pane().Bucket().Key
	srcPrefix := c.pane().Path()

	// In dual-pane mode the destination defaults to the OTHER pane's location
	// (the Midnight-Commander copy-to-other-panel workflow); otherwise it
	// defaults to the source.
	dstBucketName := srcBucketName
	dstPrefix := srcPrefix
	if c.dual {
		if o := c.other(); o.Bucket() != nil {
			dstBucketName = *o.Bucket().Key
			dstPrefix = o.Path()
		}
	}

	// List buckets so the destination dropdown offers cross-bucket targets. Off
	// the UI goroutine (network); if it fails, fall back to the current bucket
	// only so same-bucket copy/move keeps working.
	go func() {
		bucketNames := []string{srcBucketName}
		if list, err := c.model.ListBuckets(context.Background()); err == nil {
			bucketNames = bucketNames[:0]
			seen := false
			for _, b := range list {
				if b == nil || b.Key == nil {
					continue
				}
				bucketNames = append(bucketNames, *b.Key)
				if *b.Key == srcBucketName {
					seen = true
				}
			}
			if !seen {
				bucketNames = append([]string{srcBucketName}, bucketNames...)
			}
		}

		c.shell.Do(func() {
			form := c.view.NewCopyMoveForm(title, bucketNames, dstBucketName, dstPrefix)
			form.AddButton(title, func() {
				_, dstBucketName := form.GetFormItem(0).(*tview.DropDown).GetCurrentOption()
				dstPrefix := strings.TrimSpace(form.GetFormItem(1).(*tview.InputField).GetText())
				lay.Close()

				dstPrefix = model.NormalizePrefix(dstPrefix)
				sameBucket := dstBucketName == srcBucketName
				if sameBucket && dstPrefix == srcPrefix {
					c.error(title+" failed", fmt.Errorf("destination equals source"))
					return
				}

				dstBucket := c.pane().Bucket()
				if !sameBucket {
					bn := dstBucketName
					dstBucket = &model.Object{Key: &bn, Ot: model.Bucket}
				}
				srcBucket := c.pane().Bucket()
				scope := c.pane().ScopeKey()
				mdl := c.model
				c.confirmOverwrites(mdl, dstBucket, title,
					plannedCopyKeys(mdl, srcBucket, dstBucket, items, dstPrefix),
					func(skip map[string]bool) {
						c.runCopyOrMove(isMove, title, items, srcBucket, dstBucket, dstBucketName, dstPrefix, scope, nil, skip)
					})
			})
			form.AddButton("Cancel", func() {
				lay.Close()
			})
			form.SetInputCapture(escapeCloses(&lay))
			lay = c.pushModal(form, 75, 11)
		})
	}()
}

// runCopyOrMove hands the items to the shared runner as an ops.Copy. The
// pipeline — per-item dispatch, the server's object acknowledgements, the
// failure ledger and the retry set — is ops.Copy, which knows nothing about
// the terminal. What stays here is what is genuinely the controller's: the
// one-step undo, the cut clipboard, and the marks in the source scope.
func (c *Controller) runCopyOrMove(isMove bool, title string, items []copyMoveItem, srcBucket, dstBucket *model.Object, dstBucketName, dstPrefix, srcScope string, afterMove func(), skip map[string]bool) {
	// Runs on the UI goroutine: capture the client before spawning, so a
	// profile switch can't retarget the transfer mid-run.
	mdl := c.model

	op := &ops.Copy{
		Move:          isMove,
		Title:         title,
		Items:         copyItemsToOps(items),
		SrcBucket:     srcBucket,
		DstBucket:     dstBucket,
		DstBucketName: dstBucketName,
		DstPrefix:     dstPrefix,
		Skip:          skip,
		Mover:         mdl,
	}
	if isMove {
		op.OnMoved = func(moved []ops.Transfer) {
			c.setUndo(&undoOp{
				items: invertOps(opsToTransferPairs(moved)),
				desc:  fmt.Sprintf("%s of %d item(s)", title, len(moved)),
			})
			// A cut clipboard is consumed only when something actually moved:
			// a fully failed (or same-place) paste keeps the cut so it can be
			// retried. Serialized through the UI queue — c.clip is UI state.
			if afterMove != nil {
				c.shell.Do(afterMove)
			}
			// Clear the marks in the scope the items were marked in — NOT the
			// current scope: a paste finishes in the destination listing, and
			// clearing there left phantom marks in the source folder that made
			// later commands silently operate on nothing.
			c.pane().ClearSelectedIn(srcScope)
		}
	}
	c.runner().Start(op)
}

// copyItemsToOps projects the controller's clipboard entries onto the
// operation's. The size the controller carries is only needed by the
// cross-profile copy, which streams bytes and has its own flow.
func copyItemsToOps(items []copyMoveItem) []ops.CopyItem {
	out := make([]ops.CopyItem, 0, len(items))
	for _, it := range items {
		out = append(out, ops.CopyItem{ShortName: it.shortName, SrcKey: it.srcKey, IsFolder: it.isFolder})
	}
	return out
}

// opsToTransferPairs is the inverse direction, for the undo ledger.
func opsToTransferPairs(moved []ops.Transfer) []transferPair {
	out := make([]transferPair, 0, len(moved))
	for _, m := range moved {
		out = append(out, transferPair{
			srcBucket: m.SrcBucket, dstBucket: m.DstBucket,
			srcKey: m.SrcKey, dstKey: m.DstKey, isFolder: m.IsFolder,
		})
	}
	return out
}

// clipItems maps objects to clipboard/copy entries, skipping anything that is
// not a file or folder (buckets, nil, keyless).
func clipItems(objs []*model.Object) []copyMoveItem {
	var items []copyMoveItem
	for _, o := range objs {
		if o == nil || o.Key == nil || o.FullPath == nil {
			continue
		}
		if o.Ot != model.File && o.Ot != model.Folder {
			continue
		}
		items = append(items, copyMoveItem{shortName: *o.Key, srcKey: *o.FullPath, isFolder: o.Ot == model.Folder})
	}
	return items
}

// yank fills the clipboard from the marked set (or the highlighted item). op is
// "copy" or "cut".
func (c *Controller) yank(op string) {
	if c.remoteOnly("Yank") {
		return
	}
	if c.pane().Bucket() == nil {
		return
	}
	names := c.selectedNames()
	if len(names) == 0 {
		n := c.getSelectedObjectName()
		if n == "" || n == ".." {
			return
		}
		names = []string{n}
	}
	var objs []*model.Object
	for _, n := range names {
		if o, ok := c.pane().Lookup(n); ok {
			objs = append(objs, o)
		}
	}
	items := clipItems(objs)
	if len(items) == 0 {
		return
	}
	c.clip = clipboard{op: op, bucket: c.pane().Bucket(), prefix: c.pane().Path(), scope: c.pane().ScopeKey(), items: items}
	verb := "Copied"
	if op == "cut" {
		verb = "Cut"
	}
	c.success(fmt.Sprintf("%s %d item(s) — press p to paste", verb, len(items)))
}

// paste copies (or moves, for a cut) the clipboard into the current location,
// reusing the copy/move runner with the clipboard's origin as the source.
func (c *Controller) paste() {
	if c.remoteOnly("Paste") {
		return
	}
	if c.readOnlyBlocked("paste objects") {
		return
	}
	if c.pane().Bucket() == nil || c.clip.bucket == nil || len(c.clip.items) == 0 {
		return
	}
	dstBucketName := *c.pane().Bucket().Key
	if c.clip.bucket.Key != nil && *c.clip.bucket.Key == dstBucketName &&
		model.NormalizePrefix(c.clip.prefix) == model.NormalizePrefix(c.pane().Path()) {
		c.error("Paste failed", fmt.Errorf("destination equals source"))
		return
	}
	isMove := c.clip.op == "cut"
	items := c.clip.items
	srcBucket := c.clip.bucket
	dstBucket := c.pane().Bucket()
	dstPrefix := c.pane().Path()
	scope := c.clip.scope
	mdl := c.model
	c.confirmOverwrites(mdl, dstBucket, "Paste",
		plannedCopyKeys(mdl, srcBucket, dstBucket, items, dstPrefix),
		func(skip map[string]bool) {
			// The cut is consumed by the runner once something actually moved,
			// so a failed or canceled paste keeps the clipboard for a retry.
			c.runCopyOrMove(isMove, "Paste", items, srcBucket, dstBucket, dstBucketName, dstPrefix,
				scope, func() { c.clip = clipboard{} }, skip)
		})
}

// Undo reverses the last move/rename (one step) after a confirmation.
func (c *Controller) Undo() {
	// confirmLay is assigned below; every handler here runs after that.
	var confirmLay *shell.Layer
	if c.remoteOnly("Undo") {
		return
	}
	if c.readOnlyBlocked("undo (it writes objects back)") {
		return
	}
	op := c.takeUndo()
	if op == nil {
		c.success("Nothing to undo")
		return
	}
	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Undo %s?", op.desc)).
		SetDoneFunc(func(_ int, label string) {
			confirmLay.Close()
			if label != widgets.ConfirmOK {
				return
			}
			c.runUndo(op)
		})
	confirmLay = c.pushRaw(confirm)
}

// runUndo applies the reverse moves behind a cancellable progress modal.
func (c *Controller) runUndo(op *undoOp) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	mdl := c.model
	ctx, cancel := context.WithCancel(context.Background())
	progress := tview.NewModal().
		SetText("Undoing...\n").
		AddButtons([]string{"Cancel"}).
		SetDoneFunc(func(_ int, _ string) {
			cancel()
			lay.Close()
		})
	lay = c.pushRaw(progress)

	go func() {
		var failed []string
		okCount := 0
	loop:
		for i, it := range op.items {
			select {
			case <-ctx.Done():
				break loop
			default:
			}
			n := i + 1
			line := it.srcKey
			c.shell.Do(func() {
				progress.SetText(fmt.Sprintf("Undoing %d/%d\n%s", n, len(op.items), line))
			})
			// No overwrite prompt here: undo has its own confirmation and puts
			// objects back where they were moments ago.
			if _, err := mdl.MoveKeys(ctx, it.srcBucket, it.dstBucket, it.srcKey, it.dstKey, it.isFolder, nil, nil); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", it.srcKey, err))
				continue
			}
			okCount++
		}
		c.setUndo(nil) // one-step undo, now consumed
		c.logActivity("undo %s: %d ok, %d failed", op.desc, okCount, len(failed))

		c.shell.Do(func() {
			status := "Undo complete."
			if len(failed) > 0 {
				status = "Undo finished with errors."
			}
			msg := fmt.Sprintf("%s\n\nOK: %d\nFailed: %d", status, okCount, len(failed))
			for _, f := range failed {
				msg += "\n  - " + f
			}
			msg += "\n\nPress Done to return."
			progress.ClearButtons()
			progress.AddButtons([]string{"Done"})
			progress.SetText(msg)
			progress.SetDoneFunc(func(_ int, _ string) {
				lay.Close()
			})
			c.view.App.SetFocus(progress)
		})
		c.updateList()
	}()
}

// ShowSummaryModal opens graphical bucket/folder summary.
func (c *Controller) ShowSummaryModal() {
	if c.remoteOnly("The size summary") {
		return
	}
	var bucket *model.Object
	prefix := c.pane().Path()

	if c.pane().Bucket() == nil {
		cur := c.getSelectedObjectName()
		if cur == "" {
			return
		}
		if val, ok := c.pane().Lookup(cur); ok && val != nil && val.Ot == model.Bucket {
			bucket = val
		} else {
			tmp := cur
			bucket = &model.Object{Key: &tmp, Ot: model.Bucket}
		}
		prefix = ""
	} else {
		bucket = c.pane().Bucket()

		// If a folder is selected, summarize that folder; else summarize current path.
		cur := c.getSelectedObjectName()
		if cur != "" {
			if val, ok := c.pane().Lookup(cur); ok && val != nil && val.Ot == model.Folder {
				prefix = *val.FullPath // full prefix, already ends with "/"
			}
		}
	}

	if bucket == nil || bucket.Key == nil {
		return
	}

	c.showSummaryModalFor(bucket, prefix, nil)
}

func (c *Controller) showSummaryModalFor(bucket *model.Object, prefix string, prev *shell.Layer) {
	prefix = model.NormalizePrefix(prefix)

	scopeLabel := *bucket.Key
	if prefix != "" {
		scopeLabel = fmt.Sprintf("%s/%s", *bucket.Key, strings.TrimSuffix(prefix, "/"))
	}

	// The scan is a full recursive listing, so it gets a wait modal it can be
	// cancelled from rather than an unexplained pause.
	wait, _, ctx, cancel := c.cancellableWait(fmt.Sprintf("Scanning %s ...", scopeLabel))

	go func() {
		defer cancel()
		objects, err := c.model.ListObjects(ctx, prefix, bucket)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Failed to build summary", err)
			}
			return
		}

		total, catRows, groupRows := buildSummary(objects, prefix)

		c.shell.Do(func() {
			// lay is assigned at the end of this closure, before any handler
			// below can fire.
			var lay *shell.Layer
			graph := c.view.NewSummaryGraph(" Summary ", scopeLabel, total, catRows, groupRows, func(groupName string) {
				if groupName == "" || groupName == "(root)" {
					return
				}
				if !strings.HasSuffix(groupName, "/") {
					return
				}
				nextPrefix := prefix + groupName
				c.showSummaryModalFor(bucket, nextPrefix, lay)
			})

			wrapper := tview.NewFlex().SetDirection(tview.FlexRow)
			wrapper.AddItem(graph.Root, 0, 1, true)
			wrapper.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
				switch ev.Key() {
				case tcell.KeyEsc:
					lay.Close()
					return nil
				case tcell.KeyTAB:
					if c.view.App.GetFocus() == graph.Cats {
						c.view.App.SetFocus(graph.Groups)
					} else {
						c.view.App.SetFocus(graph.Cats)
					}
					return nil
				}
				if ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q') {
					lay.Close()
					return nil
				}
				return ev
			})

			lay = c.pushModal(wrapper, 100, 28)
			prev.Close()
			c.view.App.SetFocus(graph.Cats)
		})
	}()
}

func buildSummary(objs []s3t.Object, prefix string) (total int64, cats []widgets.SummaryRow, groups []widgets.SummaryRow) {
	var docs, arch, media, other int64
	groupBytes := make(map[string]int64)

	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		key := *o.Key
		if key == prefix {
			continue
		}
		// Ignore folder marker objects
		if strings.HasSuffix(key, "/") {
			continue
		}
		sz := o.Size
		if sz < 0 {
			sz = 0
		}
		total += sz

		switch detectCategory(key) {
		case "documents":
			docs += sz
		case "archives":
			arch += sz
		case "media":
			media += sz
		default:
			other += sz
		}

		rel := key
		if prefix != "" && strings.HasPrefix(key, prefix) {
			rel = strings.TrimPrefix(key, prefix)
		}
		if rel == "" {
			continue
		}
		parts := strings.SplitN(rel, "/", 2)
		g := "(root)"
		if len(parts) >= 2 && parts[0] != "" {
			g = parts[0] + "/"
		}
		groupBytes[g] += sz
	}

	cats = []widgets.SummaryRow{
		{Name: "Documents", Bytes: docs},
		{Name: "Archives", Bytes: arch},
		{Name: "Media", Bytes: media},
		{Name: "Other", Bytes: other},
	}

	for name, b := range groupBytes {
		groups = append(groups, widgets.SummaryRow{Name: name, Bytes: b})
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Bytes == groups[j].Bytes {
			return groups[i].Name < groups[j].Name
		}
		return groups[i].Bytes > groups[j].Bytes
	})

	// Top 10, but disclosed: the truncation used to be silent, so a bucket
	// with a hundred prefixes showed ten of them and looked complete. The
	// usage browser (Ctrl+G's sibling, see usage.go) is the un-truncated
	// view; this row says so rather than leaving the gap unexplained.
	if len(groups) > summaryGroupRows {
		var rest int64
		var restN int
		for _, g := range groups[summaryGroupRows:] {
			rest += g.Bytes
			restN++
		}
		groups = groups[:summaryGroupRows]
		groups = append(groups, widgets.SummaryRow{
			Name:  fmt.Sprintf("(%d more prefixes)", restN),
			Bytes: rest,
		})
	}

	return total, cats, groups
}

// summaryGroupRows is how many prefixes the summary graph names before
// rolling the rest into one disclosed row.
const summaryGroupRows = 10

func detectCategory(key string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(key), "."))
	if ext == "" {
		return "other"
	}
	switch ext {
	// documents
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
		"txt", "md", "rtf", "odt", "ods", "odp", "csv", "json", "yaml", "yml":
		return "documents"
	// archives
	case "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "tgz", "tbz", "zst":
		return "archives"
	// media
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff",
		"mp3", "wav", "flac", "aac", "ogg",
		"mp4", "mkv", "mov", "avi", "webm":
		return "media"
	default:
		return "other"
	}
}

// watchResize re-renders the active pane when its width changes, so the column
// layout follows terminal resizes and dual-pane toggles instead of keeping the
// widths it was built with. Converges after one extra render: the re-render
// records the new width, so the next draw sees no change.
func (c *Controller) watchResize() {
	c.view.App.SetAfterDrawFunc(func(_ tcell.Screen) {
		if !c.browsing {
			return
		}
		_, _, w, _ := c.view.List.GetInnerRect()
		if w > 0 && w != c.lastWidth {
			c.lastWidth = w
			go c.renderList()
		}
	})
}

func (c *Controller) setInput() {
	c.watchResize()
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		}
		return event
	})
	// Both panes get the same handlers; they act on the active pane through the
	// c.view.* fields, and only the focused (active) pane receives events.
	for i := 0; i < 2; i++ {
		c.view.PaneList(i).SetInputCapture(c.listInputCapture)
		c.wireListChanged(c.view.PaneList(i))
	}
}

// wireListChanged makes a list update the details panel as its selection moves.
// It reads the secondary text of the changed row (the object key) directly, so
// it works regardless of which pane fired it.
func (c *Controller) wireListChanged(list *tview.List) {
	list.SetChangedFunc(func(_ int, _ string, secondary string, _ rune) {
		c.fillDetails(strings.TrimSpace(secondary))
	})
}

// actionTable maps every action to what it does. Separating this from the
// bindings is what makes the keymap configurable: the table is the app's
// vocabulary, keymap.go decides which keys speak it, and the palette and the
// hotkey panel are both generated from the pair.
func (c *Controller) actionTable() map[actionID]func() {
	return map[actionID]func(){
		actUp:             c.Up,
		actToggleSelect:   c.ToggleSelectCurrent,
		actSelectAll:      c.SelectAllVisible,
		actClearSelection: c.ClearSelection,
		actFilter:         c.focusFilter,
		actRefresh:        c.Refresh,
		actRefreshAlt:     c.Refresh,
		actSortKey:        c.CycleSort,
		actSortDir:        c.ToggleSortDir,
		actDownload:       c.Download,
		actUpload:         func() { c.ShowLocalFSModal(c.params.HomeDir) },
		actDelete:         c.Delete,
		actCreate:         c.Create,
		actRename:         c.Rename,
		actCopy:           func() { c.copyOrMove(false) },
		actMove:           func() { c.copyOrMove(true) },
		actYank:           func() { c.yank("copy") },
		actCut:            func() { c.yank("cut") },
		actPaste:          c.paste,
		actUndo:           c.Undo,
		actSearch:         c.RecursiveSearch,
		actProperties:     func() { c.ShowFileProperties(c.getSelectedObjectName()) },
		actPresign:        func() { c.PresignLink(c.getSelectedObjectName()) },
		actSummary:        c.ShowSummaryModal,
		actUsage:          c.ShowUsage,
		actPreview:        c.Preview,
		actVerify:         c.VerifyObject,
		actVersions:       c.ShowVersions,
		actMeta:           c.EditObjectMeta,
		actStorageClass:   c.ChangeStorageClass,
		actEdit:           c.EditObject,
		actDuplicates:     c.FindDuplicates,
		actSync:           c.Sync,
		actCompare:        c.ComparePanes,
		actCrossCopy:      c.CopyToProfile,
		actBookmarks:      c.Bookmarks,
		actPalette:        c.CommandPalette,
		actTransfers:      c.transfers.Show,
		actHistoryBack:    c.HistoryBack,
		actHistoryFwd:     c.HistoryForward,
		actHistoryBackAlt: c.HistoryBack,
		actHistoryFwdAlt:  c.HistoryForward,
		actDualPane:       c.ToggleDualPane,
		actLocalPane:      c.ToggleLocalPane,
		actTrashRestore:   c.RestoreFromTrash,
		actTrashEmpty:     c.EmptyTrash,
		actProfiles:       c.Profiles,
		actActivityLog:    c.ShowActivityLog,
		actBucketConfig:   c.BucketDashboard,
		actAbortUploads:   c.AbortMultipartUploads,
		actHotkeys:        c.ShowHotkeys,
		actAbout:          c.ShowAbout,
		actQuit:           c.Stop,
		// Actions whose behaviour depends on pane state rather than on a
		// single method.
		actSwapPane: func() {
			if c.dual {
				c.swapAndFocus()
			}
		},
		actCancelListing: func() { c.CancelListing() },
	}
}

// ShowHotkeys opens the hotkey panel for the browser screen.
func (c *Controller) ShowHotkeys() {
	// helpLay is assigned below; every handler here runs after that.
	var helpLay *shell.Layer
	help := c.view.HotkeysModal(false, c.keys.helpLines(), func() {
		helpLay.Close()
	})
	helpLay = c.pushRaw(help)
}

// ShowAbout opens the about panel.
func (c *Controller) ShowAbout() {
	// aboutLay is assigned below; every handler here runs after that.
	var aboutLay *shell.Layer
	about := c.view.AboutModal()
	about.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
		aboutLay.Close()
		return nil
	})
	aboutLay = c.pushModal(about, 70, 19)
}

// listInputCapture is the shared browser-pane key handler: it resolves the
// event through the keymap and runs the action, leaving anything unbound to
// the list widget (arrows, Enter, page keys).
//
// Esc is a special case worth naming: it cancels a running listing when there
// is one, and otherwise falls through, so it keeps working as "get out of
// here" for the widgets underneath.
func (c *Controller) listInputCapture(event *tcell.EventKey) *tcell.EventKey {
	table := c.actionTable()

	// A leader press arms the second namespace; the next key resolves there.
	if c.leaderArmed {
		c.leaderArmed = false
		c.view.SetStatus("")
		if id, ok := c.keys.lookupLeader(event); ok {
			if run, ok := table[id]; ok {
				run()
			}
			return nil
		}
		// An unknown key after the leader does nothing rather than falling
		// through: "," then a typo should not delete anything.
		return nil
	}
	if c.keys.isLeader(event) {
		c.leaderArmed = true
		c.view.SetStatus(c.leaderHint())
		return nil
	}

	id, ok := c.keys.lookup(event)
	if !ok {
		return event
	}
	if id == actCancelListing {
		if c.CancelListing() {
			return nil
		}
		return event
	}
	if run, ok := table[id]; ok {
		run()
		return nil
	}
	return event
}

// leaderHint lists what the leader namespace offers, in the status field, so
// the second namespace is discoverable by pressing the key rather than by
// reading the docs.
func (c *Controller) leaderHint() string {
	chords := make([]string, 0, len(c.keys.leader))
	for chord, id := range c.keys.leader {
		name := string(id)
		if i := strings.Index(chord, "r:"); i >= 0 {
			chords = append(chords, chord[i+2:]+" "+name)
		}
	}
	sort.Strings(chords)
	return " " + strings.Join(chords, " · ") + " "
}

// swapPane snapshots the active pane into panes[active], loads the other pane,
// and repoints the active-widget pointers. It performs no rendering.
func (c *Controller) swapPane() {
	// Swapping is now just a change of which pane is addressed: there is no
	// twelve-field copy to race a navigation goroutine writing the same
	// fields, which is what made a Tab during a slow bucket-open land in the
	// wrong pane.
	c.active ^= 1
	c.view.List = c.view.PaneList(c.active)
	c.view.Filter = c.view.PaneFilter(c.active)
	c.view.Header = c.view.PaneHeader(c.active)
}

// swapAndFocus switches the active pane (Tab), refreshes the border highlight
// and focus, and re-fetches the newly active pane so it reflects any changes
// made from the other pane.
func (c *Controller) swapAndFocus() {
	c.swapPane()
	c.view.SetActivePane(c.active)
	c.view.App.SetFocus(c.view.List)
	go c.updateList()
}

// ToggleDualPane shows/hides the second pane (Ctrl+O). Turning it on seeds pane
// 1 with the current location (mirror) and focuses it; turning it off returns
// to the single primary pane.
func (c *Controller) ToggleDualPane() {
	if c.dual {
		if c.active == 1 {
			c.swapPane() // make pane 0 active again before collapsing
		}
		c.dual = false
		c.view.ShowSinglePane()
		c.view.App.SetFocus(c.view.List)
		return
	}

	c.dual = true
	c.view.ShowDualPane()
	if !c.pane1Init {
		c.pane1Init = true
		// Seed the second pane as a mirror of the first.
		c.panes[1].Enter(c.pane().Bucket(), c.pane().Path(), "")
	}
	c.swapAndFocus()
}

// resetPanes returns to a clean single-pane state on pane 0. Called when
// entering a profile (fresh browser) or the profiles screen.
func (c *Controller) resetPanes() {
	c.dual = false
	c.active = 0
	c.pane1Init = false
	c.view.List = c.view.PaneList(0)
	c.view.Filter = c.view.PaneFilter(0)
	c.view.Header = c.view.PaneHeader(0)
	// Both panes start over. A local pane must not survive a profile switch
	// either: its selection scope and history belong to the session that
	// opened it, and a fresh Pane drops all three at once.
	c.panes = [2]*session.Pane{session.New(), session.New()}
	c.view.ShowSinglePane()
	c.filterSuppress = true
	c.view.PaneFilter(0).SetText("")
	c.view.PaneFilter(1).SetText("")
	c.filterSuppress = false
}

func (c *Controller) fillDetails(key string) {
	c.view.Details.Clear()
	var otype string
	if val, ok := c.pane().Lookup(key); ok {
		switch ot := val.Ot; ot {
		case model.File:
			otype = "File"
		case model.Folder:
			otype = "Folder"
		case model.Bucket:
			otype = "Bucket"
		default:
			otype = "Unknown"
		}
		fmt.Fprintf(c.view.Details, "[green] Type: [white] %v\n", otype)
		if val.Ot == model.File {
			fmt.Fprintf(c.view.Details, "[green] Size: [white] %s\n", humanize.IBytes(uint64(*val.Size)))
		}
		if val.LastModified != nil {
			fmt.Fprintf(c.view.Details, "[green] Modified: [white] %v\n", val.LastModified)
		}
		if val.Etag != nil {
			fmt.Fprintf(c.view.Details, "[green] Etag: [white] %s\n\n", *val.Etag)
		}
		if val.Ot != model.Bucket {
			fmt.Fprintf(c.view.Details, "[green] FullPath: [white] %s\n\n", *val.FullPath)
		}
		if val.StorageClass != nil {
			fmt.Fprintf(c.view.Details, "[green] Storage class: [white] %s\n", *val.StorageClass)
		}
	}
}

func (c *Controller) Run() error {
	c.shell.Start()
	c.Profiles()
	c.runStatusTicker()
	c.logActivity("session started")
	// A keys.json the app could not honour is reported once here rather than
	// leaving the user to guess why a rebinding did nothing.
	for _, w := range c.keys.warnings {
		c.logActivity("keymap: %s", w)
	}
	// A missing/corrupt config no longer crashes startup; surface it as a
	// modal over the (empty) profiles screen so the user can recover.
	if len(c.keys.warnings) > 0 && c.params.LoadErr == nil {
		msg := c.view.NewErrorMessageQ("Keymap", strings.Join(c.keys.warnings, "; "))
		var keymapLay *shell.Layer
		msg.SetDoneFunc(func(int, string) { keymapLay.Close() })
		keymapLay = c.pushModal(msg, 90, 10)
	}
	if c.params.LoadErr != nil {
		errMsg := c.view.NewErrorMessageQ("Config error", c.params.LoadErr.Error())
		var cfgLay *shell.Layer
		errMsg.SetDoneFunc(func(int, string) { cfgLay.Close() })
		cfgLay = c.pushModal(errMsg, 8, 3)
	}
	return c.view.App.Run()
}

// pushModal centres p over whatever is on screen and puts it on the overlay
// stack. The returned handle is the only way to take it down again, so two
// flows can no longer collide on a page name the way twelve "progress" callers
// used to.
func (c *Controller) pushModal(p tview.Primitive, width, height int) *shell.Layer {
	return c.shell.Push(c.view.ModalEdit(p, width, height))
}

// pushModalClamped is pushModal for an overlay that must shrink to fit the
// terminal rather than run off the bottom.
func (c *Controller) pushModalClamped(p tview.Primitive, width, height int) *shell.Layer {
	return c.shell.Push(c.view.ModalClamped(p, width, height))
}

// pushRaw puts a primitive that already centres itself (tview.Modal) on the
// stack unwrapped.
func (c *Controller) pushRaw(p tview.Primitive) *shell.Layer {
	return c.shell.Push(p)
}

// error shows a modal error report. Safe from any goroutine: the toast goes
// through shell.Do, which never blocks, so an input handler and a transfer
// worker call it the same way. (It used to be the other way round — a bare
// call from the UI goroutine froze the app.)
func (c *Controller) error(header string, err error) {
	errMsg := c.view.NewErrorMessageQ(header, err.Error())
	errMsg.SetDoneFunc(func(int, string) { c.shell.CloseToast() })
	c.shell.Toast(c.view.ModalEdit(errMsg, 8, 3))
}

func (c *Controller) success(header string) {
	succMsg := c.view.NewSuccessMessageQ(header)
	succMsg.SetDoneFunc(func(int, string) { c.shell.CloseToast() })
	c.shell.Toast(c.view.ModalEdit(succMsg, 8, 3))
}

// chooseDir opens an icon-styled directory browser (same look as the upload
// browser) and calls onChosen with the directory the user picks. Used as the
// download-destination fallback when the active profile has no DownloadDir.
func (c *Controller) chooseDir(startPath string, onChosen func(dir string)) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	currentPath := startPath
	layout, localList := c.view.NewCreateLocalFileListForm()
	app := c.view.App

	// The picker lives on its own page name so it can OVERLAY a form (the
	// sync dialog's Browse…) instead of replacing it: closing the picker then
	// reveals the form untouched, and tview refocuses the front page.
	selectBtn := tview.NewButton("Select here").SetSelectedFunc(func() {
		dir := currentPath
		if !strings.HasSuffix(dir, string(os.PathSeparator)) {
			dir += string(os.PathSeparator)
		}
		lay.Close()
		onChosen(dir)
	})
	cancelBtn := tview.NewButton("Cancel").SetSelectedFunc(func() {
		lay.Close()
	})

	buttonRow := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(selectBtn, 0, 1, false).
		AddItem(tview.NewBox(), 2, 0, false).
		AddItem(cancelBtn, 0, 1, false)

	flex, _ := layout.(*tview.Flex)
	flex.AddItem(buttonRow, 1, 0, false)

	focusables := []tview.Primitive{localList, selectBtn, cancelBtn}
	focusIndex := 0
	setNextFocus := func() {
		focusIndex = (focusIndex + 1) % len(focusables)
		app.SetFocus(focusables[focusIndex])
	}

	var renderList func(string)
	renderList = func(curPath string) {
		currentPath = curPath
		localList.Clear()
		localList.SetTitle(fmt.Sprintf("Choose download dir: %s", curPath)).SetBorder(true)

		entries, err := os.ReadDir(curPath)
		if err != nil {
			c.error("Failed to read directory", err)
			return
		}

		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})

		if parent := filepath.Dir(curPath); parent != curPath {
			localList.AddItem("[cyan]📁[-] ..", "..", 0, func(p string) func() {
				return func() { renderList(p) }
			}(filepath.Dir(curPath)))
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			full := filepath.Join(curPath, name)
			localList.AddItem("[cyan]📁[-] "+name+"/", name, 0, func(p string) func() {
				return func() { renderList(p) }
			}(full))
		}
	}

	localList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab:
			setNextFocus()
			return nil
		case tcell.KeyEsc:
			lay.Close()
			return nil
		case tcell.KeyBackspace2:
			renderList(filepath.Dir(currentPath))
			return nil
		}
		return event
	})
	selectBtn.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			setNextFocus()
			return nil
		}
		return event
	})
	cancelBtn.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			setNextFocus()
			return nil
		}
		return event
	})

	modal := c.view.ModalEdit(layout, 60, 25)
	lay = c.pushRaw(modal)
	renderList(startPath)
}

func (c *Controller) ShowLocalFSModal(startPath string) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	if c.pane().Bucket() == nil {
		return
	}
	currentPath := startPath
	layout, localList := c.view.NewCreateLocalFileListForm()

	app := c.view.App

	okBtn := tview.NewButton("Upload").SetSelectedFunc(func() {
		i := localList.GetCurrentItem()
		if i < 0 || localList.GetItemCount() == 0 {
			return
		}
		_, raw := localList.GetItemText(i)
		if raw == "" || raw == ".." {
			return
		}
		fullPath := filepath.Join(currentPath, raw)

		lay.Close()
		c.Upload(fullPath)
	})

	cancelBtn := tview.NewButton("Cancel").SetSelectedFunc(func() {
		lay.Close()
	})

	buttonRow := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(okBtn, 0, 1, false).
		AddItem(tview.NewBox(), 2, 0, false).
		AddItem(cancelBtn, 0, 1, false)

	flex, _ := layout.(*tview.Flex)
	flex.AddItem(buttonRow, 1, 0, false)

	focusables := []tview.Primitive{localList, okBtn, cancelBtn}
	focusIndex := 0
	setNextFocus := func() {
		focusIndex = (focusIndex + 1) % len(focusables)
		app.SetFocus(focusables[focusIndex])
	}

	var renderList func(string)
	renderList = func(curPath string) {
		currentPath = curPath
		localList.Clear()
		localList.SetTitle(fmt.Sprintf("Local FS: %s", curPath)).SetBorder(true)

		entries, err := os.ReadDir(curPath)
		if err != nil {
			c.error("Failed to read directory", err)
			return
		}

		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})

		// Parent navigation entry.
		if parent := filepath.Dir(curPath); parent != curPath {
			localList.AddItem("[cyan]📁[-] ..", "..", 0, func(p string) func() {
				return func() { renderList(p) }
			}(filepath.Dir(curPath)))
		}

		for _, entry := range entries {
			name := entry.Name()
			fullPath := filepath.Join(curPath, name)
			isDir := entry.IsDir()

			var label string
			switch {
			case isDir:
				label = "[cyan]📁[-] " + name + "/"
			case entry.Type()&os.ModeSymlink != 0:
				label = "[magenta]🔗[-] " + name
			default:
				label = "📄 " + name
			}

			localList.AddItem(label, name, 0, func(p string, dir bool) func() {
				return func() {
					if dir {
						renderList(p)
					}
				}
			}(fullPath, isDir))
		}
	}

	localList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab:
			setNextFocus()
			return nil
		case tcell.KeyEsc:
			lay.Close()
			return nil
		case tcell.KeyBackspace2:
			parent := filepath.Dir(currentPath)
			renderList(parent)
			return nil
		}
		return event
	})

	okBtn.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			setNextFocus()
			return nil
		}
		return event
	})
	cancelBtn.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			setNextFocus()
			return nil
		}
		return event
	})

	modal := c.view.ModalEdit(layout, 60, 25)
	lay = c.pushRaw(modal)
	renderList(startPath)
}

func (c *Controller) Upload(localPath string) {
	if c.readOnlyBlocked("upload") {
		return
	}
	// Capture the destination now, on the UI goroutine. The byte phase starts
	// only after a jobSem slot frees, which can be minutes later — reading
	// c.pane().Bucket() then would panic on the buckets screen, or silently
	// upload into whatever bucket the user happens to be viewing.
	dstBucket := c.pane().Bucket()
	dstPath := c.pane().Path()
	// The client too: a profile switch while this job is queued must not
	// retarget the upload at the new profile's endpoint.
	mdl := c.model

	files, _, err := mdl.PrepareUpload(localPath, dstPath, dstBucket)
	if err == nil && len(files) == 0 {
		// A directory with no files is still uploadable: Upload creates its
		// folder-marker object. Only a non-directory with nothing to send is
		// an error.
		if fi, statErr := os.Stat(localPath); statErr == nil && fi.IsDir() {
			c.uploadEmptyDir(localPath, dstPath, dstBucket)
			return
		}
	}
	if err != nil || len(files) == 0 {
		if err == nil {
			err = fmt.Errorf("nothing to upload")
		}
		c.error("Upload failed", err)
		return
	}

	// PrepareUpload already knows every key this would write, so the overwrite
	// scan needs no extra listing of the source.
	plan := func() ([]string, error) {
		keys := make([]string, 0, len(files))
		for _, f := range files {
			keys = append(keys, f.RemotePath)
		}
		return keys, nil
	}
	c.confirmOverwrites(mdl, dstBucket, "Upload", plan, func(skip map[string]bool) {
		kept, keptSize := unskippedUploads(files, skip)
		if len(kept) == 0 {
			c.success("Nothing to upload")
			return
		}
		c.runUpload(mdl, localPath, dstPath, dstBucket, kept, keptSize, skip)
	})
}

// unskippedUploads drops the targets the user chose to keep and re-totals the
// bytes, so the progress gauge measures what is actually being sent.
func unskippedUploads(files []model.UploadTarget, skip map[string]bool) ([]model.UploadTarget, int64) {
	if len(skip) == 0 {
		var total int64
		for _, f := range files {
			total += f.Size
		}
		return files, total
	}
	kept := make([]model.UploadTarget, 0, len(files))
	var total int64
	for _, f := range files {
		if skip[f.RemotePath] {
			continue
		}
		kept = append(kept, f)
		total += f.Size
	}
	return kept, total
}

// runUpload hands the approved files to the shared runner as an ops.Upload.
// Everything that used to be open-coded here — the job, the Background/Cancel
// modal, the throttle, the summary — now lives once in pkg/ui/flows.
func (c *Controller) runUpload(mdl *model.Model, localPath, dstPath string, dstBucket *model.Object, files []model.UploadTarget, totalSize int64, skip map[string]bool) {
	c.runner().Start(&ops.Upload{
		LocalPath:  localPath,
		DstPrefix:  dstPath,
		Bucket:     dstBucket,
		Files:      files,
		TotalBytes: totalSize,
		Skip:       skip,
		Pusher:     mdl,
		Gate:       c.queue,
	})
}

// uploadEmptyDir uploads a directory that contains no files: Upload creates
// its folder-marker object. There are no bytes to meter, so this skips the
// progress modal and the transfer queue entirely.
func (c *Controller) uploadEmptyDir(localPath, dstPath string, dstBucket *model.Object) {
	mdl := c.model
	go func() {
		if err := mdl.Upload(context.Background(), localPath, dstPath, dstBucket, nil, nil); err != nil {
			c.error("Upload failed", err)
			return
		}
		c.logActivity("Upload: empty folder %s → %s/%s", filepath.Base(localPath), *dstBucket.Key, dstPath)
		c.success("Empty folder created")
		c.updateList()
	}()
}

func (c *Controller) ShowFileProperties(key string) {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	// A local pane holds File objects too, but no bucket, no ETag and no S3
	// link — reading them was a nil dereference that took the whole TUI down.
	if c.remoteOnly("Properties") {
		return
	}
	obj, ok := c.pane().Lookup(key)
	if !ok || obj.Ot != model.File {
		return
	}
	bucket := c.pane().Bucket()
	if bucket == nil || bucket.Key == nil || obj.FullPath == nil {
		return
	}

	bucketName := *bucket.Key
	fullPath := *obj.FullPath

	var url string
	if strings.Contains(c.model.Cf.Url, "amazonaws.com") && c.model.Cf.Region != nil {
		url = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucketName, *c.model.Cf.Region, fullPath)
	} else {
		url = fmt.Sprintf("%s/%s/%s", c.model.Cf.Url, bucketName, fullPath)
	}

	// Size and ETag are optional on an S3-compatible listing, so neither is
	// dereferenced blind.
	size := "-"
	if obj.Size != nil {
		size = humanize.IBytes(uint64(*obj.Size))
	}
	etag := "-"
	if obj.Etag != nil {
		etag = *obj.Etag
	}

	text := fmt.Sprintf(
		"[black]Name: [white]%s\n[black]Size: [white]%s\n[black]Modified: [white]%v\n[black]Etag: [white]%s\n[black]Link: [white]%s",
		*obj.Key,
		size,
		obj.LastModified,
		etag,
		url,
	)

	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"Copy Link", "Presign Link", "Close"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			lay.Close()
			switch buttonLabel {
			case "Copy Link":
				u.CopyToClipboard(url)
				c.success("Link copied to clipboard")
			case "Presign Link":
				c.PresignLink(key)
			}
		})

	lay = c.pushModal(modal, 75, 12)
}

// PresignLink prompts for an expiry and copies a presigned GET URL for the
// selected object to the clipboard. Unlike the naive public link, this works
// for private buckets.
func (c *Controller) PresignLink(key string) {
	// Without this a local file reached the model and came back with a bare
	// "bucket is nil", which says nothing about why.
	if c.remoteOnly("Presigned links") {
		return
	}
	obj, ok := c.pane().Lookup(key)
	if !ok || obj.Ot != model.File {
		return
	}
	if obj.FullPath == nil {
		c.error("Presign", fmt.Errorf("object has no path"))
		return
	}
	bucket := c.pane().Bucket()
	if bucket == nil {
		return
	}
	fullPath := *obj.FullPath

	type ttlChoice struct {
		label string
		ttl   time.Duration
	}
	choices := []ttlChoice{
		{"15 min", 15 * time.Minute},
		{"1 hour", time.Hour},
		{"24 hours", 24 * time.Hour},
		{"7 days", model.PresignMaxTTL},
	}

	labels := make([]string, 0, len(choices)+1)
	for _, ch := range choices {
		labels = append(labels, ch.label)
	}
	labels = append(labels, "Cancel")

	// lay is assigned at the bottom; the handler below runs only after that.
	var lay *shell.Layer
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Presigned link expiry for:\n%s", *obj.Key)).
		AddButtons(labels).
		SetDoneFunc(func(_ int, buttonLabel string) {
			lay.Close()
			var ttl time.Duration
			for _, ch := range choices {
				if ch.label == buttonLabel {
					ttl = ch.ttl
					break
				}
			}
			if ttl == 0 { // Cancel or unknown
				return
			}
			go func() {
				link, err := c.model.PresignGetURL(bucket, fullPath, ttl)
				if err != nil {
					c.error("Presign failed", err)
					return
				}
				u.CopyToClipboard(link)
				c.success("Presigned link copied to clipboard")
			}()
		})

	// The properties modal, if that is where this came from, closed its own
	// layer before calling in — nothing to dismiss here any more.
	lay = c.pushModal(modal, 75, 12)
}

func (c *Controller) clearDetailsIfNoSelection() {
	c.shell.Do(func() {
		if c.view.List.GetItemCount() == 0 {
			c.view.Details.Clear()
			return
		}
		i := c.view.List.GetCurrentItem()
		if i < 0 {
			c.view.Details.Clear()
			return
		}
		_, cur := c.view.List.GetItemText(i)
		if strings.TrimSpace(cur) == "" {
			c.view.Details.Clear()
		}
	})
}

// searchMaxResults caps the recursive-search result list so a query matching a
// huge bucket can't build an unbounded modal; the cap is surfaced in the title.
const searchMaxResults = 1000

// searchHit is one recursive-search result: a full object key and its size.
type searchHit struct {
	bucket string // "" = current bucket (single-bucket search)
	key    string
	size   int64
}

// searchMatch reports whether a full object key matches query. The query is a
// substring, a glob or an "re:" regex (see match.go); matching is against the
// whole key, so a glob's "*" has to cross "/". Folder-marker keys (ending in
// "/") never match; search targets real objects.
func searchMatch(key, query string) bool {
	return matchKey(mustMatcher(query), key)
}

// matchKey applies a pre-compiled matcher to a full object key, skipping
// folder markers. Compiling once outside the loop matters: a recursive search
// runs this over every key in the prefix.
func matchKey(m matcher, key string) bool {
	if key == "" || strings.HasSuffix(key, "/") {
		return false
	}
	return m.match(key)
}

// computeHits filters a recursive object listing to those matching query,
// returning at most max hits and whether more were dropped (max <= 0: no cap).
func computeHits(objs []s3t.Object, query string, max int) (hits []searchHit, truncated bool) {
	m := mustMatcher(query)
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		k := *o.Key
		if !matchKey(m, k) {
			continue
		}
		if max > 0 && len(hits) >= max {
			truncated = true
			break
		}
		hits = append(hits, searchHit{key: k, size: o.Size})
	}
	return hits, truncated
}

// parentPrefix returns the S3 prefix of the folder containing key (terminated
// with "/"), or "" when key sits at the bucket root. A trailing slash on key
// (folder marker) is treated as part of the name, so the parent is the folder
// above it.
func parentPrefix(key string) string {
	k := strings.TrimSuffix(key, "/")
	i := strings.LastIndex(k, "/")
	if i < 0 {
		return ""
	}
	return k[:i+1]
}

// RecursiveSearch prompts for a query, then lists every object under the current
// prefix (recursively) whose key matches, in a results modal. Enter on a result
// reveals it (navigates to its folder and highlights it).
func (c *Controller) RecursiveSearch() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	if c.remoteOnly("Recursive search") {
		return
	}
	if c.pane().Bucket() == nil {
		return // only meaningful inside a bucket
	}
	bucket := c.pane().Bucket()
	prefix := c.pane().Path()

	form := c.view.NewSearchForm("Recursive search")
	form.AddButton("Search", func() {
		q := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		allBuckets := form.GetFormItem(1).(*tview.Checkbox).IsChecked()
		lay.Close()
		if q == "" {
			return
		}
		// A regex that does not compile is reported here rather than silently
		// finding nothing: unlike the live filter, a search is a deliberate
		// one-shot request and an empty result set would read as "no matches".
		if _, err := newMatcher(q); err != nil {
			c.error("Invalid search pattern", err)
			return
		}
		if allBuckets {
			c.runAllBucketsSearch(q)
			return
		}
		c.runRecursiveSearch(bucket, prefix, q)
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 65, 9)
}

// runAllBucketsSearch searches every bucket recursively (off the UI
// goroutine), collecting matches tagged with their bucket and capped at
// searchMaxResults total. Buckets that can't be listed (a different region on
// the shared client, a denied permission) are skipped.
//
// Matching happens page by page rather than after a full listing: an
// all-buckets scan can page through millions of keys, and accumulating them
// only to throw nearly all of them away cost memory proportional to the
// account rather than to the result set.
func (c *Controller) runAllBucketsSearch(query string) {
	wait, searching, ctx, cancel := c.cancellableWait(fmt.Sprintf("Searching all buckets for %q ...", query))

	go func() {
		defer cancel()
		buckets, err := c.model.ListBuckets(ctx)
		if err != nil {
			c.shell.Do(wait.Close)
			if ctx.Err() == nil {
				c.error("Search failed", err)
			}
			return
		}
		m := mustMatcher(query)
		var hits []searchHit
		truncated := false
		progress := c.searchProgress(searching, query)
		scanned := 0

		for _, b := range buckets {
			if b == nil || b.Key == nil || ctx.Err() != nil {
				continue
			}
			if len(hits) >= searchMaxResults {
				truncated = true
				break
			}
			name := *b.Key
			err := c.model.ListObjectsStream(ctx, "", b, func(page []s3t.Object, _ int) bool {
				scanned += len(page)
				progress(scanned, len(hits))
				for _, o := range page {
					if o.Key == nil || !matchKey(m, *o.Key) {
						continue
					}
					if len(hits) >= searchMaxResults {
						truncated = true
						return false
					}
					hits = append(hits, searchHit{bucket: name, key: *o.Key, size: o.Size})
				}
				return true
			})
			if err != nil && ctx.Err() != nil {
				break // canceled: report what was found so far
			}
		}
		partial := ctx.Err() != nil
		c.shell.Do(func() {
			wait.Close()
			c.presentSearchResults(query, hits, truncated || partial)
		})
	}()
}

// runRecursiveSearch searches the current prefix recursively off the UI
// goroutine, streaming pages so a huge prefix reports progress and can be
// stopped, then shows the matches (or an error / "no matches" note).
func (c *Controller) runRecursiveSearch(bucket *model.Object, prefix, query string) {
	wait, searching, ctx, cancel := c.cancellableWait(fmt.Sprintf("Searching for %q ...", query))

	go func() {
		defer cancel()
		m := mustMatcher(query)
		var hits []searchHit
		truncated := false
		progress := c.searchProgress(searching, query)
		scanned := 0

		err := c.model.ListObjectsStream(ctx, prefix, bucket, func(page []s3t.Object, _ int) bool {
			scanned += len(page)
			progress(scanned, len(hits))
			for _, o := range page {
				if o.Key == nil || !matchKey(m, *o.Key) {
					continue
				}
				if len(hits) >= searchMaxResults {
					truncated = true
					return false
				}
				hits = append(hits, searchHit{key: *o.Key, size: o.Size})
			}
			return true
		})
		canceled := ctx.Err() != nil
		if err != nil && !canceled {
			c.shell.Do(wait.Close)
			c.error("Search failed", err)
			return
		}
		c.shell.Do(func() {
			wait.Close()
			c.presentSearchResults(query, hits, truncated || canceled)
		})
	}()
}

// searchProgress builds a throttled "scanned N, M hits" updater for the search
// wait modal. Without it a scan of a large prefix is indistinguishable from a
// hang, which is what made the uncancellable version feel broken.
func (c *Controller) searchProgress(m *tview.Modal, query string) func(scanned, hits int) {
	var last time.Time
	return func(scanned, hits int) {
		if time.Since(last) < listProgressEvery {
			return
		}
		last = time.Now()
		c.shell.Do(func() {
			m.SetText(fmt.Sprintf("Searching for %q ...\n\nscanned %s keys, %d match(es)",
				query, humanCount(scanned), hits))
		})
	}
}

// presentSearchResults builds the results list (or a "no matches" note) and
// shows it. Must run on the UI goroutine.
func (c *Controller) presentSearchResults(query string, hits []searchHit, truncated bool) {
	if len(hits) == 0 {
		m := tview.NewModal().
			SetText(fmt.Sprintf("No matches for %q", query)).
			AddButtons([]string{"OK"})
		var emptyLay *shell.Layer
		m.SetDoneFunc(func(int, string) { emptyLay.Close() })
		emptyLay = c.pushModal(m, 50, 8)
		return
	}

	// lay is assigned at the bottom; the handlers below run only after that.
	var lay *shell.Layer
	results := tview.NewList().ShowSecondaryText(false)
	results.SetBorder(true)
	results.SetSelectedBackgroundColor(tcell.ColorBlue)
	results.SetSelectedTextColor(tcell.ColorWhite)

	countLabel := fmt.Sprintf("%d match(es)", len(hits))
	if truncated {
		countLabel = fmt.Sprintf("first %d match(es)", len(hits))
	}
	results.SetTitle(fmt.Sprintf(" Search %q — %s (Enter: reveal, Esc: close) ", query, countLabel))

	for _, h := range hits {
		h := h
		label := fmt.Sprintf("📄 %s  [gray](%s)[-]", h.key, humanize.IBytes(uint64(h.size)))
		if h.bucket != "" {
			label = fmt.Sprintf("[yellow]%s[-]/%s  [gray](%s)[-]", h.bucket, h.key, humanize.IBytes(uint64(h.size)))
		}
		results.AddItem(label, h.key, 0, func() {
			lay.Close()
			if h.bucket != "" && (c.pane().Bucket() == nil || h.bucket != *c.pane().Bucket().Key) {
				c.jumpTo(h.bucket, parentPrefix(h.key), h.key)
			} else {
				c.revealKey(h.key)
			}
		})
	}
	results.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			lay.Close()
			return nil
		}
		return event
	})

	lay = c.pushModal(results, 100, 30)
	c.view.App.SetFocus(results)
}

// revealKey navigates to the folder containing key and highlights it. key is a
// full object key inside the current bucket (from a search result).
func (c *Controller) revealKey(key string) {
	c.clearFilterUI() // ensure the revealed item isn't hidden by an old filter
	c.recordHistory()
	c.pane().SetPath(parentPrefix(key))
	c.pane().SetRestoreNext(key)
	go c.updateList()
}

// addBookmark appends bm unless an entry with the same Bucket+Prefix already
// exists; returns the (possibly unchanged) list and whether it was added.
func addBookmark(list []cfg.Bookmark, bm cfg.Bookmark) ([]cfg.Bookmark, bool) {
	for _, b := range list {
		if b.Bucket == bm.Bucket && b.Prefix == bm.Prefix {
			return list, false
		}
	}
	return append(list, bm), true
}

// jumpTo navigates to bucketName + prefix (bookmarks, cross-bucket reveal). It
// resolves the bucket and rebuilds the client region off the UI goroutine,
// mirroring Down. selectKey, if non-empty, is the object to highlight on
// arrival; otherwise the cursor lands on "..".
func (c *Controller) jumpTo(bucketName, prefix, selectKey string) {
	c.clearFilterUI()
	c.view.Details.Clear()
	c.recordHistory()
	pane := c.pane() // the jump belongs to this pane, Tab or no Tab
	go func() {
		bucket := c.findBucketByName(context.Background(), bucketName)
		if bucket == nil {
			c.error("Jump", fmt.Errorf("bucket %q not found", bucketName))
			return
		}
		if err := c.model.RefreshClient(&bucketName); err != nil {
			c.error("Failed to resolve bucket region", err)
		}
		restore := selectKey
		if restore == "" {
			restore = ".."
		}
		pane.Enter(bucket, prefix, restore)
		c.updateList()
		c.shell.Do(func() { c.view.App.SetFocus(c.view.List) })
	}()
}

// Bookmarks opens the per-profile bookmark manager: Enter jumps to a bookmark,
// the first row adds the current location, Del removes, Esc closes.
func (c *Controller) Bookmarks() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	if c.remoteOnly("Bookmarks") {
		return
	}
	if c.activeConfig == nil {
		return
	}
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(" Bookmarks — Enter: go • Del: remove • Esc: close ")
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)
	// The rows are built before the overlay exists, so they are handed the
	// address of the handle rather than its value: passing lay here would
	// freeze the nil it still holds, and Layer.Close is nil-safe, so picking
	// a bookmark would navigate without ever taking the manager down.
	c.rebuildBookmarkList(list, &lay)

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			lay.Close()
			return nil
		case tcell.KeyDelete:
			c.removeBookmarkAt(list, &lay)
			return nil
		}
		return ev
	})

	lay = c.pushModal(list, 80, 24)
	c.view.App.SetFocus(list)
}

// bookmarkOffset is the number of leading non-bookmark rows in the manager list
// (the "add current" row, shown only when inside a bucket).
func (c *Controller) bookmarkOffset() int {
	if c.pane().Bucket() != nil {
		return 1
	}
	return 0
}

// rebuildBookmarkList fills the manager's rows. lay is the manager's own
// overlay, taken by address because the rows are wired before it is pushed and
// are rebuilt afterwards — see Bookmarks.
func (c *Controller) rebuildBookmarkList(list *tview.List, lay **shell.Layer) {
	list.Clear()
	if c.pane().Bucket() != nil {
		list.AddItem("[green]＋ Add current location[-]", "", 0, func() {
			c.addCurrentBookmark(list, lay)
		})
	}
	for _, bm := range c.activeConfig.Bookmarks {
		bm := bm
		list.AddItem("★ "+bm.Name, "", 0, func() {
			(*lay).Close()
			c.jumpTo(bm.Bucket, bm.Prefix, "")
		})
	}
	if len(c.activeConfig.Bookmarks) == 0 && c.pane().Bucket() == nil {
		list.AddItem("[gray](no bookmarks)[-]", "", 0, func() {})
	}
}

func (c *Controller) addCurrentBookmark(list *tview.List, lay **shell.Layer) {
	if c.pane().Bucket() == nil {
		return
	}
	name := *c.pane().Bucket().Key + "/" + c.pane().Path()
	bm := cfg.Bookmark{Name: name, Bucket: *c.pane().Bucket().Key, Prefix: c.pane().Path()}
	newList, added := addBookmark(c.activeConfig.Bookmarks, bm)
	if !added {
		c.error("Bookmark", fmt.Errorf("this location is already bookmarked"))
		return
	}
	c.activeConfig.Bookmarks = newList
	if err := c.params.WriteConfig(); err != nil {
		c.error("Bookmark", err)
		return
	}
	c.rebuildBookmarkList(list, lay)
}

func (c *Controller) removeBookmarkAt(list *tview.List, lay **shell.Layer) {
	idx := list.GetCurrentItem() - c.bookmarkOffset()
	if idx < 0 || idx >= len(c.activeConfig.Bookmarks) {
		return
	}
	c.activeConfig.Bookmarks = append(c.activeConfig.Bookmarks[:idx], c.activeConfig.Bookmarks[idx+1:]...)
	if err := c.params.WriteConfig(); err != nil {
		c.error("Bookmark", err)
		return
	}
	c.rebuildBookmarkList(list, lay)
}

// location is a navigable position: a bucket (nil = the buckets screen) and a
// prefix within it.
func (c *Controller) curLocation() session.Visit {
	return session.Visit{Bucket: c.pane().Bucket(), Path: c.pane().Path()}
}

// recordHistory pushes the current location so a later Back returns to it.
// Called at the start of user-initiated navigation, before state changes.
func (c *Controller) recordHistory() {
	c.pane().Hist().Record(c.curLocation())
}

// HistoryBack / HistoryForward move through the navigation history.
func (c *Controller) HistoryBack() {
	if loc, ok := c.pane().Hist().Back(c.curLocation()); ok {
		c.navigateTo(loc)
	}
}

func (c *Controller) HistoryForward() {
	if loc, ok := c.pane().Hist().Forward(c.curLocation()); ok {
		c.navigateTo(loc)
	}
}

// navigateTo jumps straight to loc without touching the history stacks (the
// history machinery drives it). Mirrors Down/jumpTo's client handling.
func (c *Controller) navigateTo(loc session.Visit) {
	c.clearFilterUI()
	c.view.Details.Clear()
	// Capture the pane here, on the UI goroutine. The goroutine below writes
	// to this pane whatever the user does with Tab meanwhile — the whole
	// point of panes being addressed rather than mirrored.
	pane := c.pane()
	if loc.Bucket == nil {
		pane.Enter(nil, "", "")
		go c.updateList()
		return
	}
	go func() {
		name := *loc.Bucket.Key
		if err := c.model.RefreshClient(&name); err != nil {
			c.error("Failed to resolve bucket region", err)
		}
		// One write for the whole location: a reader can never catch the new
		// bucket paired with the old prefix.
		pane.Enter(loc.Bucket, loc.Path, "..")
		c.updateList()
	}()
}

// paletteAction is one entry in the command palette: a label and the action to
// run when it is chosen.
type paletteAction struct {
	label string
	run   func()
}

// subsequence reports whether every rune of sub appears in s in order (so "dl"
// matches "Download"). Both are compared as-is; callers lower-case first.
func subsequence(s, sub string) bool {
	subR := []rune(sub)
	i := 0
	for _, r := range s {
		if i < len(subR) && subR[i] == r {
			i++
		}
	}
	return i == len(subR)
}

// filterActions returns the actions whose label matches query as a case-
// insensitive subsequence. An empty query returns a copy of all actions.
func filterActions(actions []paletteAction, query string) []paletteAction {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		out := make([]paletteAction, len(actions))
		copy(out, actions)
		return out
	}
	var out []paletteAction
	for _, a := range actions {
		if subsequence(strings.ToLower(a.label), q) {
			out = append(out, a)
		}
	}
	return out
}

// paletteActions is the registry of browser-screen actions the palette offers.
func (c *Controller) paletteActions() []paletteAction {
	return []paletteAction{
		{"Download", func() { c.Download() }},
		{"Upload (local browser)", func() { c.ShowLocalFSModal(c.params.HomeDir) }},
		{"New bucket/folder", c.Create},
		{"Delete", func() { c.Delete() }},
		{"Rename", c.Rename},
		{"Copy to…", func() { c.copyOrMove(false) }},
		{"Move to…", func() { c.copyOrMove(true) }},
		{"Clipboard: copy (yank)", func() { c.yank("copy") }},
		{"Clipboard: cut", func() { c.yank("cut") }},
		{"Clipboard: paste", c.paste},
		{"Undo last move/rename", c.Undo},
		{"Sync (local ⇄ remote, or remote → remote)", c.Sync},
		{"Compare the two panes", c.ComparePanes},
		{"Find duplicates (size + ETag)", c.FindDuplicates},
		{"Edit in $EDITOR", c.EditObject},
		{"Copy to another profile…", c.CopyToProfile},
		{"Transfers", c.transfers.Show},
		{"Preview (read-only)", c.Preview},
		{"Usage browser (du)", c.ShowUsage},
		{"Verify local copy against object", c.VerifyObject},
		{c.localPaneLabel(), c.ToggleLocalPane},
		{"Upload from the local pane", c.uploadFromLocalPane},
		{"Restore from trash", c.RestoreFromTrash},
		{"Empty the trash", c.EmptyTrash},
		{"Filter listing", c.focusFilter},
		{"Refresh listing", c.Refresh},
		{"Sort: cycle name/size/date", c.CycleSort},
		{"Sort: reverse direction", c.ToggleSortDir},
		{"Recursive search", c.RecursiveSearch},
		{"Bookmarks", c.Bookmarks},
		{"Toggle dual-pane", c.ToggleDualPane},
		{"History back", c.HistoryBack},
		{"History forward", c.HistoryForward},
		{"Size summary", c.ShowSummaryModal},
		{"Properties", func() { c.ShowFileProperties(c.getSelectedObjectName()) }},
		{"Versions (history / restore)", c.ShowVersions},
		{"Metadata & tags", c.EditObjectMeta},
		{"Storage class / Glacier restore", c.ChangeStorageClass},
		{"Presign link", func() { c.PresignLink(c.getSelectedObjectName()) }},
		{"Select all visible", c.SelectAllVisible},
		{"Clear selection", c.ClearSelection},
		{"Abort incomplete uploads", c.AbortMultipartUploads},
		{"Bucket config", c.BucketDashboard},
		{"Activity log", c.ShowActivityLog},
		{"Back to profiles", c.Profiles},
	}
}

// CommandPalette opens a fuzzy action launcher: type to filter, ↑/↓ to move,
// Enter to run, Esc to close.
func (c *Controller) CommandPalette() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	actions := c.paletteActions()
	var shown []paletteAction

	list := tview.NewList().ShowSecondaryText(false)
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	input := tview.NewInputField().SetLabel("> ")

	fill := func(q string) {
		shown = filterActions(actions, q)
		list.Clear()
		for _, a := range shown {
			list.AddItem(a.label, "", 0, nil)
		}
	}
	fill("")

	run := func() {
		i := list.GetCurrentItem()
		if i < 0 || i >= len(shown) {
			return
		}
		act := shown[i]
		lay.Close()
		c.view.App.SetFocus(c.view.List)
		act.run()
	}

	input.SetChangedFunc(func(text string) { fill(text) })
	input.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			lay.Close()
			c.view.App.SetFocus(c.view.List)
			return nil
		case tcell.KeyEnter:
			run()
			return nil
		case tcell.KeyDown:
			if n := list.GetItemCount(); n > 0 {
				list.SetCurrentItem((list.GetCurrentItem() + 1) % n)
			}
			return nil
		case tcell.KeyUp:
			if n := list.GetItemCount(); n > 0 {
				list.SetCurrentItem((list.GetCurrentItem() - 1 + n) % n)
			}
			return nil
		}
		return ev
	})

	flex := tview.NewFlex().SetDirection(tview.FlexRow)
	flex.AddItem(input, 1, 0, true)
	flex.AddItem(list, 0, 1, false)
	flex.SetBorder(true).SetTitle(" Command palette — type to filter, ↑/↓, Enter, Esc ")

	lay = c.pushModal(flex, 70, 22)
	c.view.App.SetFocus(input)
}

// activityMax caps the in-session operation log.
const activityMax = 200

// logActivity records a one-line, timestamped operation entry. Safe from any
// goroutine.
func (c *Controller) logActivity(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	c.activityMu.Lock()
	c.activity = appendCapped(c.activity, activityEntry{when: now, msg: msg}, activityMax)
	c.activityMu.Unlock()

	// Also to disk, tagged with the profile: the in-session ring answers
	// "what just happened", the file answers "what did I do yesterday".
	// An unwritable log must never interrupt the operation it describes, so
	// the error is deliberately dropped.
	if c.auditPath != "" {
		_ = appendAudit(c.auditPath, auditLine(now, c.auditProfile(), msg), auditMaxBytes)
	}
}

// ShowActivityLog displays the operation log, newest first.
func (c *Controller) ShowActivityLog() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	c.activityMu.Lock()
	entries := make([]activityEntry, len(c.activity))
	copy(entries, c.activity)
	c.activityMu.Unlock()

	tv := tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	tv.SetBorder(true).SetTitle(" Activity log — Esc to close ")
	if len(entries) == 0 {
		tv.SetText("[gray](no activity yet)[-]")
	} else {
		var b strings.Builder
		for i := len(entries) - 1; i >= 0; i-- { // newest first
			fmt.Fprintf(&b, "[gray]%s[-]  %s\n", entries[i].when.Format("15:04:05"), entries[i].msg)
		}
		tv.SetText(b.String())
	}
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			lay.Close()
			c.view.App.SetFocus(c.view.List)
			return nil
		}
		return ev
	})
	lay = c.pushModal(tv, 90, 30)
	c.view.App.SetFocus(tv)
}

// AbortMultipartUploads lists the current bucket's incomplete multipart uploads
// and lets the user abort them (Del = selected, a = all).
func (c *Controller) AbortMultipartUploads() {
	if c.remoteOnly("Aborting uploads") {
		return
	}
	if c.readOnlyBlocked("abort uploads") {
		return
	}
	if c.pane().Bucket() == nil {
		return
	}
	bucket := c.pane().Bucket()
	go func() {
		ups, err := c.model.ListMultipartUploads(bucket)
		if err != nil {
			c.error("Multipart uploads", err)
			return
		}
		c.shell.Do(func() { c.presentMultipartUploads(bucket, ups) })
	}()
}

func (c *Controller) presentMultipartUploads(bucket *model.Object, ups []model.MultipartUpload) {
	if len(ups) == 0 {
		m := tview.NewModal().SetText("No incomplete multipart uploads.").AddButtons([]string{"OK"})
		var emptyLay *shell.Layer
		m.SetDoneFunc(func(int, string) { emptyLay.Close() })
		emptyLay = c.pushModal(m, 50, 8)
		return
	}

	// lay is assigned at the bottom; every handler below runs after that.
	var lay *shell.Layer
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(" Incomplete uploads — Del: abort • a: abort all • Esc: close ")
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	items := make([]model.MultipartUpload, len(ups))
	copy(items, ups)

	var refresh func()
	abortAt := func(i int) {
		if i < 0 || i >= len(items) {
			return
		}
		up := items[i]
		go func() {
			if err := c.model.AbortMultipartUpload(bucket, up.Key, up.UploadID); err != nil {
				c.error("Abort upload", err)
				return
			}
			c.logActivity("aborted incomplete upload %s (%s)", up.Key, *bucket.Key)
			c.shell.Do(func() {
				for j := range items {
					if items[j].UploadID == up.UploadID && items[j].Key == up.Key {
						items = append(items[:j], items[j+1:]...)
						break
					}
				}
				refresh()
			})
		}()
	}
	abortAll := func() {
		snapshot := make([]model.MultipartUpload, len(items))
		copy(snapshot, items)
		go func() {
			for _, up := range snapshot {
				if err := c.model.AbortMultipartUpload(bucket, up.Key, up.UploadID); err != nil {
					c.error("Abort upload", err)
					return
				}
				c.logActivity("aborted incomplete upload %s (%s)", up.Key, *bucket.Key)
			}
			c.shell.Do(func() {
				items = items[:0]
				refresh()
			})
		}()
	}
	refresh = func() {
		list.Clear()
		if len(items) == 0 {
			list.AddItem("[gray](none left)[-]", "", 0, func() { lay.Close() })
			return
		}
		for _, up := range items {
			id := up.UploadID
			if len(id) > 12 {
				id = id[:12] + "…"
			}
			when := ""
			if up.Initiated != nil {
				when = up.Initiated.Format("2006-01-02 15:04")
			}
			list.AddItem(up.Key, fmt.Sprintf("upload %s • %s", id, when), 0, nil)
		}
	}
	refresh()

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			lay.Close()
			return nil
		case tcell.KeyDelete:
			abortAt(list.GetCurrentItem())
			return nil
		case tcell.KeyRune:
			if ev.Rune() == 'a' {
				abortAll()
				return nil
			}
		}
		return ev
	})

	lay = c.pushModal(list, 90, 24)
	c.view.App.SetFocus(list)
}

// BucketDashboard shows the current bucket's configuration (read-only).
func (c *Controller) BucketDashboard() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	if c.remoteOnly("The bucket dashboard") {
		return
	}
	if c.pane().Bucket() == nil {
		return
	}
	bucket := c.pane().Bucket()
	go func() {
		info, err := c.model.BucketConfig(bucket)
		if err != nil {
			c.error("Bucket config", err)
			return
		}
		c.shell.Do(func() {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(true)
			tv.SetBorder(true).SetTitle(" Bucket config — Esc to close ")
			tv.SetText(fmt.Sprintf(
				"[green]Bucket:[white] %s\n\n[green]Region:[white] %s\n[green]Versioning:[white] %s\n[green]Default encryption:[white] %s\n[green]Object lock:[white] %s",
				*bucket.Key, info.Region, info.Versioning, info.Encryption, info.ObjectLock))
			tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
				if ev.Key() == tcell.KeyEsc {
					lay.Close()
					c.view.App.SetFocus(c.view.List)
					return nil
				}
				return ev
			})
			lay = c.pushModal(tv, 70, 12)
			c.view.App.SetFocus(tv)
		})
	}()
}
