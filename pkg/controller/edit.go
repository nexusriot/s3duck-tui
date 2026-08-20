package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// editMaxSize caps what the editor flow will load: the object is held in
// memory twice (original and edited), and $EDITOR is for configs and notes,
// not gigabyte logs.
const editMaxSize = 1 << 20 // 1 MiB

// binarySniffLen is how much of the head is inspected for NUL bytes — the same
// heuristic git uses to classify a file as binary.
const binarySniffLen = 8000

// isProbablyBinary reports whether data looks like something no text editor
// should be pointed at.
func isProbablyBinary(data []byte) bool {
	n := len(data)
	if n > binarySniffLen {
		n = binarySniffLen
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// editorCommand resolves the editor to launch: $EDITOR, then $VISUAL, then vi.
// The value may carry arguments ("emacs -nw"), so it is split into fields.
func editorCommand(getenv func(string) string) (name string, args []string, err error) {
	for _, v := range []string{"EDITOR", "VISUAL"} {
		if fields := strings.Fields(getenv(v)); len(fields) > 0 {
			return fields[0], fields[1:], nil
		}
	}
	if _, lookErr := exec.LookPath("vi"); lookErr != nil {
		return "", nil, fmt.Errorf("$EDITOR and $VISUAL are unset and vi is not in PATH")
	}
	return "vi", nil, nil
}

// tempSuffix derives a safe filename suffix from the object key, so the editor
// gets its syntax highlighting. Anything but a short, plain extension is
// dropped rather than sanitized.
var tempSuffixRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

func tempSuffix(key string) string {
	ext := path.Ext(path.Base(key))
	if tempSuffixRe.MatchString(ext) {
		return ext
	}
	return ""
}

// EditObject opens the highlighted object in $EDITOR: download to a temp file,
// suspend the TUI while the editor owns the terminal, and re-upload on change
// with the Content-Type and user metadata preserved. Guarded by a size cap and
// a binary sniff — this is for configs and notes, not archives.
func (c *Controller) EditObject() {
	_, obj, ok := c.currentObject()
	if !ok || obj.Ot != model.File {
		return
	}
	bucket := c.currentBucket
	key := *obj.FullPath

	if obj.Size != nil && *obj.Size > editMaxSize {
		go c.error("Edit", fmt.Errorf("%s is %s; the in-editor cap is %s",
			*obj.Key, humanize.IBytes(uint64(*obj.Size)), humanize.IBytes(editMaxSize)))
		return
	}

	editor, editorArgs, err := editorCommand(os.Getenv)
	if err != nil {
		go c.error("Edit", err)
		return
	}
	mdl := c.model

	go func() {
		content, err := mdl.GetObjectContent(context.Background(), bucket, key, editMaxSize)
		if err != nil {
			c.error("Edit: download failed", err)
			return
		}
		if isProbablyBinary(content.Data) {
			c.error("Edit", fmt.Errorf("%s looks binary; refusing to open it in a text editor", key))
			return
		}

		tmp, err := os.CreateTemp("", "s3duck-edit-*"+tempSuffix(key))
		if err != nil {
			c.error("Edit", err)
			return
		}
		tmpPath := tmp.Name()
		// The temp file is removed on every path EXCEPT a failed or refused
		// upload — the user's edit must survive somewhere they can be told
		// about, or a save conflict would destroy their work.
		keepTemp := false
		defer func() {
			if !keepTemp {
				os.Remove(tmpPath)
			}
		}()

		// CreateTemp already made the file 0600 — no other user can read the
		// object body while the editor holds it.
		_, werr := tmp.Write(content.Data)
		if closeErr := tmp.Close(); werr == nil {
			werr = closeErr
		}
		if werr != nil {
			c.error("Edit: writing temp file", werr)
			return
		}

		// Suspend hands the terminal to the editor and restores the TUI when it
		// exits. Safe from this goroutine: Suspend takes the app's own locks.
		var runErr error
		suspended := c.view.App.Suspend(func() {
			cmd := exec.Command(editor, append(editorArgs, tmpPath)...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			runErr = cmd.Run()
		})
		// Repaint after the editor owned the screen.
		c.view.App.QueueUpdateDraw(func() {})

		if !suspended {
			c.error("Edit", fmt.Errorf("could not suspend the UI"))
			return
		}
		if runErr != nil {
			c.error(fmt.Sprintf("Edit: %s failed", editor), runErr)
			return
		}

		edited, err := os.ReadFile(tmpPath)
		if err != nil {
			c.error("Edit: reading temp file back", err)
			return
		}
		if bytes.Equal(edited, content.Data) {
			c.success("Not modified")
			return
		}

		// Lost-update guard: the editor may have been open for minutes, and a
		// backgrounded sync or another writer can rewrite the key meanwhile.
		// Refuse to overwrite a version we never saw — the edit is preserved
		// in the temp file, whose path the error reports. (A HEAD-compare,
		// not If-Match: conditional PUT support is spotty across
		// S3-compatible backends.)
		if cur, headErr := mdl.CurrentETag(context.Background(), bucket, key); headErr == nil &&
			content.ETag != "" && cur != content.ETag {
			keepTemp = true
			c.error("Edit: not saved", fmt.Errorf(
				"%s changed on the server while you were editing; your version is kept at %s", key, tmpPath))
			return
		}

		if err := mdl.PutBytes(context.Background(), bucket, key, edited, content); err != nil {
			keepTemp = true
			c.error("Edit: upload failed", fmt.Errorf("%v\nyour version is kept at %s", err, tmpPath))
			return
		}
		c.logActivity("Edited in %s: %s (%s → %s)", editor, key,
			humanize.IBytes(uint64(len(content.Data))), humanize.IBytes(uint64(len(edited))))
		c.success(fmt.Sprintf("Saved %s (%s)", key, humanize.IBytes(uint64(len(edited)))))
		c.updateList()
	}()
}
