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
	"github.com/nexusriot/s3duck-tui/pkg/model"
	u "github.com/nexusriot/s3duck-tui/pkg/utils"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

type overwriteDecision int

const (
	decOverwrite overwriteDecision = iota
	decSkip
	decOverwriteAll
	decSkipAll
	decCancel
)

type downloadSummary struct {
	totalObjects int
	downloaded   int
	skipped      int
	overwritten  int
	failed       int
	bytesDone    int64

	skippedPaths []string
	failedItems  []string
}

func (s *downloadSummary) addSkipped(p string) {
	s.skipped++
	if len(s.skippedPaths) < 8 {
		s.skippedPaths = append(s.skippedPaths, p)
	}
}

func (s *downloadSummary) addFailed(key string, err error) {
	s.failed++
	if len(s.failedItems) < 8 {
		s.failedItems = append(s.failedItems, fmt.Sprintf("%s: %v", key, err))
	}
}

func (s *downloadSummary) text(totalBytes int64, canceled bool) string {
	status := "Download complete."
	if canceled {
		status = "Download canceled."
	} else if s.failed > 0 {
		status = "Download finished with errors."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", status)
	fmt.Fprintf(&b, "Objects: %d total\n", s.totalObjects)
	fmt.Fprintf(&b, "Downloaded: %d\n", s.downloaded)
	fmt.Fprintf(&b, "Skipped: %d\n", s.skipped)
	fmt.Fprintf(&b, "Overwritten: %d\n", s.overwritten)
	fmt.Fprintf(&b, "Failed: %d\n", s.failed)
	fmt.Fprintf(&b, "Bytes: %s / %s\n",
		humanize.IBytes(uint64(s.bytesDone)),
		humanize.IBytes(uint64(totalBytes)),
	)

	if len(s.skippedPaths) > 0 {
		fmt.Fprintf(&b, "\nSkipped:\n")
		for _, p := range s.skippedPaths {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		if s.skipped > len(s.skippedPaths) {
			fmt.Fprintf(&b, "  ...and %d more\n", s.skipped-len(s.skippedPaths))
		}
	}

	if len(s.failedItems) > 0 {
		fmt.Fprintf(&b, "\nFailed:\n")
		for _, it := range s.failedItems {
			fmt.Fprintf(&b, "  - %s\n", it)
		}
		if s.failed > len(s.failedItems) {
			fmt.Fprintf(&b, "  ...and %d more\n", s.failed-len(s.failedItems))
		}
	}

	fmt.Fprintf(&b, "\nPress Done to return.")
	return b.String()
}

func (c *Controller) selectedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.selScopeLocked())
}

type Controller struct {
	debug           bool
	view            *view.View
	model           *model.Model
	buckets         []*model.Object
	objs            map[string]*model.Object
	currentPath     string
	currentBucket   *model.Object
	bucketPos       int
	position        map[string]int
	params          *cfg.Params
	selectedByScope map[string]map[string]bool
	restoreNext     string
	// activeConfig is the profile currently being browsed (nil on the
	// profiles screen). Used to resolve the per-profile download directory.
	activeConfig *cfg.Config

	// filter is the active in-listing name filter (case-insensitive
	// substring); "" means no filter. Guarded by mu because renderList may
	// read it from a background goroutine while the filter field's change
	// handler (UI goroutine) writes it.
	filter string
	// filterSuppress stops the filter field's change handler from re-rendering
	// while we reset the field programmatically (navigation, reveal). Only
	// touched on the UI goroutine, where SetText fires the handler inline.
	filterSuppress bool

	// hist is the back/forward navigation history for this pane.
	hist histStack

	// Dual-pane state. The controller's live per-location fields above ARE the
	// active pane; panes[inactive] snapshots the other pane. active is 0 or 1;
	// dual is whether both panes are shown; pane1Init guards one-time pane-1
	// seeding. Both panes share one model/endpoint. See DESIGN.md.
	panes     [2]paneState
	active    int
	dual      bool
	pane1Init bool

	// activity is a capped, in-session log of operations shown via the palette.
	activity   []activityEntry
	activityMu sync.Mutex

	// clip is the object clipboard (yank/cut → paste).
	clip clipboard

	// lastUndo is the one-step undo of the last move/rename (reverse ops, ready
	// to apply). Guarded by undoMu since it is written from transfer goroutines.
	lastUndo *undoOp
	undoMu   sync.Mutex

	// Background transfer queue: jobs is the list shown in the transfers panel;
	// jobSem caps concurrent byte-transfer phases at 2 (aggregate bandwidth is
	// separately capped by model.Limiter). transfersList/transfersOpen back the
	// live panel; all touched on the UI goroutine except job fields (own mutex).
	jobs          []*transferJob
	jobsMu        sync.Mutex
	nextJobID     int
	jobSem        chan struct{}
	transfersList *tview.List
	transfersOpen bool

	// mu guards objs and selectedByScope, which are read on tview's UI
	// goroutine (input/list callbacks) while being written by background
	// refresh/upload/download goroutines.
	mu sync.Mutex
	// refreshMu serializes updateList so overlapping refreshes can't stack
	// network calls or race on the object map.
	refreshMu sync.Mutex
}

// paneState is the swappable per-pane state: everything that differs between
// the two dual-pane locations. The active pane's copy lives in the controller's
// own fields; this holds the inactive pane. (Widgets are fixed per pane in the
// view and are not part of this snapshot.)
type paneState struct {
	buckets         []*model.Object
	objs            map[string]*model.Object
	currentPath     string
	currentBucket   *model.Object
	bucketPos       int
	restoreNext     string
	filter          string
	selectedByScope map[string]map[string]bool
	hist            histStack
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

func NewController(debug bool) *Controller {

	v := view.NewView()
	params := cfg.NewParams()

	c := &Controller{
		debug:           debug,
		view:            v,
		model:           nil,
		currentPath:     "",
		bucketPos:       0,
		position:        make(map[string]int),
		params:          params,
		selectedByScope: make(map[string]map[string]bool),
	}
	// The inactive pane needs a live selection map so selScopeLocked never
	// writes to a nil map after a swap.
	c.panes[1].selectedByScope = make(map[string]map[string]bool)
	c.jobSem = make(chan struct{}, 2) // at most 2 concurrent byte transfers
	c.wireFilter(v.PaneFilter(0))
	c.wireFilter(v.PaneFilter(1))
	return c
}

// wireFilter installs a filter box's live-update and key handlers. Called once
// per pane at startup. The handlers act on the active pane via the c.view.*
// fields, so both panes' boxes share the same logic. Typing narrows the current
// listing live; Enter keeps the filter and returns to the list, Esc clears it.
func (c *Controller) wireFilter(filter *tview.InputField) {
	filter.SetChangedFunc(func(text string) {
		if c.filterSuppress {
			return
		}
		c.setFilter(text)
		// Call renderList directly (not via a goroutine) so re-renders stay in
		// keystroke order; it only reads the cached map, then queues the draw.
		c.renderList()
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
	c.setFilter("")
	c.filterSuppress = true
	c.view.Filter.SetText("")
	c.filterSuppress = false
}

// focusFilter moves keyboard focus to the filter box.
func (c *Controller) focusFilter() {
	c.view.App.SetFocus(c.view.Filter)
}

func (c *Controller) askOverwrite(path string) overwriteDecision {
	ch := make(chan overwriteDecision, 1)

	c.view.App.QueueUpdateDraw(func() {
		m := tview.NewModal().
			SetText(fmt.Sprintf("File already exists:\n%s\n\nWhat do you want to do?", path)).
			AddButtons([]string{"Overwrite", "Skip", "Overwrite All", "Skip All", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				c.view.Pages.RemovePage("overwrite")
				switch label {
				case "Overwrite":
					ch <- decOverwrite
				case "Skip":
					ch <- decSkip
				case "Overwrite All":
					ch <- decOverwriteAll
				case "Skip All":
					ch <- decSkipAll
				default:
					ch <- decCancel
				}
			})

		c.view.Pages.AddPage("overwrite", c.view.ModalEdit(m, 80, 10), true, true)
		c.view.App.SetFocus(m)
	})

	return <-ch
}

func (c *Controller) scopeKey() string {
	if c.currentBucket == nil || c.currentBucket.Key == nil {
		return ""
	}
	return *c.currentBucket.Key + ":" + c.currentPath
}

// selScopeLocked returns the selection set for the current scope.
// Callers must hold c.mu.
func (c *Controller) selScopeLocked() map[string]bool {
	s := c.scopeKey()
	if s == "" {
		return nil
	}
	m, ok := c.selectedByScope[s]
	if !ok {
		m = make(map[string]bool)
		c.selectedByScope[s] = m
	}
	return m
}

func (c *Controller) isSelected(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.selScopeLocked()
	return m != nil && m[name]
}

func (c *Controller) toggleSelected(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.selScopeLocked()
	if m == nil {
		return
	}
	if m[name] {
		delete(m, name)
	} else {
		m[name] = true
	}
}

func (c *Controller) clearSelected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.selScopeLocked()
	for k := range m {
		delete(m, k)
	}
}

func (c *Controller) selectNames(names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.selScopeLocked()
	if m == nil {
		return
	}
	for _, n := range names {
		m[n] = true
	}
}

func (c *Controller) lookupObj(name string) (*model.Object, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objs[name]
	return o, ok
}

func (c *Controller) objsSnapshot() []*model.Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*model.Object, 0, len(c.objs))
	for _, o := range c.objs {
		out = append(out, o)
	}
	return out
}

func (c *Controller) setObjs(m map[string]*model.Object) {
	c.mu.Lock()
	c.objs = m
	c.mu.Unlock()
}

// objKey is the unique identity used as the list's secondary text, the c.objs
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

func (c *Controller) makeObjectMap() error {
	var list []*model.Object
	var err error
	dirs := make(map[string]*model.Object)

	if c.currentBucket == nil {
		list, err = c.model.ListBuckets()
		if err != nil {
			return err
		}
	} else {
		list, err = c.model.List(c.currentPath, c.currentBucket)
	}
	if err != nil {
		return err
	}
	for _, obj := range list {
		dirs[objKey(obj)] = obj
	}
	c.setObjs(dirs)
	return nil
}

func localDownloadPath(currentPath, destPath, s3Key string) string {
	key := filepath.ToSlash(s3Key)
	prefix := filepath.ToSlash(currentPath)

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	relativeKey := key
	if strings.HasPrefix(key, prefix) {
		relativeKey = strings.TrimPrefix(key, prefix)
	}

	return filepath.Join(destPath, relativeKey)
}

func getPosition(element string, slice []string) int {
	for k, v := range slice {
		if element == v {
			return k
		}
	}
	return 0
}

// throughput returns the transfer rate (units/sec) and estimated time remaining
// for `done` of `total` units over `elapsed`. rate is 0 when it can't be
// computed yet; eta is -1 (unknown) when there is no measurable rate, total is
// unknown, or the transfer is already complete.
func throughput(done, total int64, elapsed time.Duration) (rate float64, eta time.Duration) {
	secs := elapsed.Seconds()
	if secs <= 0 || done <= 0 {
		return 0, -1
	}
	rate = float64(done) / secs
	if rate <= 0 || total <= 0 || done >= total {
		return rate, -1
	}
	eta = time.Duration(float64(total-done)/rate) * time.Second
	return rate, eta
}

// formatDuration renders a non-negative duration as M:SS (or H:MM:SS past an
// hour). A negative duration — an unknown ETA — renders as "--".
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "--"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// byteRateETA formats a one-line "1.2 MiB/s · ETA 3:45" suffix for a byte
// transfer; unknown values render as "--".
func byteRateETA(done, total int64, elapsed time.Duration) string {
	rate, eta := throughput(done, total, elapsed)
	rateStr := "--"
	if rate > 0 {
		rateStr = humanize.IBytes(uint64(rate)) + "/s"
	}
	return fmt.Sprintf("%s · ETA %s", rateStr, formatDuration(eta))
}

