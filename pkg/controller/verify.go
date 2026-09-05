package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// verifyDownloads reports whether the active profile asks for a post-transfer
// integrity check.
func (c *Controller) verifyDownloads() bool {
	return c.activeConfig != nil && c.activeConfig.VerifyDownloads
}

// verifyDownloaded checks a just-downloaded object against the file on disk.
// The local path is derived exactly as the download derived it, so the two can
// never disagree about which file was written.
func (c *Controller) verifyDownloaded(ctx context.Context, mdl *model.Model, bucket *model.Object, t model.DownloadTarget, srcPath, cwd string) (model.VerifyResult, error) {
	local, err := model.SafeLocalPath(cwd, srcPath, t.Key)
	if err != nil {
		return model.VerifyResult{}, err
	}
	return mdl.VerifyLocalFile(ctx, bucket, t.Key, local)
}

// VerifyObject compares the highlighted object against its local copy in the
// download directory — the standalone half of the post-transfer verify, for
// answering "is the file I already have still the file in the bucket?"
//
// It is also the honest place to learn that an object is *not* verifiable: a
// multipart upload with no additional checksum can only be compared by size,
// and the report says so rather than implying a byte-for-byte match.
func (c *Controller) VerifyObject() {
	if c.remoteOnly("Verify") {
		return
	}
	if c.currentBucket == nil {
		go c.error("Verify", fmt.Errorf("open a bucket first"))
		return
	}
	name, obj, ok := c.currentObject()
	if !ok || obj == nil || obj.Ot != model.File || obj.FullPath == nil {
		go c.error("Verify", fmt.Errorf("highlight a file: a folder has no single checksum"))
		return
	}
	_ = name

	key := *obj.FullPath
	bucket := c.currentBucket
	mdl := c.model
	dir := c.resolveDownloadDir()
	srcPath := c.currentPath

	local, err := model.SafeLocalPath(dir, srcPath, key)
	if err != nil {
		go c.error("Verify", err)
		return
	}
	if _, err := os.Stat(local); err != nil {
		go c.error("Verify", fmt.Errorf(
			"no local copy at %s — download it first (the check compares the object against the file in the download directory)", local))
		return
	}

	_, ctx, cancel := c.cancellableWait("progress", fmt.Sprintf("Verifying %s ...", filepath.Base(local)))
	go func() {
		defer cancel()
		res, err := mdl.VerifyLocalFile(ctx, bucket, key, local)
		c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
		if err != nil {
			if ctx.Err() == nil {
				c.error("Verify failed", err)
			}
			return
		}
		c.logActivity("Verify %s: %s", key, res.String())
		c.view.App.QueueUpdateDraw(func() {
			m := tview.NewModal().AddButtons([]string{"OK"})
			m.SetText(fmt.Sprintf("%s\n\nlocal: %s\n\n%s", key, local, res.String()))
			if res.Mismatch {
				// A mismatch means the transfer completed and the bytes are
				// wrong, which is the one outcome here that must not be
				// mistaken for a routine confirmation.
				m.SetBackgroundColor(tcell.ColorDarkRed)
			}
			m.SetDoneFunc(func(int, string) {
				c.view.Pages.RemovePage("modal-msg")
			})
			c.view.Pages.AddPage("modal-msg", c.view.ModalEdit(m, 90, 14), true, true)
		})
	}()
}
