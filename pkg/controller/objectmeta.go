package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

// S3 tag limits, enforced before the request so the user gets a precise message
// instead of a generic InvalidTag from the service.
const (
	maxObjectTags    = 10
	maxTagKeyLen     = 128
	maxTagValueLen   = 256
	metaEditorHeight = 26
	// metaFormPage is deliberately NOT "modal": c.error adds its report as
	// page "modal", and tview's AddPage replaces a page of the same name — so
	// a validation error raised while the editor is open would destroy the
	// form and everything typed into it.
	metaFormPage = "modal-meta"
)

// parseKVLines reads "key=value" lines into a map, skipping blank lines and
// "#" comments. The first "=" separates; later ones belong to the value. A line
// without "=" or with an empty key is an error rather than being dropped, so a
// typo can't silently discard a pair the user meant to set.
func parseKVLines(text string) (map[string]string, error) {
	out := map[string]string{}
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected key=value, got %q", i+1, line)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", i+1, k)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// formatKVLines renders a map as sorted "key=value" lines, so reopening the
// editor shows a stable order rather than Go's randomized map iteration.
func formatKVLines(kv map[string]string) string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	return b.String()
}

// validateTags applies S3's tag-set limits.
func validateTags(tags map[string]string) error {
	if len(tags) > maxObjectTags {
		return fmt.Errorf("%d tags exceeds the S3 limit of %d", len(tags), maxObjectTags)
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(k) > maxTagKeyLen {
			return fmt.Errorf("tag key %q is %d chars, over the %d limit", k, len(k), maxTagKeyLen)
		}
		if v := tags[k]; len(v) > maxTagValueLen {
			return fmt.Errorf("value of tag %q is %d chars, over the %d limit", k, len(v), maxTagValueLen)
		}
	}
	return nil
}

// metaSummary renders the read-only facts shown above the editable fields.
func metaSummary(meta model.ObjectMeta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  •  etag %s", humanize.IBytes(uint64(meta.Size)), meta.ETag)
	if meta.LastModified != nil {
		fmt.Fprintf(&b, "  •  %s", meta.LastModified.Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(&b, "  •  %s", meta.StorageClass)
	if r := model.RestoreSummary(meta.StorageClass, meta.Restore); r != "" {
		fmt.Fprintf(&b, "  •  %s", r)
	}
	return b.String()
}

// EditObjectMeta opens the metadata + tags editor for the highlighted object.
// Both are fetched together because they come from two different calls but read
// as one concept, and saving writes only the halves that actually changed —
// a metadata save is a full object copy, so it is not worth doing for a
// tags-only edit.
func (c *Controller) EditObjectMeta() {
	_, obj, ok := c.currentObject()
	if !ok || obj.Ot != model.File {
		return
	}
	bucket := c.currentBucket
	key := *obj.FullPath

	loading := tview.NewModal().SetText("Loading metadata...")
	c.view.Pages.AddPage("progress", loading, true, true)

	go func() {
		ctx := context.Background()
		meta, err := c.model.HeadObject(ctx, bucket, key)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress").SwitchToPage("main") })
			c.error("Failed to read metadata", err)
			return
		}
		// Tagging is optional on S3-compatible backends; treat a failure as
		// "no tags" rather than blocking the whole editor.
		tags, tagErr := c.model.ObjectTags(ctx, bucket, key)
		if tagErr != nil {
			tags = map[string]string{}
		}

		origMeta := meta
		origTags := formatKVLines(tags)

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress").SwitchToPage("main")

			form := c.view.NewObjectMetaForm(key, metaSummary(meta), tagErr != nil)
			input := func(label string) *tview.InputField {
				return form.GetFormItemByLabel(label).(*tview.InputField)
			}
			area := func(label string) *tview.TextArea {
				return form.GetFormItemByLabel(label).(*tview.TextArea)
			}

			input(view.FieldContentType).SetText(meta.ContentType)
			input(view.FieldCacheControl).SetText(meta.CacheControl)
			input(view.FieldContentDisposition).SetText(meta.ContentDisposition)
			input(view.FieldContentEncoding).SetText(meta.ContentEncoding)
			area(view.FieldUserMetadata).SetText(formatKVLines(meta.UserMetadata), false)
			area(view.FieldTags).SetText(origTags, false)

			form.AddButton("Save", func() {
				newMeta := origMeta
				newMeta.ContentType = strings.TrimSpace(input(view.FieldContentType).GetText())
				newMeta.CacheControl = strings.TrimSpace(input(view.FieldCacheControl).GetText())
				newMeta.ContentDisposition = strings.TrimSpace(input(view.FieldContentDisposition).GetText())
				newMeta.ContentEncoding = strings.TrimSpace(input(view.FieldContentEncoding).GetText())

				userMeta, err := parseKVLines(area(view.FieldUserMetadata).GetText())
				if err != nil {
					go c.error("Invalid metadata", err)
					return
				}
				newMeta.UserMetadata = userMeta

				tagsText := area(view.FieldTags).GetText()
				newTags, err := parseKVLines(tagsText)
				if err != nil {
					go c.error("Invalid tags", err)
					return
				}
				if err := validateTags(newTags); err != nil {
					go c.error("Invalid tags", err)
					return
				}

				metaChanged := !sameMeta(origMeta, newMeta)
				tagsChanged := formatKVLines(newTags) != origTags

				c.view.Pages.RemovePage(metaFormPage)
				if !metaChanged && !tagsChanged {
					go c.success("Nothing changed")
					return
				}
				c.saveObjectMeta(bucket, key, newMeta, newTags, metaChanged, tagsChanged)
			})
			form.AddButton("Cancel", func() { c.view.Pages.RemovePage(metaFormPage) })

			c.view.Pages.AddPage(metaFormPage, c.view.ModalEdit(form, 80, metaEditorHeight), true, true)
		})
	}()
}

