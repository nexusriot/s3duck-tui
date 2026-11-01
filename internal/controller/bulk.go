package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

// Bulk metadata and storage class: m and c acted on the highlighted object
// only, while download, copy, move and delete honoured the marked set — so
// tagging forty objects, or archiving a folder, was forty dialogs. With more
// than one item marked (or a folder chosen) they now resolve the selection to
// object keys and apply one change to each through ops.Batch: one
// confirmation, one progress modal, Background/Cancel, and Retry failed.

// bulkMetaEdit is a change to apply to many objects at once. Unlike the
// single-object editor it does not show values to overwrite — the objects
// differ — so every field says what to change, and blank means "leave it".
type bulkMetaEdit struct {
	contentType, cacheControl string
	setMeta, setTags          map[string]string
	dropMeta, dropTags        []string
}

// empty reports an edit that would change nothing.
func (e bulkMetaEdit) empty() bool {
	return e.contentType == "" && e.cacheControl == "" &&
		len(e.setMeta) == 0 && len(e.setTags) == 0 && len(e.dropMeta) == 0 && len(e.dropTags) == 0
}

// touchesMeta / touchesTags say which half the edit changes: a metadata change
// is a full copy of the object, so it is not made for a tags-only edit.
func (e bulkMetaEdit) touchesMeta() bool {
	return e.contentType != "" || e.cacheControl != "" || len(e.setMeta) > 0 || len(e.dropMeta) > 0
}

func (e bulkMetaEdit) touchesTags() bool { return len(e.setTags) > 0 || len(e.dropTags) > 0 }

// applyMeta merges the edit into one object's metadata, keeping everything it
// does not name, and reports whether anything changed.
func (e bulkMetaEdit) applyMeta(m model.ObjectMeta) (model.ObjectMeta, bool) {
	out := m
	out.UserMetadata = map[string]string{}
	for k, v := range m.UserMetadata {
		out.UserMetadata[k] = v
	}
	if e.contentType != "" {
		out.ContentType = e.contentType
	}
	if e.cacheControl != "" {
		out.CacheControl = e.cacheControl
	}
	for _, k := range e.dropMeta {
		delete(out.UserMetadata, strings.ToLower(k))
	}
	for k, v := range e.setMeta {
		// S3 lower-cases user metadata keys on the way in, so a key that
		// differed only in case would read back as a change every time.
		out.UserMetadata[strings.ToLower(k)] = v
	}
	return out, !sameMeta(m, out)
}

// applyTags merges the edit into one object's tag set.
func (e bulkMetaEdit) applyTags(tags map[string]string) (map[string]string, bool) {
	out := map[string]string{}
	for k, v := range tags {
		out[k] = v
	}
	for _, k := range e.dropTags {
		delete(out, k)
	}
	for k, v := range e.setTags {
		out[k] = v
	}
	return out, formatKVLines(out) != formatKVLines(tags)
}

// splitKeys reads a comma- or space-separated list of keys.
func splitKeys(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// bulkTargets resolves the selection to object keys: files as they are,
// folders to every object under them (folder markers and the trash left
// out), duplicates removed. Off the UI goroutine.
func bulkTargets(ctx context.Context, mdl *model.Model, bucket *model.Object, objs []*model.Object, trash string) ([]string, error) {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if k == "" || strings.HasSuffix(k, "/") || seen[k] || isTrashed(trash, k) {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}
	for _, o := range objs {
		if o == nil || o.FullPath == nil {
			continue
		}
		switch o.Ot {
		case model.File:
			add(*o.FullPath)
		case model.Folder:
			listed, err := mdl.ListObjects(ctx, *o.FullPath, bucket)
			if err != nil {
				return nil, fmt.Errorf("listing %s: %w", *o.FullPath, err)
			}
			for _, l := range listed {
				if l.Key != nil {
					add(*l.Key)
				}
			}
		}
	}
	return keys, nil
}

// isBulk reports whether m and c should act on the selection rather than the
// highlighted object: more than one item marked, or a folder in the selection.
func (c *Controller) isBulk() bool {
	if c.selectedCount() > 1 {
		return true
	}
	for _, o := range c.purgeSelection() {
		if o.Ot == model.Folder {
			return true
		}
	}
	return false
}

// resolveBulk expands the selection behind a cancellable wait and calls then
// with the keys, on the UI goroutine.
func (c *Controller) resolveBulk(what string, then func(bucket *model.Object, keys []string)) {
	bucket := c.pane().Bucket()
	objs := c.purgeSelection()
	mdl := c.model
	trash := c.trashPrefix()
	wait, _, ctx, cancel := c.cancellableWait(fmt.Sprintf("Resolving the selection for %s...", what))
	go func() {
		defer cancel()
		keys, err := bulkTargets(ctx, mdl, bucket, objs, trash)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error(what, err)
			}
			return
		}
		if len(keys) == 0 {
			c.error(what, fmt.Errorf("the selection holds no objects"))
			return
		}
		c.shell.Do(func() { then(bucket, keys) })
	}()
}

