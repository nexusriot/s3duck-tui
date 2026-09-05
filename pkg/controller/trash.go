package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

// trashStamp formats the per-delete folder inside the trash prefix. Second
// resolution: two deletes in the same second are the same user action.
func trashStamp(t time.Time) string { return t.UTC().Format("20060102-150405") }

// trashKeyFor maps an object key onto its place in the trash. The original
// key is preserved in full under the stamp folder, so a restore is a move
// back to the tail after the stamp.
//
//	.s3duck-trash/20260902-181500/docs/report.pdf
func trashKeyFor(trashPrefix, stamp, key string) string {
	return trashPrefix + stamp + "/" + strings.TrimPrefix(key, "/")
}

// restoreKeyFrom is trashKeyFor inverted: given a key inside the trash, the
// original key it was deleted from. It returns "" for a key that is not in
// the trash, or one whose stamp folder holds no original path.
func restoreKeyFrom(trashPrefix, key string) string {
	rest, ok := cutPrefix(key, trashPrefix)
	if !ok {
		return ""
	}
	i := strings.Index(rest, "/")
	if i < 0 || i == len(rest)-1 {
		return ""
	}
	return rest[i+1:]
}

func cutPrefix(s, prefix string) (string, bool) {
	if prefix == "" || !strings.HasPrefix(s, prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// isTrashed reports whether a key lives in the profile's trash.
func isTrashed(trashPrefix, key string) bool {
	return trashPrefix != "" && strings.HasPrefix(key, trashPrefix)
}

// trashPrefix returns the active profile's trash prefix, or "" when safe
// delete is off.
func (c *Controller) trashPrefix() string {
	if c.activeConfig == nil {
		return ""
	}
	return c.activeConfig.Trashed()
}

// trashTarget moves one delete target under the trash prefix. It is a
// server-side move (copy + delete of exactly what was copied), so no bytes
// cross the wire and the semantics match the ordinary move path.
//
// Deleting something that is *already* in the trash removes it for real:
// otherwise the trash could never be emptied, and each attempt would nest a
// deeper copy of it inside itself.
func (c *Controller) trashTarget(ctx context.Context, mdl *model.Model, bucket *model.Object, t deleteTarget, trash string) error {
	if isTrashed(trash, t.key) {
		key := t.key
		return mdl.Delete(ctx, &key, bucket)
	}
	dst := trashKeyFor(trash, trashStamp(time.Now()), t.key)
	if t.isFolder {
		// MoveKeys wants both prefixes slash-terminated; trashKeyFor keeps
		// the source's trailing slash, so dst already is.
		_, err := mdl.MoveKeys(ctx, bucket, bucket, t.key, dst, true, nil, nil)
		return err
	}
	_, err := mdl.MoveKeys(ctx, bucket, bucket, t.key, dst, false, nil, nil)
	return err
}

// EmptyTrash removes the profile's trash prefix outright, after a confirmation
// stating what it holds. This is the delete that is never diverted.
func (c *Controller) EmptyTrash() {
	trash := c.trashPrefix()
	if trash == "" {
		go c.error("Safe delete is off", fmt.Errorf("this profile deletes objects directly, so there is no trash"))
		return
	}
	if c.currentBucket == nil {
		go c.error("No bucket", fmt.Errorf("open a bucket first: the trash lives inside one"))
		return
	}
	if c.readOnlyBlocked("empty the trash") {
		return
	}
	bucket := c.currentBucket
	target := deleteTarget{name: trash, key: trash, isFolder: true}

	_, ctx, cancel := c.cancellableWait("progress", "Sizing the trash...")
	mdl := c.model
	go func() {
		defer cancel()
		objs, err := mdl.ListObjects(ctx, trash, bucket)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			if ctx.Err() == nil {
				c.error("Cannot size the trash", err)
			}
			return
		}
		for _, o := range objs {
			target.objects++
			target.bytes += o.Size
		}
		if target.objects == 0 {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.success(fmt.Sprintf("The trash (%s) is already empty", trash))
			return
		}
		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress")
			confirm := c.view.NewConfirm()
			confirm.SetText(fmt.Sprintf(
				"Permanently delete the trash?\n\n%s\n\nThis removes %d object(s), %s.\nThey are not recoverable.",
				trash, target.objects, view.HumanizeBytes(target.bytes)))
			confirm.SetDoneFunc(func(_ int, label string) {
				c.view.Pages.RemovePage("modal")
				if label != "ok" {
					return
				}
				// runDelete diverts to the trash for a trash-enabled profile;
				// trashTarget notices the target is already inside the trash
				// and removes it for real.
				c.runDelete([]deleteTarget{target}, bucket)
			})
			c.view.Pages.AddPage("modal", c.view.ModalEdit(confirm, 70, 12), true, true)
		})
	}()
}

// RestoreFromTrash moves the marked items (or the highlighted one) out of the
// trash and back to the keys they were deleted from.
func (c *Controller) RestoreFromTrash() {
	trash := c.trashPrefix()
	if trash == "" {
		go c.error("Safe delete is off", fmt.Errorf("this profile has no trash to restore from"))
		return
	}
	if c.currentBucket == nil || c.readOnlyBlocked("restore from the trash") {
		return
	}
	names := c.selectedNames()
	if len(names) == 0 {
		if n := c.getSelectedObjectName(); n != "" && n != ".." {
			names = []string{n}
		}
	}
	type restoreOp struct {
		src, dst string
		isFolder bool
	}
	var ops []restoreOp
	for _, n := range names {
		o, ok := c.lookupObj(n)
		if !ok || o.Ot == model.Bucket || o.FullPath == nil {
			continue
		}
		src := *o.FullPath
		dst := restoreKeyFrom(trash, src)
		if dst == "" {
			continue
		}
		ops = append(ops, restoreOp{src: src, dst: dst, isFolder: o.Ot == model.Folder})
	}
	if len(ops) == 0 {
		go c.error("Nothing to restore", fmt.Errorf("select items inside %s — only objects in the trash carry the key they came from", trash))
		return
	}

	bucket := c.currentBucket
	mdl := c.model
	progress, ctx, cancel := c.cancellableWait("progress", "Restoring from trash...")
	go func() {
		defer cancel()
		res := newOpResult("Restore")
		canceled := false
		for i, op := range ops {
			if ctx.Err() != nil {
				canceled = true
				break
			}
			i, op := i, op
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf("Restoring\n%d/%d\n%s", i+1, len(ops), op.dst))
			})
			if _, err := mdl.MoveKeys(ctx, bucket, bucket, op.src, op.dst, op.isFolder, nil, nil); err != nil {
				res.fail(retryItem{label: op.src}, err)
				continue
			}
			res.ok()
			c.logActivity("Restored %s -> %s", op.src, op.dst)
		}
		c.reportOpResult(progress, res, canceled, nil)
		c.updateList()
	}()
}

// trashSummaryLine describes the safe-delete setting for the profile details
// pane.
func trashSummaryLine(p *cfg.Config) string {
	if p == nil || !p.Trash {
		return "off (delete removes objects)"
	}
	return "on -> " + p.Trashed()
}
