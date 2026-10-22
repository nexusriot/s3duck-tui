package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// localDiffCandidate is where the local copy of key most likely is: next to
// the user in an open local pane (by base name — the pane shows one directory),
// otherwise at the path a download would have written it to.
func localDiffCandidate(localPaneDir, downloadDir, prefix, key string) (string, error) {
	if localPaneDir != "" {
		return filepath.Join(localPaneDir, path.Base(key)), nil
	}
	return model.SafeLocalPath(downloadDir, prefix, key)
}

// readLocalCapped reads a whole local file, refusing one over max — the same
// cap the object side gets, and for the same reason: both bodies are held in
// memory and walked line by line.
func readLocalCapped(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", p)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is %s, over the %s diff cap", p, humanize.IBytes(uint64(st.Size())), humanize.IBytes(uint64(max)))
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s grew past the %s diff cap while being read", p, humanize.IBytes(uint64(max)))
	}
	return data, nil
}

// localDiffOutcome is what comparing the two bodies came to, decided without
// a terminal so every branch is testable.
type localDiffOutcome struct {
	// identical: byte for byte.
	identical bool
	// message, when set, is the whole answer (no diff to show): binary
	// content, or texts that differ only in what the line diff ignores.
	message string
	// body is the rendered diff; changes are its rows that differ, for n / N.
	body    string
	changes []int
}

// compareLocal decides what to show for a local body against an object body.
func compareLocal(local, remote []byte, localLabel, remoteLabel string) (localDiffOutcome, error) {
	if bytes.Equal(local, remote) {
		return localDiffOutcome{identical: true}, nil
	}
	if isProbablyBinary(local) || isProbablyBinary(remote) {
		return localDiffOutcome{message: fmt.Sprintf(
			"binary content differs (local %s, object %s) — a line diff would be noise",
			humanize.IBytes(uint64(len(local))), humanize.IBytes(uint64(len(remote))))}, nil
	}
	body, err := unifiedDiff(string(local), string(remote), localLabel, remoteLabel)
	if err != nil {
		return localDiffOutcome{}, err
	}
	if body == "" {
		// unifiedDiff compares lines with CRLF and the final newline
		// normalised, so two bodies can differ in bytes and still produce no
		// diff. Saying "identical" there would be wrong in exactly the case a
		// user diffing a Windows-edited file cares about.
		return localDiffOutcome{message: "the files differ only in line endings (CRLF vs LF) or the final newline"}, nil
	}
	return localDiffOutcome{body: body, changes: diffChangeRows(body)}, nil
}

// diffChangeRows lists the rows of a rendered diff that are a change, so the
// viewer can jump between them: a full-context diff of a long file is mostly
// unchanged lines.
func diffChangeRows(body string) []int {
	var rows []int
	prevChanged := false
	for i, line := range strings.Split(body, "\n") {
		changed := strings.HasPrefix(line, "[red]- ") || strings.HasPrefix(line, "[green]+ ")
		// One stop per run of changes, at its first line.
		if changed && !prevChanged {
			rows = append(rows, i)
		}
		prevChanged = changed
	}
	return rows
}

// nextChange returns the change row after (or, backwards, before) the current
// top row, wrapping around; -1 when there are none.
func nextChange(rows []int, top int, forward bool) int {
	if len(rows) == 0 {
		return -1
	}
	if forward {
		for _, r := range rows {
			if r > top {
				return r
			}
		}
		return rows[0]
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] < top {
			return rows[i]
		}
	}
	return rows[len(rows)-1]
}

// DiffLocal compares the highlighted object with its local copy, line by line:
// "what did I change since I downloaded it", or "is the deployed config the
// one in my checkout". The local copy is looked for where the local pane is,
// else where a download would have put it; when it is not there, the path is
// asked for, prefilled with that guess.
func (c *Controller) DiffLocal() {
	if c.remoteOnly("Diff against a local file") {
		return
	}
	if c.pane().Bucket() == nil {
		c.error("Diff", fmt.Errorf("open a bucket first"))
		return
	}
	_, obj, ok := c.currentObject()
	if !ok || obj == nil || obj.Ot != model.File || obj.FullPath == nil {
		c.error("Diff", fmt.Errorf("highlight a file"))
		return
	}
	key := *obj.FullPath
	bucket := c.pane().Bucket()

	guess, err := localDiffCandidate(c.otherPaneLocalDir(), c.resolveDownloadDir(), c.pane().Path(), key)
	if err != nil {
		c.error("Diff", err)
		return
	}
	if st, err := os.Stat(guess); err == nil && !st.IsDir() {
		c.runLocalDiff(bucket, key, guess)
		return
	}

	// formLay is assigned below; the buttons fire only after that.
	var formLay *shell.Layer
	form := c.view.NewInputForm(" Diff "+key+" against local file ", "Local file", guess)
	submit := func() {
		p := expandHome(strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText()), c.params.HomeDir)
		formLay.Close()
		if p == "" {
			return
		}
		c.runLocalDiff(bucket, key, p)
	}
	form.AddButton("Diff", submit)
	form.AddButton("Cancel", func() { formLay.Close() })
	form.SetInputCapture(escapeCloses(&formLay))
	formLay = c.pushModal(form, 90, 7)
}

// runLocalDiff fetches the object, reads the local file and shows the result.
func (c *Controller) runLocalDiff(bucket *model.Object, key, local string) {
	mdl := c.model
	remoteLabel := "s3://" + *bucket.Key + "/" + key
	wait, _, ctx, cancel := c.cancellableWait(fmt.Sprintf("Comparing %s ...", path.Base(key)))
	go func() {
		defer cancel()
		localData, err := readLocalCapped(local, editMaxSize)
		if err != nil {
			c.shell.Do(wait.Close)
			if errors.Is(err, os.ErrNotExist) {
				err = fmt.Errorf("no file at %s", local)
			}
			c.error("Diff: reading the local file failed", err)
			return
		}
		remote, err := mdl.GetVersionContent(ctx, bucket, key, "", editMaxSize)
		c.shell.Do(wait.Close)
		if err != nil {
			if ctx.Err() == nil {
				c.error("Diff: reading the object failed", fmt.Errorf("%w — for a large object, V compares checksums instead", err))
			}
			return
		}

		out, err := compareLocal(localData, remote, "local "+local, remoteLabel)
		switch {
		case err != nil:
			c.error("Diff", fmt.Errorf("%s: %w — diff the files with a local tool", key, err))
			return
		case out.identical:
			c.success(fmt.Sprintf("%s\n\nis identical to\n\n%s", local, remoteLabel))
			return
		case out.message != "":
			c.error("Diff", fmt.Errorf("%s vs %s: %s", local, remoteLabel, out.message))
			return
		}
		c.logActivity("Diff %s against %s: %d change(s)", key, local, len(out.changes))

		title := fmt.Sprintf(" %s — local (−) vs object (+), %d change(s) ", tview.Escape(path.Base(key)), len(out.changes))
		c.showTextOverlay(title, out.body,
			"[gray]↑↓←→/PgUp/PgDn[white] scroll · [gray]n/N[white] next/previous change · [gray]Esc/q[white] close",
			func(ev *tcell.EventKey, _ *shell.Layer, tv *tview.TextView) bool {
				if ev.Key() != tcell.KeyRune || (ev.Rune() != 'n' && ev.Rune() != 'N') {
					return false
				}
				top, _ := tv.GetScrollOffset()
				if row := nextChange(out.changes, top, ev.Rune() == 'n'); row >= 0 {
					tv.ScrollTo(row, 0)
				}
				return true
			})
	}()
}