// objRateETA is byteRateETA for object-count transfers (server-side copy/move).
func objRateETA(done, total int, elapsed time.Duration) string {
	rate, eta := throughput(int64(done), int64(total), elapsed)
	rateStr := "--"
	if rate > 0 {
		rateStr = fmt.Sprintf("%.0f obj/s", rate)
	}
	return fmt.Sprintf("%s · ETA %s", rateStr, formatDuration(eta))
}

func (c *Controller) getSelectedObjectName() string {
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	return strings.TrimSpace(cur)
}

func (c *Controller) Profiles() {
	c.resetPanes() // collapse to single-pane; the filter box is inert here
	c.setConfigInput()
	c.fillConfigData()
}

func (c *Controller) Delete() error {
	if c.view.List.GetItemCount() == 0 {
		return nil
	}
	cur := c.getSelectedObjectName()

	val, ok := c.lookupObj(cur)
	if !ok {
		return nil
	}

	// op is the S3 key to delete: the bucket name for buckets, otherwise the
	// full key (FullPath already ends with "/" for folders, which model.Delete
	// treats as a recursive prefix delete).
	op := *val.Key
	if val.Ot != model.Bucket {
		op = *val.FullPath
	}

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to delete %s", op)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")

			if buttonLabel != "OK" {
				return
			}
			go func() {
				var err error
				if val.Ot == model.Bucket {
					err = c.model.DeleteBucket(val.Key)
				} else {
					err = c.model.Delete(&op, c.currentBucket)
				}
				if err != nil {
					c.error(fmt.Sprintf("Failed to delete %s", *val.Key), err, false)
					return
				}
				// Drop the deleted item from the selection set so the
				// "Selected: N" count and markers stay accurate.
				c.mu.Lock()
				if m := c.selScopeLocked(); m != nil {
					delete(m, cur)
				}
				c.mu.Unlock()

				c.updateList()
				c.clearDetailsIfNoSelection()
			}()
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
	return nil
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

func (c *Controller) Download() error {

	if c.view.List.GetItemCount() == 0 || c.currentBucket == nil {
		return nil
	}

	names := c.selectedNames()
	if len(names) == 0 {
		// current item only
		cur := c.getSelectedObjectName()
		names = []string{cur}
	}
	var allObjects []model.DownloadTarget
	var totalSize int64

	for _, name := range names {
		val, ok := c.lookupObj(name)
		if !ok || (val.Ot != model.File && val.Ot != model.Folder) {
			continue
		}

		key := *val.FullPath
		objs, size, err := c.model.ResolveDownloadObjects(
			key,
			val.Ot == model.Folder,
			val.Size,
			c.currentBucket,
		)
		if err != nil {
			return err
		}
		allObjects = append(allObjects, objs...)
		totalSize += size
	}

	if len(allObjects) == 0 {
		return nil
	}

	proceed := func(cwd string) {
		confirm := c.view.NewConfirm()
		confirm.SetText(fmt.Sprintf(
			"Download to %s\nSelected: %d item(s)\nResolved: %d object(s)\nTotal size: %s",
			cwd,
			len(names),
			len(allObjects),
			humanize.IBytes(uint64(totalSize)),
		)).SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")

			if buttonLabel != "OK" {
				return
			}

			ctx, cancel := context.WithCancel(context.Background())
			job := c.addJob("download", fmt.Sprintf("%d obj → %s", len(allObjects), cwd), totalSize, len(allObjects), cancel)
			progress := tview.NewModal().
				SetText("Starting download...\n").
				AddButtons([]string{"Background", "Cancel"})
			progress.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
				switch buttonLabel {
				case "Background":
					// Detach the modal; the goroutine keeps running and reports
					// into the job (watch it in the transfers panel, "t").
					job.setBackgrounded()
					c.view.Pages.RemovePage("progress").SwitchToPage("main")
					c.view.App.SetFocus(c.view.List)
				default: // Cancel — only signal; the worker drains, then
					// showSummary() turns this same modal into the report (unless
					// backgrounded). Removing "progress" here would leave
					// showSummary() focusing a detached modal and soft-lock the UI.
					cancel()
					progress.SetText("Canceling, please wait...")
				}
			})
			c.view.Pages.AddPage("progress", progress, true, true)

			go func() {
				const downloadWorkers = 4

				var (
					sumMu              sync.Mutex
					sum                = downloadSummary{totalObjects: len(allObjects)}
					completedBytes     int64
					completedCount     int
					canceled           bool
					activeProgress     = make(map[string]int64)
					lastDraw           time.Time
					effectiveTotalSize int64     // set after Phase 1; excludes skipped/failed objects
					startTime          time.Time // set when parallel transfer begins (Phase 2)
				)
				const throttle = 100 * time.Millisecond

				// toDownload is declared before showProgress so the closure can
				// reference it (closures capture variables, not values).
				type resolvedItem struct{ object model.DownloadTarget }
				var toDownload []resolvedItem

				showProgress := func() {
					sumMu.Lock()
					now := time.Now()
					if now.Sub(lastDraw) < throttle {
						sumMu.Unlock()
						return
					}
					lastDraw = now
					done := completedCount
					cb := completedBytes
					activePct := int64(0)
					var activeNames []string
					for k, b := range activeProgress {
						activeNames = append(activeNames, path.Base(k))
						activePct += b
					}
					isCanceled := canceled
					sumMu.Unlock()

					if isCanceled {
						return
					}

					visBytes := cb + activePct
					pct := 0.0
					if effectiveTotalSize > 0 {
						pct = float64(visBytes) / float64(effectiveTotalSize) * 100
					}

					activeStr := "resolving..."
					if len(activeNames) > 0 {
						runes := []rune(strings.Join(activeNames, "  "))
						if len(runes) > 50 {
							activeStr = string(runes[:47]) + "..."
						} else {
							activeStr = string(runes)
						}
					}

					job.setProgress(visBytes, done)
					if job.isBackgrounded() {
						return
					}
					c.view.App.QueueUpdateDraw(func() {
						progress.SetText(fmt.Sprintf(
							"Downloading [%d workers]\n%d / %d done\n%s / %s (%.1f%%)\n%s\n\n%s",
							downloadWorkers,
							done, len(toDownload),
							humanize.IBytes(uint64(visBytes)),
							humanize.IBytes(uint64(effectiveTotalSize)),
							pct,
							byteRateETA(visBytes, effectiveTotalSize, time.Since(startTime)),
							activeStr,
						))
					})
				}

				showSummary := func() {
					sumMu.Lock()
					isCanceled := canceled
					failedN := sum.failed
					cb := completedBytes
					cc := completedCount
					sumMu.Unlock()
					job.setProgress(cb, cc)
					c.finalizeJob(job, isCanceled, failedN)
					if job.isBackgrounded() {
						return // no modal to update; details live in the panel
					}
					c.view.App.QueueUpdateDraw(func() {
						report := sum.text(totalSize, isCanceled)

						progress.ClearButtons()
						progress.AddButtons([]string{"Done", "Copy report"})
						progress.SetText(report)

						progress.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
							switch buttonLabel {
							case "Copy report":
								u.CopyToClipboard(report)
								go c.success("Download report copied")
							case "Done":
								c.view.Pages.RemovePage("progress").SwitchToPage("main")
							}
						})
						c.view.App.SetFocus(progress)
					})
				}

				// Overwrite prompts block on UI input and must not overlap, so
				// we resolve all conflicts before launching parallel workers.
				overwriteAll := false
				skipAll := false

				for _, object := range allObjects {
					select {
					case <-ctx.Done():
						sumMu.Lock()
						canceled = true
						sumMu.Unlock()
					default:
					}
					sumMu.Lock()
					isCanceled := canceled
					sumMu.Unlock()
					if isCanceled {
						break
					}

					keyStr := object.Key
					if keyStr == "" {
						sumMu.Lock()
						sum.addFailed("<nil-key>", fmt.Errorf("object key is empty"))
						sumMu.Unlock()
						continue
					}

					// Directory markers: create immediately; don't queue for download.
					if strings.HasSuffix(keyStr, "/") {
						dst := localDownloadPath(c.currentPath, cwd, keyStr)
						if mkErr := os.MkdirAll(dst, 0760); mkErr != nil {
							sumMu.Lock()
							sum.addFailed(keyStr, mkErr)
							sumMu.Unlock()
						} else {
							sumMu.Lock()
							sum.downloaded++
							sumMu.Unlock()
						}
						continue
					}

					dst := localDownloadPath(c.currentPath, cwd, keyStr)
					if _, statErr := os.Stat(dst); statErr == nil {
						if skipAll {
							sumMu.Lock()
							sum.addSkipped(dst)
							sumMu.Unlock()
							continue
						}
						if !overwriteAll {
							switch c.askOverwrite(dst) {
							case decSkip:
								sumMu.Lock()
								sum.addSkipped(dst)
								sumMu.Unlock()
								continue
							case decSkipAll:
								skipAll = true
								sumMu.Lock()
								sum.addSkipped(dst)
								sumMu.Unlock()
								continue
							case decOverwrite:
								if err := os.Remove(dst); err != nil {
									sumMu.Lock()
									sum.addFailed(dst, err)
									sumMu.Unlock()
									continue
								}
								sumMu.Lock()
								sum.overwritten++
								sumMu.Unlock()
							case decOverwriteAll:
								overwriteAll = true
								if err := os.Remove(dst); err != nil {
									sumMu.Lock()
									sum.addFailed(dst, err)
									sumMu.Unlock()
									continue
								}
								sumMu.Lock()
								sum.overwritten++
								sumMu.Unlock()
							default: // decCancel
								cancel()
								sumMu.Lock()
								canceled = true
								sumMu.Unlock()
								showSummary()
								return
							}
						} else {
							if err := os.Remove(dst); err != nil {
								sumMu.Lock()
								sum.addFailed(dst, err)
								sumMu.Unlock()
								continue
							}
							sumMu.Lock()
							sum.overwritten++
							sumMu.Unlock()
						}
					}

					toDownload = append(toDownload, resolvedItem{object})
				}

				sumMu.Lock()
				isCanceled := canceled
				sumMu.Unlock()
				if isCanceled {
					showSummary()
					return
				}

				for _, ri := range toDownload {
					effectiveTotalSize += ri.object.Size
				}
				job.setTotals(effectiveTotalSize, len(toDownload))

				// Cap concurrent byte-transfer phases at 2 (aggregate bandwidth
				// is capped separately by model.Limiter); wait here for a slot.
				job.setStatus(jobQueued)
				select {
				case c.jobSem <- struct{}{}:
					defer func() { <-c.jobSem }()
				case <-ctx.Done():
					sumMu.Lock()
					canceled = true
					sumMu.Unlock()
					showSummary()
					return
				}
				job.setStatus(jobRunning)

				// startTime anchors the rate/ETA to actual transfer time, not the
				// time spent resolving overwrite prompts in Phase 1.
				startTime = time.Now()

				// Each worker holds one semaphore slot; slots are released when
				// the worker finishes so the next queued object can start.
				sem := make(chan struct{}, downloadWorkers)
				var wg sync.WaitGroup

				for _, ri := range toDownload {
					ri := ri
					slotAcquired := false
					select {
					case <-ctx.Done():
						sumMu.Lock()
						canceled = true
						sumMu.Unlock()
					case sem <- struct{}{}:
						slotAcquired = true
					}
					sumMu.Lock()
					isCanceled = canceled
					sumMu.Unlock()
					if isCanceled {
						if slotAcquired {
							<-sem // release the orphaned slot
						}
						break
					}

					wg.Add(1)
					go func() {
						defer wg.Done()
						defer func() { <-sem }()

						keyStr := ri.object.Key
						sumMu.Lock()
						activeProgress[keyStr] = 0
						sumMu.Unlock()
						showProgress()

						n, err := c.model.DownloadTarget(
							ctx,
							ri.object,
							c.currentPath,
							cwd,
							c.currentBucket.Key,
							func(written, _ int64, _ string) {
								sumMu.Lock()
								activeProgress[keyStr] = written
								sumMu.Unlock()
								showProgress()
							},
						)

						sumMu.Lock()
						delete(activeProgress, keyStr)
						sumMu.Unlock()

						if ctx.Err() != nil {
							sumMu.Lock()
							canceled = true
							sumMu.Unlock()
							return
						}
						if err != nil {
							sumMu.Lock()
							sum.addFailed(keyStr, err)
							sumMu.Unlock()
							return
						}
						sumMu.Lock()
						sum.downloaded++
						sum.bytesDone += n
						completedBytes += n
						completedCount++
						sumMu.Unlock()
						showProgress()
					}()
				}

				wg.Wait()
				showSummary()
			}()
		})

		c.view.Pages.AddPage("confirm", confirm, true, true)
	}

	// If the active profile defines a download directory, use it directly.
	// Otherwise let the user pick one with the FS browser dialog.
	if c.activeConfig != nil && strings.TrimSpace(c.activeConfig.DownloadDir) != "" {
		proceed(c.resolveDownloadDir())
	} else {
		c.chooseDir(c.params.HomeDir, func(dir string) {
			proceed(dir)
		})
	}
	return nil
}

