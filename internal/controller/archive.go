package controller

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

// archiveBaseName proposes the archive's file name, without extension: the
// item's own name for a single file or folder, otherwise the bucket and the
// folder being browsed, stamped so a second run does not collide with the
// first.
func archiveBaseName(bucket, prefix string, sel []*model.Object, now time.Time) string {
	if len(sel) == 1 && sel[0].Key != nil && *sel[0].Key != "" {
		return *sel[0].Key
	}
	base := bucket
	if p := strings.Trim(prefix, "/"); p != "" {
		base += "-" + path.Base(p)
	}
	return base + "-" + now.Format("20060102-150405")
}

// archiveFileName applies the chosen format's extension, accepting a name that
// already ends in it, and swapping the other format's (a user who flips the
// dropdown after typing "x.zip" means x.tar.gz, not x.zip.tar.gz).
func archiveFileName(name string, format ops.ArchiveFormat) string {
	name = strings.TrimSpace(name)
	for _, f := range ops.ArchiveFormats() {
		if strings.HasSuffix(strings.ToLower(name), f.Ext()) {
			name = name[:len(name)-len(f.Ext())]
			break
		}
	}
	return name + format.Ext()
}

// validateArchiveFileName refuses names that are not a single file name. The
// directory has its own field, so a separator here is a mistake.
func validateArchiveFileName(name string) error {
	base := strings.TrimSpace(name)
	switch {
	case base == "":
		return fmt.Errorf("the archive needs a file name")
	case strings.ContainsAny(base, `/\`):
		return fmt.Errorf("%q is a path; put the directory in its own field", base)
	case base == "." || base == "..":
		return fmt.Errorf("%q is not a file name", base)
	}
	return nil
}

// archiveStartDir is where the archive is offered to land: beside the user
// in a local pane, else the profile's download directory (~/Downloads when it
// has none).
func (c *Controller) archiveStartDir() string {
	if d := c.otherPaneLocalDir(); d != "" {
		return d
	}
	return strings.TrimSuffix(c.resolveDownloadDir(), string(os.PathSeparator))
}

// DownloadArchive downloads the marked items — or the highlighted one — into a
// single .tar.gz or .zip instead of a directory tree: one file to keep, move or
// send on. Folders are expanded the way a download expands them, and entry
// names are relative to the folder being browsed.
func (c *Controller) DownloadArchive() {
	if c.remoteOnly("Download as an archive") {
		return
	}
	if c.view.List.GetItemCount() == 0 || c.pane().Bucket() == nil {
		return
	}
	srcBucket := c.pane().Bucket()
	srcPath := c.pane().Path()
	mdl := c.model
	sel := c.downloadSelection()
	if len(sel) == 0 {
		return
	}

	formats := ops.ArchiveFormats()
	labels := make([]string, len(formats))
	for i, f := range formats {
		labels[i] = string(f)
	}

	// formLay is assigned below; the buttons fire only after that.
	var formLay *shell.Layer
	form := c.view.NewArchiveForm(
		fmt.Sprintf(" Download %d item(s) as an archive ", len(sel)), labels,
		archiveBaseName(*srcBucket.Key, srcPath, sel, time.Now())+formats[0].Ext(),
		c.archiveStartDir())
	// The name follows the format, so the extension shown is the one written.
	nameField := form.GetFormItemByLabel(widgets.FieldArchiveName).(*tview.InputField)
	form.GetFormItemByLabel(widgets.FieldArchiveFormat).(*tview.DropDown).SetSelectedFunc(func(_ string, i int) {
		if i >= 0 && i < len(formats) {
			nameField.SetText(archiveFileName(nameField.GetText(), formats[i]))
		}
	})
	form.AddButton("Download", func() {
		i, _ := form.GetFormItemByLabel(widgets.FieldArchiveFormat).(*tview.DropDown).GetCurrentOption()
		if i < 0 || i >= len(formats) {
			i = 0
		}
		format := formats[i]
		name := form.GetFormItemByLabel(widgets.FieldArchiveName).(*tview.InputField).GetText()
		dir := expandHome(strings.TrimSpace(form.GetFormItemByLabel(widgets.FieldArchiveDir).(*tview.InputField).GetText()), c.params.HomeDir)
		formLay.Close()

		if err := validateArchiveFileName(name); err != nil {
			c.error("Archive", err)
			return
		}
		if dir == "" {
			c.error("Archive", fmt.Errorf("the archive needs a directory"))
			return
		}
		dest := filepath.Join(dir, archiveFileName(name, format))
		c.resolveAndArchive(mdl, srcBucket, srcPath, sel, format, dest)
	})
	form.AddButton("Cancel", func() { formLay.Close() })
	form.SetInputCapture(escapeCloses(&formLay))
	formLay = c.pushModalClamped(form, 120, 11)
}

// resolveAndArchive expands folders behind a cancellable wait and hands the
// result to the runner, which confirms, queues and reports like any transfer.
func (c *Controller) resolveAndArchive(mdl *model.Model, srcBucket *model.Object, srcPath string, sel []*model.Object, format ops.ArchiveFormat, dest string) {
	wait, _, ctx, cancel := c.cancellableWait("Resolving objects...")
	go func() {
		defer cancel()
		targets, total, err := resolveTargets(ctx, mdl, srcBucket, sel)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Archive: resolving objects failed", err)
			}
			return
		}
		if len(targets) == 0 {
			c.error("Archive", fmt.Errorf("nothing to archive: the selection holds no objects"))
			return
		}
		c.shell.Do(func() {
			c.runner().Start(&ops.Archive{
				Bucket:        srcBucket,
				SrcPrefix:     srcPath,
				Targets:       targets,
				SelectedCount: len(sel),
				TotalBytes:    total,
				Format:        format,
				Dest:          dest,
				Opener:        mdl,
				FS:            ops.OSFS{},
				Gate:          c.queue,
			})
		})
	}()
}
