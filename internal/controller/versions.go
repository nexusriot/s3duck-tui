package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
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
	base := fmt.Sprintf(" Versions of %s — %d total", tview.Escape(key), len(vs))
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
	if c.remoteOnly("Version history") {
		return
	}
	_, obj, ok := c.currentObject()
	if !ok || obj.Ot != model.File {
		return
	}
	bucket := c.pane().Bucket()
	key := *obj.FullPath
	shortName := *obj.Key

	wait := c.wait("Loading version history...")

	mdl := c.model
	go func() {
		versions, err := mdl.ListVersions(context.Background(), bucket, key)
		if err != nil {
			c.shell.Do(wait.Close)
			c.error("Failed to list versions", err)
			return
		}

		c.shell.Do(func() {
			wait.Close()
			if len(versions) == 0 {
				c.error("Versions", fmt.Errorf("no versions returned for %s", shortName))
				return
			}
			c.presentVersions(bucket, key, shortName, versions)
		})
	}()
}

// presentVersions builds the history list and its key bindings. Runs on the UI
// goroutine.
func (c *Controller) presentVersions(bucket *model.Object, key, shortName string, versions []model.ObjectVersion) {
	// lay is assigned at the bottom; the key handlers below run only after.
	var lay *shell.Layer
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(versionsTitle(shortName, versions))
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	for _, v := range versions {
		primary, secondary := versionRow(v)
		list.AddItem(primary, secondary, 0, nil)
	}

	help := tview.NewTextView().SetDynamicColors(true).SetText(
		"  [::b]Enter[::-] restore as latest   [::b]w[::-] download   [::b]D[::-] diff vs current   " +
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
			lay.Close()
			return nil
		case tcell.KeyEnter:
			if v, ok := selected(); ok {
				c.confirmRestoreVersion(bucket, key, shortName, v, lay)
			}
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'w':
				if v, ok := selected(); ok {
					c.downloadVersion(bucket, key, v, lay)
				}
				return nil
			case 'd':
				if v, ok := selected(); ok {
					c.confirmDeleteVersion(bucket, key, shortName, v, lay)
				}
				return nil
			case 'D':
				// "What actually changed?" is the question a version list
				// invites and could not answer: restoring blind was the only
				// way to find out.
				if v, ok := selected(); ok {
					if v.IsDeleteMark {
						c.error("Diff", fmt.Errorf("a delete marker has no content to diff"))
						return nil
					}
					lay.Close()
					c.diffVersions(bucket, key, v)
				}
				return nil
			}
		}
		return event
	})

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true).
		AddItem(help, 1, 0, false)

	lay = c.pushModal(flex, 84, 24)
	c.view.App.SetFocus(list)
}

// confirmRestoreVersion explains what restoring does before doing it: it copies
// the chosen version to the top of the history rather than rewinding, so
// nothing is lost either way.
func (c *Controller) confirmRestoreVersion(bucket *model.Object, key, shortName string, v model.ObjectVersion, history *shell.Layer) {
	var confirmLay *shell.Layer
	if c.readOnlyBlocked("restore a version") {
		return
	}
	if v.IsLatest && !v.IsDeleteMark {
		c.error("Restore version", fmt.Errorf("that version is already the latest"))
		return
	}
	if v.IsDeleteMark {
		c.error("Restore version", fmt.Errorf("a delete marker cannot be restored; delete the marker instead (d) to bring back the version under it"))
		return
	}

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf(
		"Make this version of %s current?\n\nVersion: %s\n\nThis copies it to the top of the history — no version is removed.",
		tview.Escape(shortName), v.VersionID)).
		SetDoneFunc(func(_ int, label string) {
			confirmLay.Close()
			if label != widgets.ConfirmOK {
				return
			}
			history.Close()
			mdl := c.model
			go func() {
				if err := mdl.RestoreVersion(context.Background(), bucket, key, v.VersionID, v.StorageClass); err != nil {
					c.error("Failed to restore version", err)
					return
				}
				c.logActivity("Version restored: %s → %s", v.VersionID, key)
				c.success("Version restored as the current object")
				c.updateList()
			}()
		})
	confirmLay = c.pushRaw(confirm)
}

// confirmDeleteVersion gates the one genuinely destructive action in this
// screen: deleting a version bypasses the delete-marker mechanism and the data
// is unrecoverable.
func (c *Controller) confirmDeleteVersion(bucket *model.Object, key, shortName string, v model.ObjectVersion, history *shell.Layer) {
	var confirmLay *shell.Layer
	if c.readOnlyBlocked("delete a version") {
		return
	}
	what := "version"
	extra := "\n\n[red]This is permanent — a version delete leaves no delete marker."
	if v.IsDeleteMark {
		what = "delete marker"
		extra = "\n\nRemoving the delete marker makes the previous version current again."
	}

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Permanently delete this %s of %s?\n\nVersion: %s%s",
		what, tview.Escape(shortName), v.VersionID, extra)).
		SetDoneFunc(func(_ int, label string) {
			confirmLay.Close()
			if label != widgets.ConfirmOK {
				return
			}
			history.Close()
			mdl := c.model
			go func() {
				if err := mdl.DeleteVersion(context.Background(), bucket, key, v.VersionID); err != nil {
					c.error("Failed to delete version", err)
					return
				}
				c.logActivity("Version deleted: %s of %s", v.VersionID, key)
				c.success(fmt.Sprintf("Deleted %s %s", what, v.VersionID))
				c.updateList()
			}()
		})
	confirmLay = c.pushRaw(confirm)
}

// downloadVersion saves one version next to the normal downloads, under a name
// carrying the version id so several versions can coexist.
func (c *Controller) downloadVersion(bucket *model.Object, key string, v model.ObjectVersion, history *shell.Layer) {
	if v.IsDeleteMark {
		c.error("Download version", fmt.Errorf("a delete marker has no content to download"))
		return
	}

	dest := c.resolveDownloadDir()
	name := model.VersionFileName(key, v.VersionID)
	history.Close()

	mdl := c.model
	go func() {
		n, err := mdl.DownloadVersion(context.Background(), bucket, key, v.VersionID, dest, name)
		if err != nil {
			c.error("Failed to download version", err)
			return
		}
		c.logActivity("Version downloaded: %s → %s (%s)", v.VersionID, name, humanize.IBytes(uint64(n)))
		c.success(fmt.Sprintf("Saved %s (%s)", name, humanize.IBytes(uint64(n))))
	}()
}