// coloredLabelFor returns a colorized icon
func coloredLabelFor(o *model.Object, selected bool) string {
	if o.Key == nil {
		return ""
	}
	name := *o.Key

	switch o.Ot {
	case model.Folder:
		if selected {
			return "[green]📁 " + name + "/[-]"
		}
		return "[cyan]📁[-] " + name + "/"

	case model.File:
		if selected {
			return "[green]📄 " + name + "[-]"
		}
		return "📄 " + name

	case model.Bucket:
		return "[yellow]●[-] " + name

	default:
		return "  " + name
	}
}

func (c *Controller) labelForList(o *model.Object) (primary string, secondary string) {
	if o == nil || o.Key == nil {
		return "", ""
	}
	key := objKey(o)

	selected := false
	if c.currentBucket != nil && (o.Ot == model.File || o.Ot == model.Folder) {
		selected = c.isSelected(key)
	}

	return coloredLabelFor(o, selected), key
}

func (c *Controller) ToggleSelectCurrent() {
	if c.currentBucket == nil || c.view.List.GetItemCount() == 0 {
		return
	}

	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	cur = strings.TrimSpace(cur)

	// ignore special entry
	if cur == "" || cur == ".." {
		return
	}

	obj, ok := c.lookupObj(cur)
	if !ok || (obj.Ot != model.File && obj.Ot != model.Folder) {
		return
	}

	c.toggleSelected(cur)

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

	if err := c.makeObjectMap(); err != nil {
		go c.error("Failed to fetch folder", err, false)
		return err
	}
	c.renderList()
	return nil
}

// renderList repopulates the list widget from the in-memory object map,
// applying the active filter and the folders-first/name sort. It performs no
// network I/O, so it is cheap enough to call on every keystroke (live filter)
// or selection toggle. All widget access is marshalled onto the UI goroutine,
// which also fixes the previous race of reading list state off-goroutine.
func (c *Controller) renderList() {
	title, fText := c.listChrome()
	objs := filterSortObjects(c.objsSnapshot(), c.getFilter())

	c.view.App.QueueUpdateDraw(func() {
		// Preserve the cursor across the rebuild. Reading the widget here (on
		// the UI goroutine) avoids racing tview's event loop.
		keepCur := ""
		if keepIdx := c.view.List.GetCurrentItem(); keepIdx >= 0 && c.view.List.GetItemCount() > 0 {
			_, t := c.view.List.GetItemText(keepIdx)
			keepCur = strings.TrimSpace(t)
		}
		want := keepCur
		if c.restoreNext != "" {
			want = c.restoreNext
		}

		c.view.List.Clear()
		c.view.List.SetTitle(title)
		c.view.SetFrameText(fText)

		offset := 0
		if c.currentBucket != nil {
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

				if val, ok := c.lookupObj(cur); ok {
					if val.Ot == model.Folder || val.Ot == model.Bucket {
						if val.Ot == model.Bucket {
							c.bucketPos = c.view.List.GetCurrentItem()
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
		c.restoreNext = ""
	})
}

// listChrome builds the list title and the frame help line for the current
// screen (buckets vs objects), including the selection count and active filter.
func (c *Controller) listChrome() (title, fText string) {
	var suff string
	if c.currentBucket == nil {
		title = "(buckets)"
	} else {
		base := fmt.Sprintf("(%s)/%s", *c.currentBucket.Key, c.currentPath)
		if n := c.selectedCount(); n > 0 {
			base = fmt.Sprintf("%s  [green]Selected: %d", base, n)
		}
		title = base
		suff = "[::b][Ctrl+D[][::-]Download [::b][Ctrl+U[][::-]Upload [::b][Ctrl+F][::-]Search "
	}
	if f := c.getFilter(); f != "" {
		title = fmt.Sprintf("%s  [yellow]filter:%s", title, f)
	}
	fText = fmt.Sprintf("[::b][↓,↑][::-]D/U [::b][Ent/Bck][::-]L/U %s[::b][/][::-]Filter [::b][Del[][::-]Delete [::b][Ctrl+N][::-]Create [::b][Ctrl+P][::-]Profiles [::b][Ctrl+L][::-]Properties [::b][Ctrl+H][::-]Hotkeys [::b][Ctrl+Q][::-]Quit", suff)
	return title, fText
}

// getFilter / setFilter guard access to c.filter (see the field comment).
func (c *Controller) getFilter() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.filter
}

func (c *Controller) setFilter(s string) {
	c.mu.Lock()
	c.filter = s
	c.mu.Unlock()
}

// filterSortObjects returns the objects whose display name (o.Key) contains
// filter (case-insensitive), sorted folders/buckets first then by name. An
// empty filter returns every object. The input slice is not mutated.
func filterSortObjects(objs []*model.Object, filter string) []*model.Object {
	f := strings.ToLower(strings.TrimSpace(filter))
	out := make([]*model.Object, 0, len(objs))
	for _, o := range objs {
		if o == nil || o.Key == nil {
			continue
		}
		if f != "" && !strings.Contains(strings.ToLower(*o.Key), f) {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ot != out[j].Ot {
			return out[i].Ot > out[j].Ot
		}
		return *out[i].Key < *out[j].Key
	})
	return out
}

func (c *Controller) findBucketByName(name string) *model.Object {
	list, _ := c.model.ListBuckets()
	c.buckets = list
	for _, v := range c.buckets {
		if name == *v.Key {
			return v
		}
	}
	return nil
}

// Down enters the bucket or folder identified by name (a unique objKey: the
// bucket name when on the buckets screen, otherwise the full folder prefix).
func (c *Controller) Down(name string) {
	c.view.Details.Clear()
	c.clearFilterUI() // a filter is scoped to one listing; reset on navigation
	c.recordHistory()

	if c.currentBucket == nil {
		// Entering a bucket triggers network calls (ListBuckets +
		// GetBucketLocation); run them off the UI goroutine so the TUI
		// doesn't freeze on a slow or unreachable endpoint.
		go func() {
			bucket := c.findBucketByName(name)
			if bucket == nil {
				return
			}
			c.currentBucket = bucket
			c.model.RefreshClient(&name)
			c.restoreNext = ".."
			c.updateList()
		}()
		return
	}

	// name is the full folder prefix (already ends with "/").
	c.currentPath = name
	go c.updateList()
}

func (c *Controller) Up() {
	c.view.Details.Clear()
	c.clearFilterUI() // a filter is scoped to one listing; reset on navigation
	c.recordHistory()

	if c.currentPath == "" {
		// Leaving a bucket: restore the cursor onto it in the buckets list.
		if c.currentBucket != nil {
			c.restoreNext = *c.currentBucket.Key
		}
		c.currentBucket = nil
		go c.updateList()
		return
	}

	// Restore the cursor onto the folder we're leaving. Its unique objKey is
	// the full prefix, which equals the current path.
	c.restoreNext = c.currentPath

	fields := strings.FieldsFunc(strings.TrimSpace(c.currentPath), u.SplitFunc)
	if len(fields) == 0 {
		go c.updateList()
		return
	}

	newDir := strings.Join(fields[:len(fields)-1], "/")
	if len(fields) > 1 {
		newDir += "/"
	}
	c.currentPath = newDir
	go c.updateList()
}

func (c *Controller) Stop() {
	c.view.App.Stop()
}

// parseMaxBps reads the profile form's "max bytes/sec" field. Blank, invalid,
// or negative input means unlimited (0).
func parseMaxBps(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (c *Controller) CreateConfigEntry() {
	cForm := c.view.NewCreateProfileForm("Create config entry")
	cForm.AddButton("Save", func() {
		var region *string

		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		downloadDir := cForm.GetFormItem(5).(*tview.InputField).GetText()
		ignoreSsl := cForm.GetFormItem(6).(*tview.Checkbox).IsChecked()
		maxBps := parseMaxBps(cForm.GetFormItem(7).(*tview.InputField).GetText())

		if reg != "" {
			region = &reg
		}
		conf := cfg.Config{
			Name:           name,
			BaseUrl:        url,
			Region:         region,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			IgnoreSsl:      ignoreSsl,
			DownloadDir:    strings.TrimSpace(downloadDir),
			MaxBytesPerSec: maxBps,
		}
		err := c.params.NewConfiguration(&conf)

		c.view.Pages.RemovePage("modal")
		if err != nil {
			go c.error("Error creating config entry", err, false)
		}
		c.fillConfigData()
		c.view.List.SetCurrentItem(len(c.params.Config) - 1)
	})

	cForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 75, 21), true, true)
}

func (c *Controller) EditConfigEntry() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	entry := c.params.Config[i]
	cForm := c.view.NewCreateProfileForm("Edit config entry")

	cForm.GetFormItem(0).(*tview.InputField).SetText(entry.Name)
	cForm.GetFormItem(1).(*tview.InputField).SetText(entry.BaseUrl)
	if entry.Region != nil {
		cForm.GetFormItem(2).(*tview.InputField).SetText(*entry.Region)
	}
	cForm.GetFormItem(3).(*tview.InputField).SetText(entry.AccessKey)
	cForm.GetFormItem(4).(*tview.InputField).SetText(entry.SecretKey)
	cForm.GetFormItem(5).(*tview.InputField).SetText(entry.DownloadDir)
	cForm.GetFormItem(6).(*tview.Checkbox).SetChecked(entry.IgnoreSsl)
	if entry.MaxBytesPerSec > 0 {
		cForm.GetFormItem(7).(*tview.InputField).SetText(strconv.FormatInt(entry.MaxBytesPerSec, 10))
	}

	cForm.AddButton("Save", func() {
		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		downloadDir := cForm.GetFormItem(5).(*tview.InputField).GetText()
		ignoreSsl := cForm.GetFormItem(6).(*tview.Checkbox).IsChecked()
		var region *string
		if reg != "" {
			region = &reg
		}

		entry.Name = name
		entry.BaseUrl = url
		entry.Region = region
		entry.AccessKey = accessKey
		entry.SecretKey = secretKey
		entry.IgnoreSsl = ignoreSsl
		entry.DownloadDir = strings.TrimSpace(downloadDir)
		entry.MaxBytesPerSec = parseMaxBps(cForm.GetFormItem(7).(*tview.InputField).GetText())

		err := c.params.WriteConfig()
		c.view.Pages.RemovePage("modal")
		if err != nil {
			go c.error("Failed to save config", err, false)
		}
		c.fillConfigData()
		c.view.List.SetCurrentItem(i)
	})
	cForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 75, 21), true, true)
}

