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
	"github.com/nexusriot/s3duck-tui/internal/jobs"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/flows"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
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

// modelConfigFor translates a stored profile into the model's connection
// config. It is the single place that mapping lives, so opening a profile and
// building a second client for a cross-profile copy can't drift on which
// options they honour — the bug that class of duplication produced before was
// a cross-profile copy running unthrottled because only Duck read the cap.
func modelConfigFor(p *cfg.Config) model.Config {
	mc := model.NewConfig(
		p.BaseUrl, p.Region, p.AccessKey, p.SecretKey, p.SessionToken,
		!p.IgnoreSsl, p.MaxBytesPerSec)
	mc.AWSProfile = p.AWSProfile
	mc.Write = model.WriteOptions{
		DetectMime:        !p.NoMimeDetect,
		SSE:               p.SSE,
		SSEKMSKeyID:       p.SSEKMSKeyID,
		ChecksumAlgorithm: p.ChecksumAlgo,
	}
	return mc
}

// modelForProfile builds an independent client for another profile, the same
// way opening the profile would.
func modelForProfile(p *cfg.Config) (*model.Model, error) {
	return model.NewModel(modelConfigFor(p))
}

// CopyToProfile copies the marked objects — or the highlighted one — to a
// bucket in a *different profile*, streaming each object through this process
// (GET here → PUT there). This is the one copy that works across endpoints;
// the server-side Ctrl+Y copy requires both buckets behind one endpoint.
// Runs on the UI goroutine.
func (c *Controller) CopyToProfile() {
	if c.remoteOnly("Cross-profile copy") {
		return
	}
	if c.pane().Bucket() == nil || c.view.List.GetItemCount() == 0 {
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
		o, ok := c.pane().Lookup(n)
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
		c.error("Copy to profile", fmt.Errorf("no other profiles configured"))
		return
	}

	srcBucket := c.pane().Bucket()
	srcPrefix := model.NormalizePrefix(c.pane().Path())

	names = names[:0]
	for _, p := range others {
		names = append(names, p.Name)
	}

	var lay *shell.Layer
	form := tview.NewForm()
	form.SetTitle(fmt.Sprintf(" Copy %d item(s) to another profile ", len(items)))
	form.AddDropDown("Destination profile", names, 0, nil)
	form.SetBorder(true)
	form.AddButton("Next", func() {
		i, _ := form.GetFormItemByLabel("Destination profile").(*tview.DropDown).GetCurrentOption()
		lay.Close()
		if i < 0 || i >= len(others) {
			return
		}
		c.pickCrossDestination(items, src, srcBucket, srcPrefix, others[i])
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))

	lay = c.pushModal(form, 60, 9)
}

