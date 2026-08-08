package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// versionRow renders one version for the history list. A delete marker has no
// size and is called out explicitly — it is the entry that makes an object look
// deleted, and restoring the version below it is how you undo that.
func versionRow(v model.ObjectVersion) (primary, secondary string) {
	when := "unknown date"
	if v.LastModified != nil {
		when = v.LastModified.Format("2006-01-02 15:04:05")
	}

	marker := "  "
	if v.IsLatest {
		marker = "→ "
	}

	if v.IsDeleteMark {
		return fmt.Sprintf("%s[red]delete marker[-]  %s", marker, when), v.VersionID
	}

	size := humanize.IBytes(uint64(v.Size))
	class := ""
	if v.StorageClass != "" && !strings.EqualFold(v.StorageClass, "STANDARD") {
		class = "  " + v.StorageClass
	}
	latest := ""
	if v.IsLatest {
		latest = "  [green](latest)[-]"
	}
	return fmt.Sprintf("%s%s  %8s%s%s", marker, when, size, class, latest), v.VersionID
}

// versionsTitle summarises the history for the modal title.
func versionsTitle(key string, vs []model.ObjectVersion) string {
	var markers int
	for _, v := range vs {
		if v.IsDeleteMark {
			markers++
		}
	}
	base := fmt.Sprintf(" Versions of %s — %d total", key, len(vs))
	if markers > 0 {
		base += fmt.Sprintf(", %d delete marker(s)", markers)
	}
	return base + " "
}

// ShowVersions opens the version history of the highlighted object.
//
// The bucket must have versioning enabled for this to show more than one entry;
// on an unversioned bucket S3 reports a single "null" version, which is exactly
// what the list will show rather than pretending the feature is missing.
func (c *Controller) ShowVersions() {
	_, obj, ok := c.currentObject()
	if !ok || obj.Ot != model.File {
		return
	}
	bucket := c.currentBucket
	key := *obj.FullPath
	shortName := *obj.Key

	loading := tview.NewModal().SetText("Loading version history...")
	c.view.Pages.AddPage("progress", loading, true, true)

	go func() {
		versions, err := c.model.ListVersions(context.Background(), bucket, key)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress").SwitchToPage("main") })
			c.error("Failed to list versions", err)
			return
		}

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
			if len(versions) == 0 {
				go c.error("Versions", fmt.Errorf("no versions returned for %s", shortName))
				return
			}
			c.presentVersions(bucket, key, shortName, versions)
		})
	}()
}

// presentVersions builds the history list and its key bindings. Runs on the UI
// goroutine.
func (c *Controller) presentVersions(bucket *model.Object, key, shortName string, versions []model.ObjectVersion) {
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(versionsTitle(shortName, versions))
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	for _, v := range versions {
		primary, secondary := versionRow(v)
		list.AddItem(primary, secondary, 0, nil)
	}

	help := tview.NewTextView().SetDynamicColors(true).SetText(
		"  [::b]Enter[::-] restore as latest   [::b]w[::-] download this version   " +
			"[::b]d[::-] delete permanently   [::b]Esc[::-] close")

	selected := func() (model.ObjectVersion, bool) {
		i := list.GetCurrentItem()
		if i < 0 || i >= len(versions) {
			return model.ObjectVersion{}, false
		}
		return versions[i], true
	}

	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			c.view.Pages.RemovePage("modal-versions")
			return nil
		case tcell.KeyEnter:
			if v, ok := selected(); ok {
				c.confirmRestoreVersion(bucket, key, shortName, v)
			}
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'w':
				if v, ok := selected(); ok {
					c.downloadVersion(bucket, key, v)
				}
				return nil
			case 'd':
				if v, ok := selected(); ok {
					c.confirmDeleteVersion(bucket, key, shortName, v)
				}
				return nil
			}
		}
		return event
	})

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true).
		AddItem(help, 1, 0, false)

	c.view.Pages.AddPage("modal-versions", c.view.ModalEdit(flex, 84, 24), true, true)
	c.view.App.SetFocus(list)
}

// confirmRestoreVersion explains what restoring does before doing it: it copies
// the chosen version to the top of the history rather than rewinding, so
// nothing is lost either way.
func (c *Controller) confirmRestoreVersion(bucket *model.Object, key, shortName string, v model.ObjectVersion) {
	if v.IsLatest && !v.IsDeleteMark {
		go c.error("Restore version", fmt.Errorf("that version is already the latest"))
		return
	}
	if v.IsDeleteMark {
		go c.error("Restore version", fmt.Errorf("a delete marker cannot be restored; delete the marker instead (d) to bring back the version under it"))
		return
	}

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf(
		"Make this version of %s current?\n\nVersion: %s\n\nThis copies it to the top of the history — no version is removed.",
		shortName, v.VersionID)).
		SetDoneFunc(func(_ int, label string) {
			c.view.Pages.RemovePage("confirm")
			if label != "OK" {
				return
			}
			c.view.Pages.RemovePage("modal-versions")
			go func() {
				if err := c.model.RestoreVersion(context.Background(), bucket, key, v.VersionID, v.StorageClass); err != nil {
					c.error("Failed to restore version", err)
					return
				}
				c.logActivity("Version restored: %s → %s", v.VersionID, key)
				c.success("Version restored as the current object")
				c.updateList()
			}()
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
}

// confirmDeleteVersion gates the one genuinely destructive action in this
// screen: deleting a version bypasses the delete-marker mechanism and the data
// is unrecoverable.
func (c *Controller) confirmDeleteVersion(bucket *model.Object, key, shortName string, v model.ObjectVersion) {
	what := "version"
	extra := "\n\n[red]This is permanent — a version delete leaves no delete marker."
	if v.IsDeleteMark {
		what = "delete marker"
		extra = "\n\nRemoving the delete marker makes the previous version current again."
	}

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Permanently delete this %s of %s?\n\nVersion: %s%s",
		what, shortName, v.VersionID, extra)).
		SetDoneFunc(func(_ int, label string) {
			c.view.Pages.RemovePage("confirm")
			if label != "OK" {
				return
			}
			c.view.Pages.RemovePage("modal-versions")
			go func() {
				if err := c.model.DeleteVersion(context.Background(), bucket, key, v.VersionID); err != nil {
					c.error("Failed to delete version", err)
					return
				}
				c.logActivity("Version deleted: %s of %s", v.VersionID, key)
				c.success(fmt.Sprintf("Deleted %s %s", what, v.VersionID))
				c.updateList()
			}()
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
}

// downloadVersion saves one version next to the normal downloads, under a name
// carrying the version id so several versions can coexist.
func (c *Controller) downloadVersion(bucket *model.Object, key string, v model.ObjectVersion) {
	if v.IsDeleteMark {
		go c.error("Download version", fmt.Errorf("a delete marker has no content to download"))
		return
	}

	dest := c.resolveDownloadDir()
	name := model.VersionFileName(key, v.VersionID)
	c.view.Pages.RemovePage("modal-versions")

	go func() {
		n, err := c.model.DownloadVersion(context.Background(), bucket, key, v.VersionID, dest, name)
		if err != nil {
			c.error("Failed to download version", err)
			return
		}
		c.logActivity("Version downloaded: %s → %s (%s)", v.VersionID, name, humanize.IBytes(uint64(n)))
		c.success(fmt.Sprintf("Saved %s (%s)", name, humanize.IBytes(uint64(n))))
	}()
}