// bulkStorageClass is c over the marked set.
func (c *Controller) bulkStorageClass() {
	c.resolveBulk("Storage class", func(bucket *model.Object, keys []string) {
		mdl := c.model
		profile := c.profileName()
		var lay *shell.Layer
		form := c.view.NewStorageClassForm(fmt.Sprintf("%d objects", len(keys)),
			"Apply one class to all of them, or request a restore of the archived ones\n(objects already in that state are skipped).",
			model.StorageClasses(), "", model.RestoreTiers(), true)
		form.AddButton("Apply class", func() {
			_, class := form.GetFormItemByLabel(widgets.FieldStorageClass).(*tview.DropDown).GetCurrentOption()
			lay.Close()
			c.runner().Start(&ops.Batch{
				Verb:    "storage class → " + class,
				Confirm: fmt.Sprintf("Set the storage class of %d object(s) to %s?\n\nEach is rewritten in place (a server-side copy). Archived objects must be restored first and will fail.", len(keys), class),
				Keys:    keys,
				Apply: func(ctx context.Context, key string) error {
					meta, err := mdl.HeadObject(ctx, bucket, key)
					if err != nil {
						return err
					}
					if storageClassOf(meta) == class {
						return ops.ErrUnchanged
					}
					return mdl.SetStorageClass(ctx, bucket, key, class)
				},
			})
		})
		form.AddButton("Restore archived", func() {
			daysText := strings.TrimSpace(form.GetFormItemByLabel(widgets.FieldRestoreDays).(*tview.InputField).GetText())
			_, tier := form.GetFormItemByLabel(widgets.FieldRestoreTier).(*tview.DropDown).GetCurrentOption()
			days, err := strconv.Atoi(daysText)
			if err != nil || days < 1 {
				c.error("Restore", fmt.Errorf("days must be a positive number, got %q", daysText))
				return
			}
			lay.Close()
			c.runner().Start(&ops.Batch{
				Verb:    fmt.Sprintf("restore (%d day(s), %s)", days, tier),
				Confirm: fmt.Sprintf("Request a %s restore for %d day(s) of the archived objects among %d?\n\nObjects that are not archived, or already being restored, are skipped. Each restore is tracked (leader r).", tier, days, len(keys)),
				Keys:    keys,
				Apply: func(ctx context.Context, key string) error {
					meta, err := mdl.HeadObject(ctx, bucket, key)
					if err != nil {
						return err
					}
					if !model.IsArchived(meta.StorageClass) {
						return ops.ErrUnchanged
					}
					if ongoing, _, ok := model.ParseRestoreStatus(meta.Restore); ok && ongoing {
						return ops.ErrUnchanged
					}
					if err := mdl.RestoreObject(ctx, bucket, key, int32(days), tier); err != nil {
						return err
					}
					c.trackRestore(profile, *bucket.Key, key, int32(days), tier, restorePending)
					return nil
				},
			})
		})
		form.AddButton("Cancel", func() { lay.Close() })
		form.SetInputCapture(escapeCloses(&lay))
		lay = c.pushModal(form, 78, 16)
	})
}

// storageClassOf reads an object's class; S3 omits the header for STANDARD.
func storageClassOf(m model.ObjectMeta) string {
	if m.StorageClass == "" {
		return "STANDARD"
	}
	return m.StorageClass
}

// Labels of the bulk metadata form, read back by label.
const (
	fieldBulkContentType  = "Content-Type (blank: keep)"
	fieldBulkCacheControl = "Cache-Control (blank: keep)"
	fieldBulkSetMeta      = "Set metadata (key=value per line)"
	fieldBulkDropMeta     = "Remove metadata keys"
	fieldBulkSetTags      = "Set tags (key=value per line)"
	fieldBulkDropTags     = "Remove tag keys"
)

