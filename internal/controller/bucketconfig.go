package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// S3's bucket tag limits. Higher than an object's 10: a bucket carries cost
// allocation and ownership tags.
const (
	maxBucketTags = 50
	// awsTagPrefix marks system tags (CloudFormation stamps aws:cloudformation:*
	// on the buckets it creates). PutBucketTagging refuses them, so a set that
	// holds one cannot be written back at all.
	awsTagPrefix = "aws:"
)

// configItemLine renders one dashboard row: the label, then the value or the
// reason there is none, coloured by how the answer was arrived at.
func configItemLine(label string, item model.ConfigItem) string {
	var v string
	switch item.State {
	case model.ConfigSet:
		v = "[white]" + tview.Escape(item.Value) + "[-]"
	case model.ConfigUnset:
		v = "[gray]" + tview.Escape(item.Value) + "[-]"
	case model.ConfigUnsupported:
		v = "[gray]not supported by this endpoint[-]"
	case model.ConfigDenied:
		v = "[red]access denied[-]"
	default:
		msg := "unknown error"
		if item.Err != nil {
			msg = item.Err.Error()
		}
		v = "[red]could not read: " + tview.Escape(msg) + "[-]"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[green]%-20s[-] %s\n", label, v)
	for _, d := range item.Detail {
		fmt.Fprintf(&b, "    %s\n", tview.Escape(d))
	}
	return b.String()
}

// bucketDashboardText renders the whole dashboard. Pure, so every section's
// wording is testable without an endpoint.
func bucketDashboardText(name string, info model.BucketConfigInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[green]%-20s[-] [white]%s[-]\n", "Bucket", tview.Escape(name))
	fmt.Fprintf(&b, "[green]%-20s[-] [white]%s[-]\n\n", "Region", tview.Escape(info.Region))
	for _, s := range []struct {
		label string
		item  model.ConfigItem
	}{
		{"Versioning", info.Versioning},
		{"Default encryption", info.Encryption},
		{"Object lock", info.ObjectLock},
		{"Public access block", info.PublicAccessBlock},
		{"Tags", info.Tags},
		{"Lifecycle", info.Lifecycle},
		{"Replication", info.Replication},
		{"CORS", info.CORS},
		{"Website hosting", info.Website},
		{"Event notifications", info.Notifications},
		{"Access logging", info.Logging},
		// Last: a policy is the one section that runs to many lines.
		{"Policy", info.Policy},
	} {
		b.WriteString(configItemLine(s.label, s.item))
	}
	return b.String()
}

// validateBucketTags applies S3's bucket tag limits, plus the one rule the
// service enforces that is easy to trip over: no aws: keys.
func validateBucketTags(tags map[string]string) error {
	if len(tags) > maxBucketTags {
		return fmt.Errorf("%d tags exceeds the S3 limit of %d per bucket", len(tags), maxBucketTags)
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(strings.ToLower(k), awsTagPrefix) {
			return fmt.Errorf("tag key %q: the aws: prefix is reserved for AWS", k)
		}
		if n := utf8.RuneCountInString(k); n > maxTagKeyLen {
			return fmt.Errorf("tag key %q is %d chars, over the %d limit", k, n, maxTagKeyLen)
		}
		if n := utf8.RuneCountInString(tags[k]); n > maxTagValueLen {
			return fmt.Errorf("value of tag %q is %d chars, over the %d limit", k, n, maxTagValueLen)
		}
	}
	return nil
}

// systemTags lists the aws: tags in a set. A bucket carrying any cannot have
// its tags rewritten from here: PutBucketTagging replaces the whole set and
// refuses aws: keys, so a save would have to drop them.
func systemTags(tags map[string]string) []string {
	var out []string
	for k := range tags {
		if strings.HasPrefix(strings.ToLower(k), awsTagPrefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// BucketDashboard shows the current bucket's configuration: versioning,
// encryption, object lock, public access block, tags, lifecycle, replication,
// CORS, website hosting, event notifications, access logging and the policy.
// t edits the tags; r reads everything again.
func (c *Controller) BucketDashboard() {
	if c.remoteOnly("The bucket dashboard") {
		return
	}
	if c.pane().Bucket() == nil {
		return
	}
	c.loadBucketDashboard(c.pane().Bucket(), c.model)
}

// loadBucketDashboard reads the configuration behind a cancellable wait and
// opens the overlay.
func (c *Controller) loadBucketDashboard(bucket *model.Object, mdl *model.Model) {
	wait, _, ctx, cancel := c.cancellableWait(fmt.Sprintf("Reading the configuration of %s ...", *bucket.Key))
	go func() {
		defer cancel()
		info, err := mdl.BucketConfig(ctx, bucket)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Bucket config", err)
			}
			return
		}
		c.showTextOverlay(" Bucket config — "+tview.Escape(*bucket.Key)+" ",
			bucketDashboardText(*bucket.Key, info),
			"[gray]↑↓/PgUp/PgDn[white] scroll · [gray]t[white] edit tags · [gray]r[white] reload · [gray]Esc/q[white] close",
			func(ev *tcell.EventKey, lay *shell.Layer, _ *tview.TextView) bool {
				if ev.Key() != tcell.KeyRune {
					return false
				}
				switch ev.Rune() {
				case 'r':
					lay.Close()
					c.loadBucketDashboard(bucket, mdl)
					return true
				case 't':
					c.editBucketTags(bucket, mdl, info, lay)
					return true
				}
				return false
			})
	}()
}

// editBucketTags opens the tag editor over the dashboard, and reopens the
// dashboard once the save lands so it shows what the bucket now holds.
func (c *Controller) editBucketTags(bucket *model.Object, mdl *model.Model, info model.BucketConfigInfo, dash *shell.Layer) {
	if c.readOnlyBlocked("edit bucket tags") {
		return
	}
	switch {
	case info.TagSet == nil:
		c.error("Bucket tags", fmt.Errorf("the tags could not be read (%s), so they cannot be edited safely", bucketTagsState(info.Tags)))
		return
	case len(systemTags(info.TagSet)) > 0:
		c.error("Bucket tags", fmt.Errorf(
			"this bucket carries AWS system tags (%s). Saving replaces the whole set and S3 refuses aws: keys, so they would be lost — edit these tags with a tool that preserves them",
			strings.Join(systemTags(info.TagSet), ", ")))
		return
	}

	// lay is assigned below; the buttons fire only after that.
	var lay *shell.Layer
	orig := formatKVLines(info.TagSet)
	form := tview.NewForm()
	form.SetTitle(" Tags — " + tview.Escape(*bucket.Key) + " ")
	form.AddTextArea("Tags (key=value per line)", orig, 60, 12, 0, nil)
	form.SetBorder(true)
	form.AddButton("Save", func() {
		text := form.GetFormItem(0).(*tview.TextArea).GetText()
		lay.Close()
		tags, err := parseKVLines(text)
		if err == nil {
			err = validateBucketTags(tags)
		}
		if err != nil {
			c.error("Invalid tags", err)
			return
		}
		if formatKVLines(tags) == orig {
			c.success("Nothing changed")
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), bucketCallTimeout)
			defer cancel()
			if err := mdl.PutBucketTags(ctx, bucket, tags); err != nil {
				c.error("Saving the bucket tags failed", err)
				return
			}
			c.logActivity("Bucket tags: %s now has %d tag(s)", *bucket.Key, len(tags))
			c.shell.Do(func() {
				dash.Close()
				c.loadBucketDashboard(bucket, mdl)
			})
		}()
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 80, 18)
}

// bucketCallTimeout bounds a single bucket-level write. It has no progress to
// show, so a wait with no end in sight would only look like a hang.
const bucketCallTimeout = 30 * time.Second

// bucketTagsState names why the tags are not editable.
func bucketTagsState(item model.ConfigItem) string {
	switch item.State {
	case model.ConfigUnsupported:
		return "not supported by this endpoint"
	case model.ConfigDenied:
		return "access denied"
	case model.ConfigError:
		if item.Err != nil {
			return item.Err.Error()
		}
	}
	return "unknown"
}