func (c *Controller) CopyProfile() {

	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	newName := fmt.Sprintf("%s_%s", cur, u.RandStr(4))

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to copy config %s -> %s", cur, newName)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")

			if buttonLabel == "OK" {
				go func() {
					conf := *c.params.Config[i]
					conf.Name = newName
					if err := c.params.CopyConfig(conf); err != nil {
						c.error("Failed to copy config", err, false)
						return
					}
					c.fillConfigData()
					c.view.List.SetCurrentItem(len(c.params.Config) - 1)
				}()

			}
		})
	c.view.Pages.AddAndSwitchToPage("confirm", confirm, true)
}

func (c *Controller) create(isBucket bool) {
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

		// restore is the new object's unique objKey, so the cursor lands on it
		// after the refresh: the bucket name, or the full folder prefix.
		restore := name
		if isBucket {
			public := cForm.GetFormItem(1).(*tview.Checkbox).IsChecked()
			err = c.model.CreateBucket(&name, public)
		} else {
			key := path.Join(c.currentPath, name) + "/"
			err = c.model.CreateFolder(&key, c.currentBucket)
			restore = key
		}

		if err != nil {
			c.view.Pages.RemovePage("modal")
			go c.error("Error creating object", err, false)
			return
		}

		c.view.Pages.RemovePage("modal")
		c.restoreNext = restore
		go c.updateList()
	})

	cForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 65, 9), true, true)
}

func (c *Controller) Create() {
	if c.currentBucket == nil {
		c.create(true)
		return
	}
	c.create(false)
}

func (c *Controller) CheckProfile() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	cf := c.params.Config[i]

	mCf := model.NewConfig(cf.BaseUrl, cf.Region, cf.AccessKey, cf.SecretKey, !cf.IgnoreSsl, cf.MaxBytesPerSec)
	c.model = model.NewModel(mCf)

	// ListBuckets is a network round-trip; run it off the UI goroutine so a
	// slow or unreachable endpoint can't freeze the TUI.
	go func() {
		if _, err := c.model.ListBuckets(); err != nil {
			c.error(fmt.Sprintf("error checking profile %s", cf.Name), err, false)
		} else {
			c.success(fmt.Sprintf("successfully checked profile %s", cf.Name))
		}
	}()
}

func (c *Controller) DeleteConfigEntry() {

	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to delete %s", cur)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")

			if buttonLabel == "OK" {
				go func() {
					if err := c.params.DeleteConfig(i); err != nil {
						c.error("Failed to delete config", err, false)
						return
					}
					c.fillConfigData()
				}()

			}
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
}

func (c *Controller) ClearSelection() {
	c.clearSelected()
	go c.renderList()
}

// SelectAllVisible selects every file/folder currently shown — i.e. matching
// the active filter, not the whole prefix — so "select all" respects a filter.
func (c *Controller) SelectAllVisible() {
	if c.currentBucket == nil {
		return
	}
	var names []string
	for _, obj := range filterSortObjects(c.objsSnapshot(), c.getFilter()) {
		if obj.Ot == model.File || obj.Ot == model.Folder {
			names = append(names, objKey(obj))
		}
	}
	c.selectNames(names)
	go c.renderList()
}

func (c *Controller) selectedNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	sel := c.selScopeLocked()
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
	if c.currentBucket == nil || c.view.List.GetItemCount() == 0 {
		return "", nil, false
	}
	name := c.getSelectedObjectName()
	if name == "" || name == ".." {
		return "", nil, false
	}
	o, ok := c.lookupObj(name)
	if !ok || (o.Ot != model.File && o.Ot != model.Folder) {
		return "", nil, false
	}
	return name, o, true
}

// Rename renames within the current folder. With two or more marked items it
// opens the batch/pattern form; otherwise it renames the highlighted object.
func (c *Controller) Rename() {
	if c.currentBucket == nil {
		return
	}
	if names := c.selectedNames(); len(names) >= 2 {
		c.batchRename(names)
		return
	}
	c.renameSingle()
}

// renameSingle renames the highlighted object (server-side copy + delete;
// recursive for folders).
func (c *Controller) renameSingle() {
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
		c.view.Pages.RemovePage("modal")

		if newName == "" || newName == short {
			return
		}
		if strings.Contains(newName, "/") {
			go c.error("Invalid name", fmt.Errorf("name must not contain '/'"), false)
			return
		}

		dstKey := c.currentPath + newName
		if isFolder {
			dstKey += "/"
		}

		bucket := c.currentBucket
		go func() {
			if _, err := c.model.MoveKeys(context.Background(), bucket, bucket, srcKey, dstKey, isFolder, nil); err != nil {
				c.error("Rename failed", err, false)
				return
			}
			c.setUndo(&undoOp{
				items: invertOps([]transferPair{{srcBucket: bucket, dstBucket: bucket, srcKey: srcKey, dstKey: dstKey, isFolder: isFolder}}),
				desc:  "rename " + short,
			})
			c.logActivity("renamed %s → %s", short, newName)
			c.restoreNext = dstKey // unique objKey of the renamed item
			c.updateList()
			go c.success("Renamed")
		}()
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 65, 9), true, true)
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
		if strings.Contains(newName, "/") {
			return nil, fmt.Errorf("name must not contain '/': %q", newName)
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
	var items []renameItem
	for _, n := range names {
		o, ok := c.lookupObj(n)
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

	prefix := c.currentPath
	form := c.view.NewBatchRenameForm(fmt.Sprintf("Batch rename %d items", len(items)))
	form.AddButton("Rename", func() {
		pattern := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		find := form.GetFormItem(1).(*tview.InputField).GetText()
		replace := form.GetFormItem(2).(*tview.InputField).GetText()
		c.view.Pages.RemovePage("modal")

		ops, err := planBatchRename(items, prefix, pattern, find, replace)
		if err != nil {
			go c.error("Batch rename", err, false)
			return
		}
		c.runBatchRename(ops)
	})
	form.AddButton("Cancel", func() { c.view.Pages.RemovePage("modal") })
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 70, 11), true, true)
}

// runBatchRename executes the resolved rename operations behind a cancellable
// progress modal (server-side copy + delete per item).
func (c *Controller) runBatchRename(ops []renameOp) {
	ctx, cancel := context.WithCancel(context.Background())
	progress := tview.NewModal().
		SetText("Starting rename...\n").
		AddButtons([]string{"Cancel"}).
		SetDoneFunc(func(_ int, _ string) {
			cancel()
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
		})
	c.view.Pages.AddPage("progress", progress, true, true)

	bucket := c.currentBucket
	go func() {
		var failed []string
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
			n, label := i+1, op.label
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf("Renaming %d/%d\n%s", n, len(ops), label))
			})
			if _, err := c.model.MoveKeys(ctx, bucket, bucket, op.srcKey, op.dstKey, op.isFolder, nil); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", op.label, err))
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
		c.clearSelected()

		c.view.App.QueueUpdateDraw(func() {
			status := "Rename complete."
			if len(failed) > 0 {
				status = "Rename finished with errors."
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
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			})
			c.view.App.SetFocus(progress)
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
}

// clipboard holds objects yanked (copy) or cut (move) for a later paste.
type clipboard struct {
	op     string // "copy" or "cut"; "" = empty
	bucket *model.Object
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
	if c.currentBucket == nil || c.view.List.GetItemCount() == 0 {
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
		o, ok := c.lookupObj(n)
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

	srcBucketName := *c.currentBucket.Key
	srcPrefix := c.currentPath

	// In dual-pane mode the destination defaults to the OTHER pane's location
	// (the Midnight-Commander copy-to-other-panel workflow); otherwise it
	// defaults to the source.
	dstBucketName := srcBucketName
	dstPrefix := srcPrefix
	if c.dual {
		if other := c.panes[1-c.active]; other.currentBucket != nil {
			dstBucketName = *other.currentBucket.Key
			dstPrefix = other.currentPath
		}
	}

	// List buckets so the destination dropdown offers cross-bucket targets. Off
	// the UI goroutine (network); if it fails, fall back to the current bucket
	// only so same-bucket copy/move keeps working.
	go func() {
		bucketNames := []string{srcBucketName}
		if list, err := c.model.ListBuckets(); err == nil {
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

		c.view.App.QueueUpdateDraw(func() {
			form := c.view.NewCopyMoveForm(title, bucketNames, dstBucketName, dstPrefix)
			form.AddButton(title, func() {
				_, dstBucketName := form.GetFormItem(0).(*tview.DropDown).GetCurrentOption()
				dstPrefix := strings.TrimSpace(form.GetFormItem(1).(*tview.InputField).GetText())
				c.view.Pages.RemovePage("modal")

				dstPrefix = filepath.ToSlash(dstPrefix)
				if dstPrefix != "" && !strings.HasSuffix(dstPrefix, "/") {
					dstPrefix += "/"
				}
				sameBucket := dstBucketName == srcBucketName
				if sameBucket && dstPrefix == srcPrefix {
					go c.error(title+" failed", fmt.Errorf("destination equals source"), false)
					return
				}

				dstBucket := c.currentBucket
				if !sameBucket {
					bn := dstBucketName
					dstBucket = &model.Object{Key: &bn, Ot: model.Bucket}
				}
				c.runCopyOrMove(isMove, title, items, c.currentBucket, dstBucket, dstBucketName, dstPrefix)
			})
			form.AddButton("Cancel", func() {
				c.view.Pages.RemovePage("modal")
			})
			c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 75, 11), true, true)
		})
	}()
}

// runCopyOrMove executes the resolved copy/move against dstBucket, showing a
// cancellable progress modal with per-item object rate + ETA.
func (c *Controller) runCopyOrMove(isMove bool, title string, items []copyMoveItem, srcBucket, dstBucket *model.Object, dstBucketName, dstPrefix string) {
	ctx, cancel := context.WithCancel(context.Background())
	progress := tview.NewModal().
		SetText(fmt.Sprintf("Starting %s...\n", strings.ToLower(title))).
		AddButtons([]string{"Cancel"}).
		SetDoneFunc(func(_ int, _ string) {
			cancel()
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
		})
	c.view.Pages.AddPage("progress", progress, true, true)

	go func() {
		var failed []string
		var moved []transferPair
		okCount := 0
		canceled := false

	loop:
		for _, it := range items {
			select {
			case <-ctx.Done():
				canceled = true
				break loop
			default:
			}

			srcKey := it.srcKey
			dstKey := dstPrefix + it.shortName
			if it.isFolder {
				dstKey += "/"
			}

			itemStart := time.Now()
			cb := func(done, total int, key string) {
				line := objRateETA(done, total, time.Since(itemStart))
				c.view.App.QueueUpdateDraw(func() {
					progress.SetText(fmt.Sprintf("%s: %s → %s\n%d/%d object(s)\n%s\n%s",
						title, it.shortName, dstBucketName, done, total, line, key))
				})
			}

			var err error
			if isMove {
				_, err = c.model.MoveKeys(ctx, srcBucket, dstBucket, srcKey, dstKey, it.isFolder, cb)
			} else {
				_, err = c.model.CopyKeys(ctx, srcBucket, dstBucket, srcKey, dstKey, it.isFolder, cb)
			}
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", it.shortName, err))
				continue
			}
			okCount++
			if isMove {
				moved = append(moved, transferPair{srcBucket: srcBucket, dstBucket: dstBucket, srcKey: srcKey, dstKey: dstKey, isFolder: it.isFolder})
			}
		}

		if isMove && len(moved) > 0 {
			c.setUndo(&undoOp{items: invertOps(moved), desc: fmt.Sprintf("%s of %d item(s)", title, len(moved))})
		}
		c.logActivity("%s: %d ok, %d failed → %s/%s", title, okCount, len(failed), dstBucketName, dstPrefix)

		if canceled {
			// progress page already removed by the Cancel handler.
			c.updateList()
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			status := title + " complete."
			if len(failed) > 0 {
				status = title + " finished with errors."
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
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			})
			c.view.App.SetFocus(progress)
		})

		if isMove {
			c.clearSelected()
		}
		c.updateList()
	}()
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
	if c.currentBucket == nil {
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
		if o, ok := c.lookupObj(n); ok {
			objs = append(objs, o)
		}
	}
	items := clipItems(objs)
	if len(items) == 0 {
		return
	}
	c.clip = clipboard{op: op, bucket: c.currentBucket, items: items}
	verb := "Copied"
	if op == "cut" {
		verb = "Cut"
	}
	go c.success(fmt.Sprintf("%s %d item(s) — press p to paste", verb, len(items)))
}