// bulkMeta is m over the marked set.
func (c *Controller) bulkMeta() {
	c.resolveBulk("Metadata", func(bucket *model.Object, keys []string) {
		mdl := c.model
		var lay *shell.Layer
		form := tview.NewForm()
		form.SetTitle(fmt.Sprintf(" Metadata & tags — %d objects ", len(keys)))
		form.AddInputField(fieldBulkContentType, "", 50, nil, nil)
		form.AddInputField(fieldBulkCacheControl, "", 50, nil, nil)
		form.AddTextArea(fieldBulkSetMeta, "", 50, 3, 0, nil)
		form.AddInputField(fieldBulkDropMeta, "", 50, nil, nil)
		form.AddTextArea(fieldBulkSetTags, "", 50, 3, 0, nil)
		form.AddInputField(fieldBulkDropTags, "", 50, nil, nil)
		form.SetBorder(true)
		form.AddButton("Apply", func() {
			text := func(l string) string {
				switch f := form.GetFormItemByLabel(l).(type) {
				case *tview.InputField:
					return strings.TrimSpace(f.GetText())
				case *tview.TextArea:
					return f.GetText()
				}
				return ""
			}
			setMeta, err := parseKVLines(text(fieldBulkSetMeta))
			if err != nil {
				c.error("Metadata", err)
				return
			}
			setTags, err := parseKVLines(text(fieldBulkSetTags))
			if err == nil {
				err = validateTags(setTags)
			}
			if err != nil {
				c.error("Tags", err)
				return
			}
			edit := bulkMetaEdit{
				contentType: text(fieldBulkContentType), cacheControl: text(fieldBulkCacheControl),
				setMeta: setMeta, setTags: setTags,
				dropMeta: splitKeys(text(fieldBulkDropMeta)), dropTags: splitKeys(text(fieldBulkDropTags)),
			}
			if edit.empty() {
				c.error("Metadata", fmt.Errorf("nothing to change: every field is blank"))
				return
			}
			lay.Close()
			c.runner().Start(&ops.Batch{
				Verb:    "metadata & tags",
				Confirm: bulkMetaConfirm(edit, len(keys)),
				Keys:    keys,
				Apply:   func(ctx context.Context, key string) error { return applyBulkMeta(ctx, mdl, bucket, key, edit) },
			})
		})
		form.AddButton("Cancel", func() { lay.Close() })
		form.SetInputCapture(escapeCloses(&lay))
		lay = c.pushModalClamped(form, 80, 26)
		c.view.App.SetFocus(form)
	})
}

// bulkMetaPort is the slice of the model applyBulkMeta needs.
type bulkMetaPort interface {
	HeadObject(ctx context.Context, bucket *model.Object, key string) (model.ObjectMeta, error)
	PutObjectMeta(ctx context.Context, bucket *model.Object, key string, meta model.ObjectMeta) error
	ObjectTags(ctx context.Context, bucket *model.Object, key string) (map[string]string, error)
	PutObjectTags(ctx context.Context, bucket *model.Object, key string, tags map[string]string) error
}

// applyBulkMeta applies the edit to one object: a metadata rewrite only if
// the metadata changes, a tag write only if the tags do.
func applyBulkMeta(ctx context.Context, m bulkMetaPort, bucket *model.Object, key string, e bulkMetaEdit) error {
	changed := false
	if e.touchesMeta() {
		meta, err := m.HeadObject(ctx, bucket, key)
		if err != nil {
			return err
		}
		if next, ok := e.applyMeta(meta); ok {
			if err := m.PutObjectMeta(ctx, bucket, key, next); err != nil {
				return err
			}
			changed = true
		}
	}
	if e.touchesTags() {
		tags, err := m.ObjectTags(ctx, bucket, key)
		if err != nil {
			return err
		}
		next, ok := e.applyTags(tags)
		if ok {
			if err := validateTags(next); err != nil {
				return err
			}
			if err := m.PutObjectTags(ctx, bucket, key, next); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return ops.ErrUnchanged
	}
	return nil
}

// bulkMetaConfirm states the change before it runs.
func bulkMetaConfirm(e bulkMetaEdit, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Change %d object(s):\n\n", n)
	if e.contentType != "" {
		fmt.Fprintf(&b, "  Content-Type → %s\n", e.contentType)
	}
	if e.cacheControl != "" {
		fmt.Fprintf(&b, "  Cache-Control → %s\n", e.cacheControl)
	}
	for _, k := range sortedKeys(e.setMeta) {
		fmt.Fprintf(&b, "  metadata %s = %s\n", k, e.setMeta[k])
	}
	for _, k := range e.dropMeta {
		fmt.Fprintf(&b, "  metadata %s removed\n", k)
	}
	for _, k := range sortedKeys(e.setTags) {
		fmt.Fprintf(&b, "  tag %s = %s\n", k, e.setTags[k])
	}
	for _, k := range e.dropTags {
		fmt.Fprintf(&b, "  tag %s removed\n", k)
	}
	if e.touchesMeta() {
		b.WriteString("\nA metadata change rewrites each object in place (a server-side copy; on a versioned bucket, a new version).")
	}
	return b.String()
}

// sortedKeys returns m's keys in order.
func sortedKeys(m map[string]string) []string {
	lines := strings.Split(strings.TrimSpace(formatKVLines(m)), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok {
			out = append(out, k)
		}
	}
	return out
}
