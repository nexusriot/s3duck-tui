package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// crossOp is one object to move between profiles: an absolute source key and
// the absolute destination key it lands on.
type crossOp struct {
	SrcKey string
	DstKey string
	Size   int64
}

// crossDstKey maps a source key onto the destination prefix, keeping the tail
// relative to the source location — so copying the folder "photos/" from
// "backups/" preserves "photos/…" under the destination prefix rather than
// flattening or double-nesting it.
func crossDstKey(srcPrefix, dstPrefix, key string) string {
	return dstPrefix + strings.TrimPrefix(key, srcPrefix)
}

// modelForProfile builds an independent client for another profile, the same
// way opening the profile would.
func modelForProfile(p *cfg.Config) (*model.Model, error) {
	return model.NewModel(model.NewConfig(
		p.BaseUrl, p.Region, p.AccessKey, p.SecretKey, p.SessionToken,
		!p.IgnoreSsl, p.MaxBytesPerSec))
}

// CopyToProfile copies the marked objects — or the highlighted one — to a
// bucket in a *different profile*, streaming each object through this process
// (GET here → PUT there). This is the one copy that works across endpoints;
// the server-side Ctrl+Y copy requires both buckets behind one endpoint.
// Runs on the UI goroutine.
func (c *Controller) CopyToProfile() {
	if c.currentBucket == nil || c.view.List.GetItemCount() == 0 {
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
	var items []copyMoveItem
	for _, n := range names {
		o, ok := c.lookupObj(n)
		if !ok || (o.Ot != model.File && o.Ot != model.Folder) {
			continue
		}
		it := copyMoveItem{
			shortName: *o.Key,
			srcKey:    *o.FullPath,
			isFolder:  o.Ot == model.Folder,
		}
		// Size captured HERE, on the UI goroutine, while the listing still
		// shows these objects. The transfer goroutine used to look sizes up
		// by short name in the FullPath-keyed map — a guaranteed miss below
		// the bucket root, so totals were 0 and progress/ETA garbage.
		if o.Size != nil {
			it.size = *o.Size
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return
	}
	// The source client, before any goroutine: a profile switch mid-flow must
	// not swap the endpoint the objects are read from.
	src := c.model

	var others []*cfg.Config
	for _, p := range c.params.Config {
		if p != nil && (c.activeConfig == nil || p.Name != c.activeConfig.Name) {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		go c.error("Copy to profile", fmt.Errorf("no other profiles configured"))
		return
	}

	srcBucket := c.currentBucket
	srcPrefix := model.NormalizePrefix(c.currentPath)

	names = names[:0]
	for _, p := range others {
		names = append(names, p.Name)
	}

	form := tview.NewForm()
	form.SetTitle(fmt.Sprintf(" Copy %d item(s) to another profile ", len(items)))
	form.AddDropDown("Destination profile", names, 0, nil)
	form.SetBorder(true)
	form.AddButton("Next", func() {
		i, _ := form.GetFormItemByLabel("Destination profile").(*tview.DropDown).GetCurrentOption()
		c.view.Pages.RemovePage("modal")
		if i < 0 || i >= len(others) {
			return
		}
		c.pickCrossDestination(items, src, srcBucket, srcPrefix, others[i])
	})
	form.AddButton("Cancel", func() { c.view.Pages.RemovePage("modal") })
	form.SetInputCapture(escapeCloses(c, "modal"))

	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 60, 9), true, true)
}

// pickCrossDestination builds the destination client, lists its buckets, and
// asks for bucket + prefix. Runs on the UI goroutine; the listing is not.
func (c *Controller) pickCrossDestination(items []copyMoveItem, src *model.Model, srcBucket *model.Object, srcPrefix string, dstProfile *cfg.Config) {
	loading := tview.NewModal().SetText(fmt.Sprintf("Connecting to %s...", dstProfile.Name))
	c.view.Pages.AddPage("progress", loading, true, true)

	go func() {
		dst, err := modelForProfile(dstProfile)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.error(fmt.Sprintf("Cannot connect to %s", dstProfile.Name), err)
			return
		}
		buckets, err := dst.ListBuckets()
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.error(fmt.Sprintf("Cannot list buckets on %s", dstProfile.Name), err)
			return
		}
		var bucketNames []string
		for _, b := range buckets {
			if b != nil && b.Key != nil {
				bucketNames = append(bucketNames, *b.Key)
			}
		}
		if len(bucketNames) == 0 {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.error("Copy to profile", fmt.Errorf("profile %s has no buckets", dstProfile.Name))
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress")

			form := tview.NewForm()
			form.SetTitle(fmt.Sprintf(" Destination on %s ", dstProfile.Name))
			form.AddDropDown("Bucket", bucketNames, 0, nil)
			form.AddInputField("Prefix", "", 50, nil, nil)
			form.SetBorder(true)
			form.AddButton("Copy", func() {
				_, bucketName := form.GetFormItemByLabel("Bucket").(*tview.DropDown).GetCurrentOption()
				prefix := model.NormalizePrefix(form.GetFormItemByLabel("Prefix").(*tview.InputField).GetText())
				c.view.Pages.RemovePage("modal")
				c.runCrossCopy(items, src, srcBucket, srcPrefix, dst, dstProfile.Name, bucketName, prefix)
			})
			form.AddButton("Cancel", func() { c.view.Pages.RemovePage("modal") })
			form.SetInputCapture(escapeCloses(c, "modal"))

			c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 66, 11), true, true)
		})
	}()
}