// paste copies (or moves, for a cut) the clipboard into the current location,
// reusing the copy/move runner with the clipboard's origin as the source.
func (c *Controller) paste() {
	if c.currentBucket == nil || c.clip.bucket == nil || len(c.clip.items) == 0 {
		return
	}
	isMove := c.clip.op == "cut"
	dstBucketName := *c.currentBucket.Key
	c.runCopyOrMove(isMove, "Paste", c.clip.items, c.clip.bucket, c.currentBucket, dstBucketName, c.currentPath)
	if isMove {
		c.clip = clipboard{} // a cut is consumed by the paste
	}
}

// Undo reverses the last move/rename (one step) after a confirmation.
func (c *Controller) Undo() {
	op := c.takeUndo()
	if op == nil {
		go c.success("Nothing to undo")
		return
	}
	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Undo %s?", op.desc)).
		SetDoneFunc(func(_ int, label string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")
			if label != "OK" {
				return
			}
			c.runUndo(op)
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
}

// runUndo applies the reverse moves behind a cancellable progress modal.
func (c *Controller) runUndo(op *undoOp) {
	ctx, cancel := context.WithCancel(context.Background())
	progress := tview.NewModal().
		SetText("Undoing...\n").
		AddButtons([]string{"Cancel"}).
		SetDoneFunc(func(_ int, _ string) {
			cancel()
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
		})
	c.view.Pages.AddPage("progress", progress, true, true)

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
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf("Undoing %d/%d\n%s", n, len(op.items), line))
			})
			if _, err := c.model.MoveKeys(ctx, it.srcBucket, it.dstBucket, it.srcKey, it.dstKey, it.isFolder, nil); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", it.srcKey, err))
				continue
			}
			okCount++
		}
		c.setUndo(nil) // one-step undo, now consumed
		c.logActivity("undo %s: %d ok, %d failed", op.desc, okCount, len(failed))

		c.view.App.QueueUpdateDraw(func() {
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
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			})
			c.view.App.SetFocus(progress)
		})
		c.updateList()
	}()
}

func (c *Controller) setConfigInput() {
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		case tcell.KeyDelete:
			c.DeleteConfigEntry()
			return nil
		}
		return event
	})

	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlN:
			c.CreateConfigEntry()
			return nil
		case tcell.KeyCtrlE:
			c.EditConfigEntry()
			return nil
		case tcell.KeyCtrlY:
			c.CopyProfile()
			return nil
		case tcell.KeyCtrlH:
			help := c.view.HotkeysModal(true)

			// Close help on any key press inside the help view
			help.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-help")
				return nil
			})

			c.view.Pages.AddPage("modal-help", c.view.ModalEdit(help, 70, 20), true, true)
			return nil
		case tcell.KeyCtrlV:
			c.CheckProfile()
			return nil
		case tcell.KeyCtrlA:
			about := c.view.AboutModal()
			about.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-about")
				return nil
			})
			c.view.Pages.AddPage("modal-about", c.view.ModalEdit(about, 70, 19), true, true)
			return nil

		}
		return event
	})
}

// ShowSummary keeps backward compatibility if some places call ShowSummary().
func (c *Controller) ShowSummary() { c.ShowSummaryModal() }

// ShowSummaryModal opens graphical bucket/folder summary.
func (c *Controller) ShowSummaryModal() {
	// Determine scope
	var bucket *model.Object
	prefix := c.currentPath

	if c.currentBucket == nil {
		cur := c.getSelectedObjectName()
		if cur == "" {
			return
		}
		if val, ok := c.lookupObj(cur); ok && val != nil && val.Ot == model.Bucket {
			bucket = val
		} else {
			tmp := cur
			bucket = &model.Object{Key: &tmp, Ot: model.Bucket}
		}
		prefix = ""
	} else {
		bucket = c.currentBucket

		// If a folder is selected, summarize that folder; else summarize current path.
		cur := c.getSelectedObjectName()
		if cur != "" {
			if val, ok := c.lookupObj(cur); ok && val != nil && val.Ot == model.Folder {
				prefix = *val.FullPath // full prefix, already ends with "/"
			}
		}
	}

	if bucket == nil || bucket.Key == nil {
		return
	}

	c.showSummaryModalFor(bucket, prefix)
}

func (c *Controller) showSummaryModalFor(bucket *model.Object, prefix string) {
	// Normalize prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	scopeLabel := *bucket.Key
	if prefix != "" {
		scopeLabel = fmt.Sprintf("%s/%s", *bucket.Key, strings.TrimSuffix(prefix, "/"))
	}

	go func() {
		objects, err := c.model.ListObjects(prefix, bucket)
		if err != nil {
			c.error("Failed to build summary", err, false)
			return
		}

		total, catRows, groupRows := buildSummary(objects, prefix)

		c.view.App.QueueUpdateDraw(func() {
			graph := c.view.NewSummaryGraph(" Summary ", scopeLabel, total, catRows, groupRows, func(groupName string) {
				if groupName == "" || groupName == "(root)" {
					return
				}
				if !strings.HasSuffix(groupName, "/") {
					return
				}
				nextPrefix := prefix + groupName
				c.view.Pages.RemovePage("modal-summary")
				c.showSummaryModalFor(bucket, nextPrefix)
			})

			// Global keys for modal
			if prim, ok := graph.Root.(interface {
				SetInputCapture(func(*tcell.EventKey) *tcell.EventKey) *tview.Box
			}); ok {
				_ = prim
			}

			wrapper := tview.NewFlex().SetDirection(tview.FlexRow)
			wrapper.AddItem(graph.Root, 0, 1, true)
			wrapper.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
				switch ev.Key() {
				case tcell.KeyEsc:
					c.view.Pages.RemovePage("modal-summary")
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
					c.view.Pages.RemovePage("modal-summary")
					return nil
				}
				return ev
			})

			c.view.Pages.AddPage("modal-summary", c.view.ModalEdit(wrapper, 100, 28), true, true)
			c.view.App.SetFocus(graph.Cats)
		})
	}()
}

func buildSummary(objs []s3t.Object, prefix string) (total int64, cats []view.SummaryRow, groups []view.SummaryRow) {
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

	cats = []view.SummaryRow{
		{Name: "Documents", Bytes: docs},
		{Name: "Archives", Bytes: arch},
		{Name: "Media", Bytes: media},
		{Name: "Other", Bytes: other},
	}

	for name, b := range groupBytes {
		groups = append(groups, view.SummaryRow{Name: name, Bytes: b})
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Bytes == groups[j].Bytes {
			return groups[i].Name < groups[j].Name
		}
		return groups[i].Bytes > groups[j].Bytes
	})

	// Top 10
	if len(groups) > 10 {
		groups = groups[:10]
	}

	return total, cats, groups
}

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

func (c *Controller) setInput() {
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

// listInputCapture is the shared browser-pane key handler.
func (c *Controller) listInputCapture(event *tcell.EventKey) *tcell.EventKey {
	switch event.Key() {
	case tcell.KeyTab:
		if c.dual {
			c.swapAndFocus()
		}
		return nil
	case tcell.KeyCtrlO:
		c.ToggleDualPane()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case ' ': // Space
			c.ToggleSelectCurrent()
			return nil
		case '/':
			c.focusFilter()
			return nil
		case '[':
			c.HistoryBack()
			return nil
		case ']':
			c.HistoryForward()
			return nil
		case 'y':
			c.yank("copy")
			return nil
		case 'x':
			c.yank("cut")
			return nil
		case 'p':
			c.paste()
			return nil
		case 'u':
			c.Undo()
			return nil
		case 't':
			c.ShowTransfers()
			return nil
		}
	case tcell.KeyLeft:
		if event.Modifiers()&tcell.ModAlt != 0 {
			c.HistoryBack()
			return nil
		}
	case tcell.KeyRight:
		if event.Modifiers()&tcell.ModAlt != 0 {
			c.HistoryForward()
			return nil
		}
	case tcell.KeyCtrlF:
		c.RecursiveSearch()
		return nil
	case tcell.KeyCtrlB:
		c.Bookmarks()
		return nil
	case tcell.KeyCtrlK:
		c.CommandPalette()
		return nil
	case tcell.KeyDelete:
		c.Delete()
		return nil
	case tcell.KeyBackspace2:
		c.Up()
		return nil
	case tcell.KeyCtrlN:
		c.Create()
		return nil
	case tcell.KeyCtrlD:
		c.Download()
		return nil
	case tcell.KeyCtrlP:
		c.Profiles()
		return nil
	case tcell.KeyCtrlL:
		c.ShowFileProperties(c.getSelectedObjectName())
		return nil
	case tcell.KeyCtrlW:
		c.PresignLink(c.getSelectedObjectName())
		return nil
	case tcell.KeyCtrlU:
		c.ShowLocalFSModal(c.params.HomeDir)
		return nil
	case tcell.KeyCtrlG:
		c.ShowSummaryModal()
		return nil
	case tcell.KeyCtrlX:
		c.ClearSelection()
		return nil
	case tcell.KeyCtrlS:
		c.SelectAllVisible()
		return nil
	case tcell.KeyCtrlR:
		c.Rename()
		return nil
	case tcell.KeyCtrlY:
		c.copyOrMove(false)
		return nil
	case tcell.KeyCtrlT:
		c.copyOrMove(true)
		return nil
	case tcell.KeyCtrlH:
		help := c.view.HotkeysModal(false)
		help.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
			c.view.Pages.RemovePage("modal-help")
			return nil
		})
		c.view.Pages.AddPage("modal-help", c.view.ModalEdit(help, 74, 40), true, true)
		return nil
	case tcell.KeyCtrlA:
		about := c.view.AboutModal()
		about.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
			c.view.Pages.RemovePage("modal-about")
			return nil
		})
		c.view.Pages.AddPage("modal-about", c.view.ModalEdit(about, 70, 19), true, true)
		return nil
	}
	return event
}

// swapPane snapshots the active pane into panes[active], loads the other pane,
// and repoints the active-widget pointers. It performs no rendering.
func (c *Controller) swapPane() {
	c.mu.Lock()
	c.panes[c.active] = paneState{
		buckets:         c.buckets,
		objs:            c.objs,
		currentPath:     c.currentPath,
		currentBucket:   c.currentBucket,
		bucketPos:       c.bucketPos,
		restoreNext:     c.restoreNext,
		filter:          c.filter,
		selectedByScope: c.selectedByScope,
		hist:            c.hist,
	}
	c.active ^= 1
	p := c.panes[c.active]
	c.buckets = p.buckets
	c.objs = p.objs
	c.currentPath = p.currentPath
	c.currentBucket = p.currentBucket
	c.bucketPos = p.bucketPos
	c.restoreNext = p.restoreNext
	c.filter = p.filter
	c.selectedByScope = p.selectedByScope
	c.hist = p.hist
	c.mu.Unlock()

	c.view.List = c.view.PaneList(c.active)
	c.view.Filter = c.view.PaneFilter(c.active)
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
		c.panes[1].currentBucket = c.currentBucket
		c.panes[1].currentPath = c.currentPath
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
	c.mu.Lock()
	c.selectedByScope = make(map[string]map[string]bool)
	c.filter = ""
	c.mu.Unlock()
	c.hist = histStack{}
	c.panes[0] = paneState{}
	c.panes[1] = paneState{selectedByScope: make(map[string]map[string]bool)}
	c.view.ShowSinglePane()
	c.filterSuppress = true
	c.view.PaneFilter(0).SetText("")
	c.view.PaneFilter(1).SetText("")
	c.filterSuppress = false
}