// pickCrossDestination builds the destination client, lists its buckets, and
// asks for bucket + prefix. Runs on the UI goroutine; the listing is not.
func (c *Controller) pickCrossDestination(items []copyMoveItem, src *model.Model, srcBucket *model.Object, srcPrefix string, dstProfile *cfg.Config) {
	// The read-only flag that matters here belongs to the *destination*
	// profile: this copy writes there and only reads here.
	if dstProfile != nil && dstProfile.ReadOnly {
		c.error("Read-only destination", fmt.Errorf(
			"%q is marked read-only, so it cannot be copied into", dstProfile.Name))
		return
	}
	wait := c.wait(fmt.Sprintf("Connecting to %s...", dstProfile.Name))

	go func() {
		dst, err := modelForProfile(dstProfile)
		if err != nil {
			c.shell.Do(wait.Close)
			c.error(fmt.Sprintf("Cannot connect to %s", dstProfile.Name), err)
			return
		}
		buckets, err := dst.ListBuckets(context.Background())
		if err != nil {
			c.shell.Do(wait.Close)
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
			c.shell.Do(wait.Close)
			c.error("Copy to profile", fmt.Errorf("profile %s has no buckets", dstProfile.Name))
			return
		}

		c.shell.Do(func() {
			wait.Close()

			var lay *shell.Layer
			form := tview.NewForm()
			form.SetTitle(fmt.Sprintf(" Destination on %s ", tview.Escape(dstProfile.Name)))
			form.AddDropDown("Bucket", bucketNames, 0, nil)
			form.AddInputField("Prefix", "", 50, nil, nil)
			form.SetBorder(true)
			form.AddButton("Copy", func() {
				_, bucketName := form.GetFormItemByLabel("Bucket").(*tview.DropDown).GetCurrentOption()
				prefix := model.NormalizeTypedPrefix(form.GetFormItemByLabel("Prefix").(*tview.InputField).GetText())
				lay.Close()
				c.runCrossCopy(items, src, srcBucket, srcPrefix, dst, dstProfile.Name, bucketName, prefix)
			})
			form.AddButton("Cancel", func() { lay.Close() })
			form.SetInputCapture(escapeCloses(&lay))

			lay = c.pushModal(form, 66, 11)
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

	job := c.queue.Add("xcopy", fmt.Sprintf("%s/%s → %s:%s/%s",
		*srcBucket.Key, srcPrefix, dstProfileName, dstBucketName, dstPrefix), 0, 0, cancel)

	var lay *shell.Layer
	progress := tview.NewModal().
		SetText("Preparing cross-profile copy...\n").
		AddButtons([]string{"Background", "Cancel"}).
		SetDoneFunc(func(_ int, buttonLabel string) {
			switch buttonLabel {
			case "Background":
				job.SetBackgrounded()
			default:
				cancel()
			}
			lay.Close()
		})
	lay = c.pushRaw(progress)

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
			objs, err := src.ListObjects(ctx, it.srcKey, srcBucket)
			if err != nil {
				c.shell.Do(lay.Close)
				// A Cancel during expansion is the user's decision, not a
				// failure: reported as one, a backgrounded job rang the bell
				// with a ✗ notice for something nobody did wrong.
				if ctx.Err() != nil {
					c.queue.Finish(job, true, 0)
					return
				}
				c.queue.Finish(job, false, 1)
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
			c.queue.Finish(job, false, 0)
			c.shell.Do(lay.Close)
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
			c.shell.Do(lay.Close)
			if ctx.Err() != nil { // cancelled during the probe, as above
				c.queue.Finish(job, true, 0)
				return
			}
			c.queue.Finish(job, false, 1)
			c.error("Copy to profile", fmt.Errorf("checking the destination: %w", cErr))
			return
		}
		if len(conflicts) > 0 {
			skip, proceed := c.askOverwriteBlocking("Copy to profile", conflicts, len(dstKeys))
			if !proceed {
				cancel()
				c.queue.Finish(job, true, 0)
				c.shell.Do(lay.Close)
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
			c.queue.Finish(job, false, 0)
			c.shell.Do(lay.Close)
			c.success("Nothing to copy: every object was skipped")
			return
		}
		job.SetTotals(total, len(ops))

		job.SetStatus(jobs.Queued)
		if !c.queue.Acquire(ctx) {
			// cancelled before a transfer slot came free
			c.queue.Finish(job, true, 0)
			c.shell.Do(lay.Close)
			return
		}
		// Release the slot however this goroutine ends.
		defer c.queue.Release()
		job.SetStatus(jobs.Running)

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

			job.SetProgress(n, i)
			if job.IsBackgrounded() || throttled {
				return
			}
			pct := 0.0
			if total > 0 {
				pct = float64(n) / float64(total) * 100
			}
			c.shell.Do(func() {
				progress.SetText(fmt.Sprintf(
					"Copying to %s\n%d/%d object(s)\n%s/%s (%.1f%%)\n%s\n%s",
					dstProfileName, i, len(ops),
					humanize.IBytes(uint64(n)), humanize.IBytes(uint64(total)), pct,
					flows.ByteRateETA(n, total, time.Since(start)),
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
			job.SetProgress(doneBytes, i+1)
		}

		c.logActivity("Cross-profile copy → %s: %d ok, %d failed (%s)",
			dstProfileName, okCount, len(failed), humanize.IBytes(uint64(doneBytes)))
		c.queue.Finish(job, canceled, len(failed))

		if canceled || job.IsBackgrounded() {
			return
		}

		c.shell.Do(func() {
			// Re-check ON the UI goroutine — a Background press racing the
			// completion may have removed the page; focusing a detached modal
			// routes every key into an invisible widget.
			if job.IsBackgrounded() || !lay.IsOpen() {
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
			progress.SetDoneFunc(func(_ int, _ string) { lay.Close() })
			c.view.App.SetFocus(progress)
		})
	}()
}

// escapeCloses is the shared Esc handler for one-off forms. It takes the
// layer by pointer-to-pointer because the handler is installed before the
// layer exists — the form has to be built to be pushed.
func escapeCloses(lay **shell.Layer) func(event *tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			(*lay).Close()
			return nil
		}
		return event
	}
}