// runCrossCopy expands folders into concrete objects, then streams every
// object through this process as a cancellable, backgroundable transfer job.
// The source is never modified.
func (c *Controller) runCrossCopy(items []copyMoveItem, src *model.Model, srcBucket *model.Object, srcPrefix string, dst *model.Model, dstProfileName, dstBucketName, dstPrefix string) {
	ctx, cancel := context.WithCancel(context.Background())
	name := dstBucketName
	dstBucket := &model.Object{Key: &name, Ot: model.Bucket}

	job := c.addJob("xcopy", fmt.Sprintf("%s/%s → %s:%s/%s",
		*srcBucket.Key, srcPrefix, dstProfileName, dstBucketName, dstPrefix), 0, 0, cancel)

	progress := tview.NewModal().
		SetText("Preparing cross-profile copy...\n").
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
		// The destination bucket's region matters only on AWS; RefreshClient
		// is fail-safe and keeps the client usable on error.
		if err := dst.RefreshClient(&name); err != nil {
			c.error("Failed to resolve destination region", err)
		}

		// Expand folders into concrete operations.
		var ops []crossOp
		var total int64
		for _, it := range items {
			if !it.isFolder {
				ops = append(ops, crossOp{
					SrcKey: it.srcKey,
					DstKey: crossDstKey(srcPrefix, dstPrefix, it.srcKey),
					Size:   it.size,
				})
				total += it.size
				continue
			}
			objs, err := src.ListObjects(it.srcKey, srcBucket)
			if err != nil {
				c.finalizeJob(job, false, 1)
				c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
				c.error(fmt.Sprintf("Listing %s failed", it.srcKey), err)
				return
			}
			for _, o := range objs {
				if o.Key == nil || strings.HasSuffix(*o.Key, "/") {
					continue
				}
				ops = append(ops, crossOp{
					SrcKey: *o.Key,
					DstKey: crossDstKey(srcPrefix, dstPrefix, *o.Key),
					Size:   o.Size,
				})
				total += o.Size
			}
		}
		if len(ops) == 0 {
			c.finalizeJob(job, false, 0)
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.error("Copy to profile", fmt.Errorf("nothing to copy"))
			return
		}

		// The same guard the same-endpoint copies get: say what would be
		// replaced on the other profile before replacing it. Asked from here
		// rather than up front because only now are the folders expanded into
		// the concrete keys this would land on.
		dstKeys := make([]string, 0, len(ops))
		for _, op := range ops {
			dstKeys = append(dstKeys, op.DstKey)
		}
		conflicts, cErr := dst.Conflicts(ctx, dstBucket, dstKeys)
		if cErr != nil {
			c.finalizeJob(job, false, 1)
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.error("Copy to profile", fmt.Errorf("checking the destination: %w", cErr))
			return
		}
		if len(conflicts) > 0 {
			skip, proceed := c.askOverwriteBlocking("Copy to profile", conflicts, len(dstKeys))
			if !proceed {
				cancel()
				c.finalizeJob(job, true, 0)
				c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
				return
			}
			if len(skip) > 0 {
				kept, keptTotal := ops[:0:0], int64(0)
				for _, op := range ops {
					if skip[op.DstKey] {
						continue
					}
					kept = append(kept, op)
					keptTotal += op.Size
				}
				ops, total = kept, keptTotal
			}
		}
		if len(ops) == 0 {
			c.finalizeJob(job, false, 0)
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.success("Nothing to copy: every object was skipped")
			return
		}
		job.setTotals(total, len(ops))

		job.setStatus(jobQueued)
		select {
		case c.jobSem <- struct{}{}:
			defer func() { <-c.jobSem }()
		case <-ctx.Done():
			c.finalizeJob(job, true, 0)
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			return
		}
		job.setStatus(jobRunning)

		start := time.Now()
		var mu sync.Mutex
		var lastDraw time.Time
		var doneBytes int64
		var failed []string
		okCount := 0
		canceled := false

		draw := func(i int, op crossOp, fileDone int64) {
			mu.Lock()
			n := doneBytes + fileDone
			now := time.Now()
			throttled := now.Sub(lastDraw) < 100*time.Millisecond
			if !throttled {
				lastDraw = now
			}
			mu.Unlock()

			job.setProgress(n, i)
			if job.isBackgrounded() || throttled {
				return
			}
			pct := 0.0
			if total > 0 {
				pct = float64(n) / float64(total) * 100
			}
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf(
					"Copying to %s\n%d/%d object(s)\n%s/%s (%.1f%%)\n%s\n%s",
					dstProfileName, i, len(ops),
					humanize.IBytes(uint64(n)), humanize.IBytes(uint64(total)), pct,
					byteRateETA(n, total, time.Since(start)),
					op.SrcKey,
				))
			})
		}

	loop:
		for i, op := range ops {
			select {
			case <-ctx.Done():
				canceled = true
				break loop
			default:
			}

			draw(i, op, 0)
			err := model.CrossCopy(ctx, src, srcBucket, op.SrcKey, dst, dstBucket, op.DstKey,
				func(written, _ int64) { draw(i, op, written) })
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", op.SrcKey, err))
				continue
			}
			okCount++
			mu.Lock()
			doneBytes += op.Size
			mu.Unlock()
			job.setProgress(doneBytes, i+1)
		}

		c.logActivity("Cross-profile copy → %s: %d ok, %d failed (%s)",
			dstProfileName, okCount, len(failed), humanize.IBytes(uint64(doneBytes)))
		c.finalizeJob(job, canceled, len(failed))

		if canceled || job.isBackgrounded() {
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			// Re-check ON the UI goroutine — a Background press racing the
			// completion may have removed the page; focusing a detached modal
			// routes every key into an invisible widget.
			if job.isBackgrounded() || !c.view.Pages.HasPage("progress") {
				return
			}
			status := "Cross-profile copy complete."
			if len(failed) > 0 {
				status = "Cross-profile copy finished with errors."
			}
			msg := fmt.Sprintf("%s\n\nCopied: %d\nFailed: %d\nTransferred: %s",
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
	}()
}

// escapeCloses is the shared Esc handler for one-off forms.
func escapeCloses(c *Controller, page string) func(event *tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage(page)
			return nil
		}
		return event
	}
}
