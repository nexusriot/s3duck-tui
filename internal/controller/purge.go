package controller

import (
	"fmt"

	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// Purge: the version-aware delete. On a versioned bucket the ordinary delete
// only writes delete markers, so nothing is freed and a bucket can never be
// emptied; this deletes stored versions by ID. Two modes, because the two
// needs are different: "every version" makes keys go away for good, "old
// versions only" keeps what each key reads as now and drops its history —
// the routine clean-up of a bucket that has been versioned for a while.

// purgeTarget is one thing to purge: an exact key, or every key under a
// folder prefix.
type purgeTarget struct {
	key    string
	folder bool
}

// purgeTargetsFor turns the selected rows into purge targets. Buckets are not
// targets (deleting a bucket purges it), and neither is anything without a key.
func purgeTargetsFor(objs []*model.Object) []purgeTarget {
	var out []purgeTarget
	for _, o := range objs {
		if o == nil || o.FullPath == nil {
			continue
		}
		switch o.Ot {
		case model.File:
			out = append(out, purgeTarget{key: *o.FullPath})
		case model.Folder:
			out = append(out, purgeTarget{key: *o.FullPath, folder: true})
		}
	}
	return out
}

// purgeWhat names the selection in the confirmation and the job label.
func purgeWhat(ts []purgeTarget) string {
	if len(ts) == 1 {
		return ts[0].key
	}
	return fmt.Sprintf("%d item(s)", len(ts))
}

// planPurgeRefs picks, from one target's listing, the refs a purge removes. A
// file's listing is prefix-based, so it is narrowed to the exact key.
func planPurgeRefs(t purgeTarget, refs []model.VersionRef, mode model.PurgeMode) []model.VersionRef {
	exact := ""
	if !t.folder {
		exact = t.key
	}
	return model.SelectPurge(refs, mode, exact)
}

// purgeSelection is the marked rows, or the highlighted one.
func (c *Controller) purgeSelection() []*model.Object {
	names := c.selectedNames()
	if len(names) == 0 {
		if n := c.getSelectedObjectName(); n != "" && n != ".." {
			names = []string{n}
		}
	}
	var objs []*model.Object
	for _, n := range names {
		if o, ok := c.pane().Lookup(n); ok {
			objs = append(objs, o)
		}
	}
	return objs
}

// Purge asks which versions to delete for the selection, then scans and hands
// the planned list to the runner, whose confirmation states exactly what goes.
// Bound to leader X, and reached by v when several items are marked.
func (c *Controller) Purge() {
	if c.remoteOnly("Purge") {
		return
	}
	if c.readOnlyBlocked("purge versions") {
		return
	}
	bucket := c.pane().Bucket()
	if bucket == nil || bucket.Key == nil {
		c.error("Purge", fmt.Errorf("open a bucket first; to purge a whole bucket, delete it — that removes every version"))
		return
	}
	targets := purgeTargetsFor(c.purgeSelection())
	if len(targets) == 0 {
		return
	}

	const (
		btnAll = "Every version"
		btnOld = "Old versions only"
	)
	// lay is assigned below; the handler runs after that.
	var lay *shell.Layer
	modal := tview.NewModal().
		SetText(tview.Escape(fmt.Sprintf(
			"Purge %s\n\nEvery version: the keys are gone for good, history included.\nOld versions only: what each key reads as now stays; older versions and old delete markers go.",
			purgeWhat(targets)))).
		AddButtons([]string{btnAll, btnOld, "Cancel"}).
		SetDoneFunc(func(_ int, label string) {
			lay.Close()
			switch label {
			case btnAll:
				c.scanAndPurge(bucket, targets, model.PurgeAll)
			case btnOld:
				c.scanAndPurge(bucket, targets, model.PurgeNoncurrent)
			}
		})
	lay = c.pushRaw(modal)
}

// scanAndPurge lists each target's versions behind a cancellable wait, then
// starts the purge. Runs on the UI goroutine; the scan does not.
func (c *Controller) scanAndPurge(bucket *model.Object, targets []purgeTarget, mode model.PurgeMode) {
	mdl := c.model
	wait, modal, ctx, cancel := c.cancellableWait("Listing versions...")
	go func() {
		defer cancel()
		var planned []model.VersionRef
		scanned := 0
		for _, t := range targets {
			var refs []model.VersionRef
			err := mdl.ListVersionsUnder(ctx, bucket, t.key, func(page []model.VersionRef) bool {
				refs = append(refs, page...)
				scanned += len(page)
				n := scanned
				c.shell.Do(func() { modal.SetText(fmt.Sprintf("Listing versions...\n\n%s scanned", humanCount(n))) })
				return true
			})
			if err != nil {
				c.shell.Do(wait.Close)
				if ctx.Err() == nil {
					c.error("Purge: listing versions failed", err)
				}
				return
			}
			planned = append(planned, planPurgeRefs(t, refs, mode)...)
		}
		c.shell.Do(func() {
			wait.Close()
			if len(planned) == 0 {
				what := "no versions"
				if mode == model.PurgeNoncurrent {
					what = "no old versions"
				}
				c.success(fmt.Sprintf("%s has %s to delete", purgeWhat(targets), what))
				return
			}
			c.runner().Start(&ops.Purge{
				Bucket: bucket, Refs: planned, Mode: mode,
				What: purgeWhat(targets), Deleter: mdl,
			})
		})
	}()
}