func (c *Controller) ConfigEntryByName(name string) *cfg.Config {
	for _, v := range c.params.Config {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func (c *Controller) fillConfigDetails(cur string) {
	c.view.Details.Clear()
	item := c.ConfigEntryByName(cur)

	if item != nil {
		fmt.Fprintf(c.view.Details, "[green] Config: [white]%s\n", item.Name)
		fmt.Fprintf(c.view.Details, "[blue] Url: [gray] %s\n", item.BaseUrl)
		if item.Region != nil {
			fmt.Fprintf(c.view.Details, "[blue] Region: [white] %s\n", *item.Region)
		}
		fmt.Fprintf(c.view.Details, "[blue] Ssl: [white] %v\n", !item.IgnoreSsl)
	}
}

func (c *Controller) fillConfigData() {
	c.view.Details.Clear()
	c.view.List.Clear()
	c.view.List.SetTitle("(profiles)")

	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		c.fillConfigDetails(cur)
	})

	for _, cf := range c.params.Config {
		c.view.List.AddItem(cf.Name, cf.Name, 0, func() {
			i := c.view.List.GetCurrentItem()
			conf := c.params.Config[i]
			c.activeConfig = conf
			c.Duck(conf.BaseUrl, conf.Region, conf.AccessKey, conf.SecretKey, !conf.IgnoreSsl)
		})
	}
	c.view.SetFrameText("[::b][↓,↑][::-]Down/Up [::b][Enter[][::-]Use [::b][Ctrl+N[][::-]New [::b][Ctrl+Y[][::-]Yank(Copy) [::b][Ctrl+E[][::-]Edit [::b][Ctrl+V[][::-]Verify [::b][Del[][::-]Delete [::b][Ctrl+H[][::-]Hotkeys [::b][Ctrl+Q][::-]Quit")
}

func (c *Controller) fillDetails(key string) {
	c.view.Details.Clear()
	var otype string
	if val, ok := c.lookupObj(key); ok {
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

func (c *Controller) Duck(url string, region *string, acc string, sec string, ssl bool) {
	var maxBps int64
	if c.activeConfig != nil {
		maxBps = c.activeConfig.MaxBytesPerSec
	}
	mCf := model.NewConfig(url, region, acc, sec, ssl, maxBps)
	c.model = model.NewModel(mCf)
	c.resetPanes() // fresh single-pane browser for this profile
	c.wireListChanged(c.view.PaneList(0))
	c.currentBucket = nil
	c.currentPath = ""
	c.bucketPos = 0
	go func() {
		if err := c.updateList(); err == nil {
			c.setInput()
		}
	}()
}

func (c *Controller) Run() error {
	c.Profiles()
	// A missing/corrupt config no longer crashes startup; surface it as a
	// modal over the (empty) profiles screen so the user can recover.
	if c.params.LoadErr != nil {
		errMsg := c.view.NewErrorMessageQ("Config error", c.params.LoadErr.Error())
		errMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("modal")
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(errMsg, 8, 3), true, true)
	}
	return c.view.App.Run()
}

func (c *Controller) error(header string, err error, fatal bool) {
	errMsg := c.view.NewErrorMessageQ(header, err.Error())
	errMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
	})

	// Call QueueUpdateDraw ONLY for the update
	c.view.App.QueueUpdateDraw(func() {
		c.view.Pages.AddPage("modal", c.view.ModalEdit(errMsg, 8, 3), true, true)
	})
}

func (c *Controller) success(header string) {
	succMsg := c.view.NewSuccessMessageQ(header)
	succMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
	})

	// Same here: just view update
	c.view.App.QueueUpdateDraw(func() {
		c.view.Pages.AddPage("modal", c.view.ModalEdit(succMsg, 8, 3), true, true)
	})
}

// chooseDir opens an icon-styled directory browser (same look as the upload
// browser) and calls onChosen with the directory the user picks. Used as the
// download-destination fallback when the active profile has no DownloadDir.
func (c *Controller) chooseDir(startPath string, onChosen func(dir string)) {
	currentPath := startPath
	layout, localList := c.view.NewCreateLocalFileListForm()
	app := c.view.App

	selectBtn := tview.NewButton("Select here").SetSelectedFunc(func() {
		dir := currentPath
		if !strings.HasSuffix(dir, string(os.PathSeparator)) {
			dir += string(os.PathSeparator)
		}
		c.view.Pages.RemovePage("modal")
		onChosen(dir)
	})
	cancelBtn := tview.NewButton("Cancel").SetSelectedFunc(func() {
		c.view.Pages.RemovePage("modal")
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
			go c.error("Failed to read directory", err, false)
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
			c.view.Pages.RemovePage("modal")
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
	c.view.Pages.AddPage("modal", modal, true, true)
	renderList(startPath)
}

func (c *Controller) ShowLocalFSModal(startPath string) {
	if c.currentBucket == nil {
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

		c.view.Pages.RemovePage("modal")
		err := c.Upload(fullPath)
		if err != nil {
			go c.error("Upload failed", err, false)
		}
	})

	cancelBtn := tview.NewButton("Cancel").SetSelectedFunc(func() {
		c.view.Pages.RemovePage("modal")
	})

	buttonRow := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(okBtn, 0, 1, false).
		AddItem(tview.NewBox(), 2, 0, false).
		AddItem(cancelBtn, 0, 1, false)

	flex, _ := layout.(*tview.Flex)
	flex.AddItem(buttonRow, 1, 0, false)

	// Maintain focusable order
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
			go c.error("Failed to read directory", err, false)
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
			c.view.Pages.RemovePage("modal")
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
	c.view.Pages.AddPage("modal", modal, true, true)
	renderList(startPath)
}

func (c *Controller) Upload(localPath string) error {
	ctx, cancel := context.WithCancel(context.Background())

	files, totalSize, err := c.model.PrepareUpload(localPath, c.currentPath, c.currentBucket)
	if err != nil || len(files) == 0 {
		// Upload() runs on the tview UI goroutine (invoked from the local-FS
		// modal's button). c.error() calls QueueUpdateDraw, which blocks until
		// the UI goroutine drains the queue — calling it inline here would
		// deadlock the event loop and freeze the whole app (e.g. when the user
		// picks an empty directory). Dispatch it on its own goroutine, and
		// release the unused context created above.
		cancel()
		if err == nil {
			err = fmt.Errorf("nothing to upload")
		}
		go c.error("Upload failed", err, false)
		return nil
	}

	first := files[0]
	job := c.addJob("upload", fmt.Sprintf("%s → %s/%s", filepath.Base(localPath), *c.currentBucket.Key, c.currentPath), totalSize, len(files), cancel)

	progress := tview.NewModal().
		SetText("Starting upload...\n").
		AddButtons([]string{"Background", "Cancel"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			switch buttonLabel {
			case "Background":
				job.setBackgrounded()
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
				c.view.App.SetFocus(c.view.List)
			default: // Cancel
				cancel()
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			}
		})
	c.view.Pages.AddPage("progress", progress, true, true)
	progress.SetText(fmt.Sprintf(
		"Uploading\n0/%d file(s)\n0B/%s (0.0%%)\n-- · ETA --\nLast: %s\n-> %s",
		len(files),
		humanize.IBytes(uint64(totalSize)),
		first.LocalPath,
		first.RemotePath,
	))

	var lastDraw time.Time
	throttle := 100 * time.Millisecond
	startTime := time.Now()

	go func() {
		// Cap concurrent byte transfers at 2 (bandwidth capped by model.Limiter).
		job.setStatus(jobQueued)
		select {
		case c.jobSem <- struct{}{}:
			defer func() { <-c.jobSem }()
		case <-ctx.Done():
			c.finalizeJob(job, true, 0)
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress").SwitchToPage("main") })
			go c.updateList()
			return
		}
		job.setStatus(jobRunning)
		startTime = time.Now()

		err := c.model.Upload(ctx, localPath, c.currentPath, c.currentBucket, func(n, total int64, i, count int, local, remote string) {
			job.setProgress(n, i)
			select {
			case <-ctx.Done():
				return
			default:
			}
			if job.isBackgrounded() {
				return
			}

			now := time.Now()
			if now.Sub(lastDraw) < throttle {
				return
			}
			lastDraw = now

			percentage := 0.0
			if total > 0 {
				percentage = float64(n) / float64(total) * 100
			}
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf(
					"Uploading\n%d/%d file(s)\n%s/%s (%.1f%%)\n%s\nLast: %s\n-> %s",
					i, count,
					humanize.IBytes(uint64(n)),
					humanize.IBytes(uint64(total)),
					percentage,
					byteRateETA(n, total, time.Since(startTime)),
					local,
					remote,
				))
			})
		})

		select {
		case <-ctx.Done():
			c.finalizeJob(job, true, 0)
			go c.updateList()
			return
		default:
			if err != nil {
				c.finalizeJob(job, false, 1)
				if !job.isBackgrounded() {
					c.view.App.QueueUpdateDraw(func() {
						c.view.Pages.RemovePage("progress").SwitchToPage("main")
					})
				}
				c.error("Upload failed", err, false)
				return
			}

			c.finalizeJob(job, false, 0)
			if job.isBackgrounded() {
				go c.updateList()
				return
			}
			c.view.App.QueueUpdateDraw(func() {
				progress.ClearButtons()
				progress.SetText("Upload complete.\n\nPress Done to return.")
				progress.AddButtons([]string{"Done"})
				progress.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
					c.view.Pages.RemovePage("progress").SwitchToPage("main")
					go c.updateList()
				})
				c.view.App.SetFocus(progress)
			})
		}
	}()

	return nil
}

func (c *Controller) ShowFileProperties(key string) {
	obj, ok := c.lookupObj(key)
	if !ok || obj.Ot != model.File {
		return
	}

	bucketName := *c.currentBucket.Key
	fullPath := *obj.FullPath

	var url string
	if strings.Contains(c.model.Cf.Url, "amazonaws.com") && c.model.Cf.Region != nil {
		url = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucketName, *c.model.Cf.Region, fullPath)
	} else {
		url = fmt.Sprintf("%s/%s/%s", c.model.Cf.Url, bucketName, fullPath)
	}

	text := fmt.Sprintf(
		"[black]Name: [white]%s\n[black]Size: [white]%s\n[black]Modified: [white]%v\n[black]Etag: [white]%s\n[black]Link: [white]%s",
		*obj.Key,
		humanize.IBytes(uint64(*obj.Size)),
		obj.LastModified,
		*obj.Etag,
		url,
	)

	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"Copy Link", "Presign Link", "Close"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("modal")
			switch buttonLabel {
			case "Copy Link":
				u.CopyToClipboard(url)
				go c.success("Link copied to clipboard")
			case "Presign Link":
				c.PresignLink(key)
			}
		})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(modal, 75, 12), true, true)
}