// sameMeta compares only the fields the editor can change.
func sameMeta(a, b model.ObjectMeta) bool {
	if a.ContentType != b.ContentType ||
		a.CacheControl != b.CacheControl ||
		a.ContentDisposition != b.ContentDisposition ||
		a.ContentEncoding != b.ContentEncoding {
		return false
	}
	return formatKVLines(a.UserMetadata) == formatKVLines(b.UserMetadata)
}

// saveObjectMeta writes the changed halves and reports what it did.
func (c *Controller) saveObjectMeta(bucket *model.Object, key string, meta model.ObjectMeta, tags map[string]string, metaChanged, tagsChanged bool) {
	go func() {
		ctx := context.Background()
		var done []string

		if metaChanged {
			if err := c.model.PutObjectMeta(ctx, bucket, key, meta); err != nil {
				c.error("Failed to save metadata", err)
				return
			}
			done = append(done, "metadata")
		}
		if tagsChanged {
			if err := c.model.PutObjectTags(ctx, bucket, key, tags); err != nil {
				c.error("Failed to save tags", err)
				return
			}
			done = append(done, fmt.Sprintf("%d tag(s)", len(tags)))
		}

		c.logActivity("Metadata: updated %s on %s", strings.Join(done, " + "), key)
		c.success("Saved " + strings.Join(done, " + "))
		c.updateList()
	}()
}

// ChangeStorageClass opens the storage-class / restore dialog for the
// highlighted object. Restoring is offered only for archived objects, since
// RestoreObject is meaningless (and an error) for the others.
func (c *Controller) ChangeStorageClass() {
	_, obj, ok := c.currentObject()
	if !ok || obj.Ot != model.File {
		return
	}
	bucket := c.currentBucket
	key := *obj.FullPath

	loading := tview.NewModal().SetText("Reading storage class...")
	c.view.Pages.AddPage("progress", loading, true, true)

	go func() {
		meta, err := c.model.HeadObject(context.Background(), bucket, key)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress").SwitchToPage("main") })
			c.error("Failed to read storage class", err)
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress").SwitchToPage("main")

			archived := model.IsArchived(meta.StorageClass)
			form := c.view.NewStorageClassForm(key, metaSummary(meta),
				model.StorageClasses(), meta.StorageClass,
				model.RestoreTiers(), archived)

			form.AddButton("Apply class", func() {
				_, class := form.GetFormItemByLabel(view.FieldStorageClass).(*tview.DropDown).GetCurrentOption()
				c.view.Pages.RemovePage("modal")
				if class == meta.StorageClass {
					go c.error("Storage class", fmt.Errorf("already %s", class))
					return
				}
				c.runStorageClassChange(bucket, key, class)
			})

			if archived {
				form.AddButton("Restore", func() {
					daysText := strings.TrimSpace(form.GetFormItemByLabel(view.FieldRestoreDays).(*tview.InputField).GetText())
					_, tier := form.GetFormItemByLabel(view.FieldRestoreTier).(*tview.DropDown).GetCurrentOption()
					c.view.Pages.RemovePage("modal")

					days, err := strconv.Atoi(daysText)
					if err != nil || days < 1 {
						go c.error("Restore", fmt.Errorf("days must be a positive number, got %q", daysText))
						return
					}
					c.runRestore(bucket, key, int32(days), tier)
				})
			}
			form.AddButton("Cancel", func() { c.view.Pages.RemovePage("modal") })

			c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 78, 16), true, true)
		})
	}()
}

// runStorageClassChange applies a class change (a server-side copy in place).
func (c *Controller) runStorageClassChange(bucket *model.Object, key, class string) {
	go func() {
		if err := c.model.SetStorageClass(context.Background(), bucket, key, class); err != nil {
			c.error("Failed to change storage class", err)
			return
		}
		c.logActivity("Storage class: %s → %s", key, class)
		c.success(fmt.Sprintf("Storage class set to %s", class))
		c.updateList()
	}()
}

// runRestore requests a Glacier restore. The request is asynchronous on S3's
// side — completion can take minutes to hours — so this only reports that the
// request was accepted; progress shows up in the object's restore status.
func (c *Controller) runRestore(bucket *model.Object, key string, days int32, tier string) {
	go func() {
		if err := c.model.RestoreObject(context.Background(), bucket, key, days, tier); err != nil {
			c.error("Restore failed", err)
			return
		}
		c.logActivity("Restore requested: %s (%d day(s), %s)", key, days, tier)
		c.success(fmt.Sprintf("Restore requested (%d day(s), %s).\nCheck the status with Ctrl+L or 'c'.", days, tier))
	}()
}