// PresignLink prompts for an expiry and copies a presigned GET URL for the
// selected object to the clipboard. Unlike the naive public link, this works
// for private buckets.
func (c *Controller) PresignLink(key string) {
	obj, ok := c.lookupObj(key)
	if !ok || obj.Ot != model.File {
		return
	}
	if obj.FullPath == nil {
		go c.error("Presign", fmt.Errorf("object has no path"), false)
		return
	}
	bucket := c.currentBucket
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

	modal := tview.NewModal().
		SetText(fmt.Sprintf("Presigned link expiry for:\n%s", *obj.Key)).
		AddButtons(labels).
		SetDoneFunc(func(_ int, buttonLabel string) {
			c.view.Pages.RemovePage("modal-presign")
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
					c.error("Presign failed", err, false)
					return
				}
				u.CopyToClipboard(link)
				c.success("Presigned link copied to clipboard")
			}()
		})

	c.view.Pages.RemovePage("modal") // dismiss any open properties modal before stacking presign
	c.view.Pages.AddPage("modal-presign", c.view.ModalEdit(modal, 75, 12), true, true)
}

func (c *Controller) clearDetailsIfNoSelection() {
	c.view.App.QueueUpdateDraw(func() {
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

// searchMatch reports whether a full object key matches query (case-insensitive
// substring). Folder-marker keys (ending in "/") never match; search targets
// real objects.
func searchMatch(key, query string) bool {
	if key == "" || strings.HasSuffix(key, "/") {
		return false
	}
	return strings.Contains(strings.ToLower(key), strings.ToLower(query))
}

// computeHits filters a recursive object listing to those matching query,
// returning at most max hits and whether more were dropped (max <= 0: no cap).
func computeHits(objs []s3t.Object, query string, max int) (hits []searchHit, truncated bool) {
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		k := *o.Key
		if !searchMatch(k, query) {
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
	if c.currentBucket == nil {
		return // only meaningful inside a bucket
	}
	bucket := c.currentBucket
	prefix := c.currentPath

	form := c.view.NewSearchForm("Recursive search")
	form.AddButton("Search", func() {
		q := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		allBuckets := form.GetFormItem(1).(*tview.Checkbox).IsChecked()
		c.view.Pages.RemovePage("modal")
		if q == "" {
			return
		}
		if allBuckets {
			c.runAllBucketsSearch(q)
			return
		}
		c.runRecursiveSearch(bucket, prefix, q)
	})
	form.AddButton("Cancel", func() { c.view.Pages.RemovePage("modal") })
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 65, 9), true, true)
}

// runAllBucketsSearch lists every bucket recursively (off the UI goroutine) and
// collects matches tagged with their bucket, capped at searchMaxResults total.
// Buckets that can't be listed (e.g. a different region on the shared client)
// are skipped.
func (c *Controller) runAllBucketsSearch(query string) {
	searching := tview.NewModal().SetText(fmt.Sprintf("Searching all buckets for %q ...", query))
	c.view.Pages.AddPage("searching", searching, true, true)

	go func() {
		buckets, err := c.model.ListBuckets()
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("searching") })
			c.error("Search failed", err, false)
			return
		}
		var hits []searchHit
		truncated := false
		for _, b := range buckets {
			if b == nil || b.Key == nil {
				continue
			}
			if len(hits) >= searchMaxResults {
				truncated = true
				break
			}
			objs, err := c.model.ListObjects("", b)
			if err != nil {
				continue // skip buckets we can't list (region/permission)
			}
			bh, tr := computeHits(objs, query, searchMaxResults-len(hits))
			for _, h := range bh {
				hits = append(hits, searchHit{bucket: *b.Key, key: h.key, size: h.size})
			}
			if tr {
				truncated = true
			}
		}
		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("searching")
			c.presentSearchResults(query, hits, truncated)
		})
	}()
}

// runRecursiveSearch lists the prefix recursively off the UI goroutine, then
// shows the matches (or an error / "no matches" note).
func (c *Controller) runRecursiveSearch(bucket *model.Object, prefix, query string) {
	searching := tview.NewModal().SetText(fmt.Sprintf("Searching for %q ...", query))
	c.view.Pages.AddPage("searching", searching, true, true)

	go func() {
		objs, err := c.model.ListObjects(prefix, bucket)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("searching") })
			c.error("Search failed", err, false)
			return
		}
		hits, truncated := computeHits(objs, query, searchMaxResults)
		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("searching")
			c.presentSearchResults(query, hits, truncated)
		})
	}()
}

// presentSearchResults builds the results list (or a "no matches" note) and
// shows it. Must run on the UI goroutine.
func (c *Controller) presentSearchResults(query string, hits []searchHit, truncated bool) {
	if len(hits) == 0 {
		m := tview.NewModal().
			SetText(fmt.Sprintf("No matches for %q", query)).
			AddButtons([]string{"OK"})
		m.SetDoneFunc(func(int, string) {
			c.view.Pages.RemovePage("search-results")
			c.view.App.SetFocus(c.view.List)
		})
		c.view.Pages.AddPage("search-results", c.view.ModalEdit(m, 50, 8), true, true)
		return
	}

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
			c.view.Pages.RemovePage("search-results")
			c.view.App.SetFocus(c.view.List)
			if h.bucket != "" && (c.currentBucket == nil || h.bucket != *c.currentBucket.Key) {
				c.jumpTo(h.bucket, parentPrefix(h.key), h.key)
			} else {
				c.revealKey(h.key)
			}
		})
	}
	results.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage("search-results")
			c.view.App.SetFocus(c.view.List)
			return nil
		}
		return event
	})

	c.view.Pages.AddPage("search-results", c.view.ModalEdit(results, 100, 30), true, true)
	c.view.App.SetFocus(results)
}

// revealKey navigates to the folder containing key and highlights it. key is a
// full object key inside the current bucket (from a search result).
func (c *Controller) revealKey(key string) {
	c.clearFilterUI() // ensure the revealed item isn't hidden by an old filter
	c.recordHistory()
	c.currentPath = parentPrefix(key)
	c.restoreNext = key
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
	go func() {
		bucket := c.findBucketByName(bucketName)
		if bucket == nil {
			c.error("Jump", fmt.Errorf("bucket %q not found", bucketName), false)
			return
		}
		c.currentBucket = bucket
		c.model.RefreshClient(&bucketName)
		c.currentPath = prefix
		if selectKey != "" {
			c.restoreNext = selectKey
		} else {
			c.restoreNext = ".."
		}
		c.updateList()
		c.view.App.QueueUpdateDraw(func() { c.view.App.SetFocus(c.view.List) })
	}()
}

// Bookmarks opens the per-profile bookmark manager: Enter jumps to a bookmark,
// the first row adds the current location, Del removes, Esc closes.
func (c *Controller) Bookmarks() {
	if c.activeConfig == nil {
		return
	}
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(" Bookmarks — Enter: go • Del: remove • Esc: close ")
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)
	c.rebuildBookmarkList(list)

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			c.view.Pages.RemovePage("modal-bookmarks")
			c.view.App.SetFocus(c.view.List)
			return nil
		case tcell.KeyDelete:
			c.removeBookmarkAt(list)
			return nil
		}
		return ev
	})

	c.view.Pages.AddPage("modal-bookmarks", c.view.ModalEdit(list, 80, 24), true, true)
	c.view.App.SetFocus(list)
}

// bookmarkOffset is the number of leading non-bookmark rows in the manager list
// (the "add current" row, shown only when inside a bucket).
func (c *Controller) bookmarkOffset() int {
	if c.currentBucket != nil {
		return 1
	}
	return 0
}

func (c *Controller) rebuildBookmarkList(list *tview.List) {
	list.Clear()
	if c.currentBucket != nil {
		list.AddItem("[green]＋ Add current location[-]", "", 0, func() {
			c.addCurrentBookmark(list)
		})
	}
	for _, bm := range c.activeConfig.Bookmarks {
		bm := bm
		list.AddItem("★ "+bm.Name, "", 0, func() {
			c.view.Pages.RemovePage("modal-bookmarks")
			c.jumpTo(bm.Bucket, bm.Prefix, "")
		})
	}
	if len(c.activeConfig.Bookmarks) == 0 && c.currentBucket == nil {
		list.AddItem("[gray](no bookmarks)[-]", "", 0, func() {})
	}
}

func (c *Controller) addCurrentBookmark(list *tview.List) {
	if c.currentBucket == nil {
		return
	}
	name := *c.currentBucket.Key + "/" + c.currentPath
	bm := cfg.Bookmark{Name: name, Bucket: *c.currentBucket.Key, Prefix: c.currentPath}
	newList, added := addBookmark(c.activeConfig.Bookmarks, bm)
	if !added {
		go c.error("Bookmark", fmt.Errorf("this location is already bookmarked"), false)
		return
	}
	c.activeConfig.Bookmarks = newList
	if err := c.params.WriteConfig(); err != nil {
		go c.error("Bookmark", err, false)
		return
	}
	c.rebuildBookmarkList(list)
}

func (c *Controller) removeBookmarkAt(list *tview.List) {
	idx := list.GetCurrentItem() - c.bookmarkOffset()
	if idx < 0 || idx >= len(c.activeConfig.Bookmarks) {
		return
	}
	c.activeConfig.Bookmarks = append(c.activeConfig.Bookmarks[:idx], c.activeConfig.Bookmarks[idx+1:]...)
	if err := c.params.WriteConfig(); err != nil {
		go c.error("Bookmark", err, false)
		return
	}
	c.rebuildBookmarkList(list)
}

// location is a navigable position: a bucket (nil = the buckets screen) and a
// prefix within it.
type location struct {
	bucket *model.Object
	path   string
}

// histStack is a browser-style back/forward history. The "current" location
// lives on the controller; the stacks hold where you came from (back) and where
// you returned from (fwd).
type histStack struct {
	back []location
	fwd  []location
}

const histMax = 100

// record pushes cur onto the back stack and starts a new branch (clears fwd).
func (h *histStack) record(cur location) {
	h.back = append(h.back, cur)
	if len(h.back) > histMax {
		h.back = h.back[len(h.back)-histMax:]
	}
	h.fwd = nil
}

// goBack returns the previous location (and true), moving cur onto the fwd
// stack. ok is false when there is nowhere to go back to.
func (h *histStack) goBack(cur location) (location, bool) {
	if len(h.back) == 0 {
		return location{}, false
	}
	prev := h.back[len(h.back)-1]
	h.back = h.back[:len(h.back)-1]
	h.fwd = append(h.fwd, cur)
	return prev, true
}

// goForward is goBack's inverse.
func (h *histStack) goForward(cur location) (location, bool) {
	if len(h.fwd) == 0 {
		return location{}, false
	}
	next := h.fwd[len(h.fwd)-1]
	h.fwd = h.fwd[:len(h.fwd)-1]
	h.back = append(h.back, cur)
	return next, true
}

func (c *Controller) curLocation() location {
	return location{bucket: c.currentBucket, path: c.currentPath}
}

// recordHistory pushes the current location so a later Back returns to it.
// Called at the start of user-initiated navigation, before state changes.
func (c *Controller) recordHistory() {
	c.hist.record(c.curLocation())
}

// HistoryBack / HistoryForward move through the navigation history.
func (c *Controller) HistoryBack() {
	if loc, ok := c.hist.goBack(c.curLocation()); ok {
		c.navigateTo(loc)
	}
}

func (c *Controller) HistoryForward() {
	if loc, ok := c.hist.goForward(c.curLocation()); ok {
		c.navigateTo(loc)
	}
}

// navigateTo jumps straight to loc without touching the history stacks (the
// history machinery drives it). Mirrors Down/jumpTo's client handling.
func (c *Controller) navigateTo(loc location) {
	c.clearFilterUI()
	c.view.Details.Clear()
	if loc.bucket == nil {
		c.currentBucket = nil
		c.currentPath = ""
		c.restoreNext = ""
		go c.updateList()
		return
	}
	go func() {
		name := *loc.bucket.Key
		c.currentBucket = loc.bucket
		c.model.RefreshClient(&name)
		c.currentPath = loc.path
		c.restoreNext = ".."
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
		{"Transfers", c.ShowTransfers},
		{"Filter listing", c.focusFilter},
		{"Recursive search", c.RecursiveSearch},
		{"Bookmarks", c.Bookmarks},
		{"Toggle dual-pane", c.ToggleDualPane},
		{"History back", c.HistoryBack},
		{"History forward", c.HistoryForward},
		{"Size summary", c.ShowSummaryModal},
		{"Properties", func() { c.ShowFileProperties(c.getSelectedObjectName()) }},
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
		c.view.Pages.RemovePage("modal-palette")
		c.view.App.SetFocus(c.view.List)
		act.run()
	}

	input.SetChangedFunc(func(text string) { fill(text) })
	input.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			c.view.Pages.RemovePage("modal-palette")
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

	c.view.Pages.AddPage("modal-palette", c.view.ModalEdit(flex, 70, 22), true, true)
	c.view.App.SetFocus(input)
}

// activityMax caps the in-session operation log.
const activityMax = 200

// logActivity records a one-line, timestamped operation entry. Safe from any
// goroutine.
func (c *Controller) logActivity(format string, args ...any) {
	c.activityMu.Lock()
	c.activity = appendCapped(c.activity, activityEntry{when: time.Now(), msg: fmt.Sprintf(format, args...)}, activityMax)
	c.activityMu.Unlock()
}

// ShowActivityLog displays the operation log, newest first.
func (c *Controller) ShowActivityLog() {
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
			c.view.Pages.RemovePage("modal-activity")
			c.view.App.SetFocus(c.view.List)
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal-activity", c.view.ModalEdit(tv, 90, 30), true, true)
	c.view.App.SetFocus(tv)
}

// AbortMultipartUploads lists the current bucket's incomplete multipart uploads
// and lets the user abort them (Del = selected, a = all).
func (c *Controller) AbortMultipartUploads() {
	if c.currentBucket == nil {
		return
	}
	bucket := c.currentBucket
	go func() {
		ups, err := c.model.ListMultipartUploads(bucket)
		if err != nil {
			c.error("Multipart uploads", err, false)
			return
		}
		c.view.App.QueueUpdateDraw(func() { c.presentMultipartUploads(bucket, ups) })
	}()
}

func (c *Controller) presentMultipartUploads(bucket *model.Object, ups []model.MultipartUpload) {
	if len(ups) == 0 {
		m := tview.NewModal().SetText("No incomplete multipart uploads.").AddButtons([]string{"OK"})
		m.SetDoneFunc(func(int, string) {
			c.view.Pages.RemovePage("modal-mpu")
			c.view.App.SetFocus(c.view.List)
		})
		c.view.Pages.AddPage("modal-mpu", c.view.ModalEdit(m, 50, 8), true, true)
		return
	}

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
				c.error("Abort upload", err, false)
				return
			}
			c.logActivity("aborted incomplete upload %s (%s)", up.Key, *bucket.Key)
			c.view.App.QueueUpdateDraw(func() {
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
					c.error("Abort upload", err, false)
					return
				}
				c.logActivity("aborted incomplete upload %s (%s)", up.Key, *bucket.Key)
			}
			c.view.App.QueueUpdateDraw(func() {
				items = items[:0]
				refresh()
			})
		}()
	}
	refresh = func() {
		list.Clear()
		if len(items) == 0 {
			list.AddItem("[gray](none left)[-]", "", 0, func() {
				c.view.Pages.RemovePage("modal-mpu")
				c.view.App.SetFocus(c.view.List)
			})
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
			c.view.Pages.RemovePage("modal-mpu")
			c.view.App.SetFocus(c.view.List)
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

	c.view.Pages.AddPage("modal-mpu", c.view.ModalEdit(list, 90, 24), true, true)
	c.view.App.SetFocus(list)
}

// BucketDashboard shows the current bucket's configuration (read-only).
func (c *Controller) BucketDashboard() {
	if c.currentBucket == nil {
		return
	}
	bucket := c.currentBucket
	go func() {
		info, err := c.model.BucketConfig(bucket)
		if err != nil {
			c.error("Bucket config", err, false)
			return
		}
		c.view.App.QueueUpdateDraw(func() {
			tv := tview.NewTextView().SetDynamicColors(true).SetWrap(true)
			tv.SetBorder(true).SetTitle(" Bucket config — Esc to close ")
			tv.SetText(fmt.Sprintf(
				"[green]Bucket:[white] %s\n\n[green]Region:[white] %s\n[green]Versioning:[white] %s\n[green]Default encryption:[white] %s\n[green]Object lock:[white] %s",
				*bucket.Key, info.Region, info.Versioning, info.Encryption, info.ObjectLock))
			tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
				if ev.Key() == tcell.KeyEsc {
					c.view.Pages.RemovePage("modal-bucketcfg")
					c.view.App.SetFocus(c.view.List)
					return nil
				}
				return ev
			})
			c.view.Pages.AddPage("modal-bucketcfg", c.view.ModalEdit(tv, 70, 12), true, true)
			c.view.App.SetFocus(tv)
		})
	}()
}

type jobStatus int

const (
	jobRunning jobStatus = iota
	jobQueued
	jobDone
	jobFailed
	jobCanceled
)

func (s jobStatus) String() string {
	switch s {
	case jobRunning:
		return "running"
	case jobQueued:
		return "queued"
	case jobDone:
		return "done"
	case jobFailed:
		return "failed"
	case jobCanceled:
		return "canceled"
	default:
		return "?"
	}
}

// transferJob tracks one background download/upload for the transfers panel.
// Its mutable fields are guarded by mu because progress goroutines write them
// while the panel ticker reads them.
type transferJob struct {
	id     int
	kind   string
	desc   string
	cancel context.CancelFunc
	start  time.Time

	mu        sync.Mutex
	status    jobStatus
	total     int64
	done      int64
	count     int
	doneCount int
	failed    int
	bg        bool
}

func (j *transferJob) setStatus(s jobStatus) { j.mu.Lock(); j.status = s; j.mu.Unlock() }
func (j *transferJob) setProgress(done int64, dc int) {
	j.mu.Lock()
	j.done, j.doneCount = done, dc
	j.mu.Unlock()
}
func (j *transferJob) setTotals(total int64, n int) {
	j.mu.Lock()
	j.total, j.count = total, n
	j.mu.Unlock()
}
func (j *transferJob) setBackgrounded()     { j.mu.Lock(); j.bg = true; j.mu.Unlock() }
func (j *transferJob) isBackgrounded() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.bg }

// jobView is an unlocked snapshot of a job for rendering.
type jobView struct {
	id                       int
	kind, desc               string
	status                   jobStatus
	total, done              int64
	count, doneCount, failed int
	start                    time.Time
}

func (j *transferJob) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return jobView{j.id, j.kind, j.desc, j.status, j.total, j.done, j.count, j.doneCount, j.failed, j.start}
}

// addJob registers a new running job and returns it.
func (c *Controller) addJob(kind, desc string, total int64, count int, cancel context.CancelFunc) *transferJob {
	c.jobsMu.Lock()
	c.nextJobID++
	j := &transferJob{
		id: c.nextJobID, kind: kind, desc: desc, cancel: cancel,
		start: time.Now(), status: jobRunning, total: total, count: count,
	}
	c.jobs = append(c.jobs, j)
	c.jobsMu.Unlock()
	return j
}

// finalizeJob sets a job's terminal status from its outcome and logs it.
func (c *Controller) finalizeJob(j *transferJob, canceled bool, failed int) {
	j.mu.Lock()
	j.failed = failed
	switch {
	case canceled:
		j.status = jobCanceled
	case failed > 0:
		j.status = jobFailed
	default:
		j.status = jobDone
	}
	st := j.status
	j.mu.Unlock()
	c.logActivity("%s %s: %s", j.kind, j.desc, st)
}

func (c *Controller) jobSnapshot() []jobView {
	c.jobsMu.Lock()
	defer c.jobsMu.Unlock()
	out := make([]jobView, 0, len(c.jobs))
	for _, j := range c.jobs {
		out = append(out, j.view())
	}
	return out
}

// transferRow formats one job for the panel. elapsed is passed in so the
// function stays pure (testable).
func transferRow(jv jobView, elapsed time.Duration) (primary, secondary string) {
	pct := 0.0
	if jv.total > 0 {
		pct = float64(jv.done) / float64(jv.total) * 100
	}
	primary = fmt.Sprintf("[%s] %s — %s", jv.status, jv.kind, jv.desc)
	rate := "done"
	if jv.status == jobRunning {
		rate = byteRateETA(jv.done, jv.total, elapsed)
	}
	secondary = fmt.Sprintf("%d/%d obj • %s / %s (%.0f%%) • %s",
		jv.doneCount, jv.count,
		view.HumanizeBytes(jv.done), view.HumanizeBytes(jv.total), pct, rate)
	return primary, secondary
}

// cancelJobAt cancels the running/queued job shown at row i.
func (c *Controller) cancelJobAt(i int) {
	c.jobsMu.Lock()
	var j *transferJob
	if i >= 0 && i < len(c.jobs) {
		j = c.jobs[i]
	}
	c.jobsMu.Unlock()
	if j == nil {
		return
	}
	j.mu.Lock()
	active := j.status == jobRunning || j.status == jobQueued
	cancel := j.cancel
	j.mu.Unlock()
	if active && cancel != nil {
		cancel()
	}
}

// clearFinishedJobs drops done/failed/canceled jobs from the list.
func (c *Controller) clearFinishedJobs() {
	c.jobsMu.Lock()
	kept := c.jobs[:0]
	for _, j := range c.jobs {
		j.mu.Lock()
		st := j.status
		j.mu.Unlock()
		if st == jobRunning || st == jobQueued {
			kept = append(kept, j)
		}
	}
	c.jobs = kept
	c.jobsMu.Unlock()
}

func (c *Controller) renderTransfers() {
	list := c.transfersList
	if list == nil {
		return
	}
	cur := list.GetCurrentItem()
	list.Clear()
	jobs := c.jobSnapshot()
	if len(jobs) == 0 {
		list.AddItem("[gray](no transfers)[-]", "", 0, nil)
		return
	}
	for _, jv := range jobs {
		primary, secondary := transferRow(jv, time.Since(jv.start))
		list.AddItem(primary, secondary, 0, nil)
	}
	if cur >= 0 && cur < list.GetItemCount() {
		list.SetCurrentItem(cur)
	}
}

// ShowTransfers opens the background-transfers panel, refreshing every 300ms
// while open. d/Del cancels the selected job, c clears finished, Esc closes.
func (c *Controller) ShowTransfers() {
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(" Transfers — d: cancel • c: clear finished • Esc: close ")
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	c.transfersList = list
	c.transfersOpen = true
	c.renderTransfers()

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c.view.App.QueueUpdateDraw(func() {
					if c.transfersOpen {
						c.renderTransfers()
					}
				})
			}
		}
	}()

	closePanel := func() {
		c.transfersOpen = false
		close(done)
		c.view.Pages.RemovePage("modal-transfers")
		c.view.App.SetFocus(c.view.List)
	}
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			closePanel()
			return nil
		case tcell.KeyDelete:
			c.cancelJobAt(list.GetCurrentItem())
			return nil
		case tcell.KeyRune:
			switch ev.Rune() {
			case 'd':
				c.cancelJobAt(list.GetCurrentItem())
				return nil
			case 'c':
				c.clearFinishedJobs()
				c.renderTransfers()
				return nil
			}
		}
		return ev
	})

	c.view.Pages.AddPage("modal-transfers", c.view.ModalEdit(list, 100, 28), true, true)
	c.view.App.SetFocus(list)
}
