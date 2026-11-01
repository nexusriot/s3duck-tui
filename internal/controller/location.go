package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	u "github.com/nexusriot/s3duck-tui/internal/utils"
)

// Locations as s3:// URIs: the launch argument, go-to-path (g) and copy URI
// (leader c). All three name a place the same way the AWS CLI does, so a URI
// copied here pastes into `aws s3 ls`, and one copied from a log pastes here.

// resolveLaunchProfile picks the profile the command line asked for. A
// location alone is enough when there is only one profile to open it in;
// with several, guessing would browse the wrong account.
func resolveLaunchProfile(profiles []*cfg.Config, name string, haveLocation bool) (*cfg.Config, error) {
	if name != "" {
		for _, p := range profiles {
			if p.Name == name {
				return p, nil
			}
		}
		return nil, fmt.Errorf("no profile named %q (configured: %s)", name, profileNameList(profiles))
	}
	if !haveLocation {
		return nil, nil
	}
	switch len(profiles) {
	case 0:
		return nil, fmt.Errorf("no profiles are configured, so there is nothing to open the location with")
	case 1:
		return profiles[0], nil
	default:
		return nil, fmt.Errorf("several profiles are configured; choose one with --profile (%s)", profileNameList(profiles))
	}
}

func profileNameList(profiles []*cfg.Config) string {
	if len(profiles) == 0 {
		return "none"
	}
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		names = append(names, fmt.Sprintf("%q", p.Name))
	}
	return strings.Join(names, ", ")
}

// ValidateLaunch checks the command line against the loaded profiles before
// the TUI takes the terminal, so a mistyped profile is a one-line error on
// stderr rather than a modal over a screen the user did not ask for.
func (c *Controller) ValidateLaunch() error {
	if c.launch.Profile == "" && c.launch.Location == nil {
		return nil
	}
	if c.params.LoadErr != nil {
		return c.params.LoadErr
	}
	_, err := resolveLaunchProfile(c.params.Config, c.launch.Profile, c.launch.Location != nil)
	return err
}

// startLaunch opens what the command line named. Runs once, on the UI
// goroutine, after the profile list is built (so Ctrl+P goes back to it).
func (c *Controller) startLaunch() {
	if c.params.LoadErr != nil {
		return
	}
	prof, err := resolveLaunchProfile(c.params.Config, c.launch.Profile, c.launch.Location != nil)
	if err != nil || prof == nil {
		// ValidateLaunch already reported anything wrong.
		return
	}
	for i, p := range c.params.Config {
		if p == prof {
			c.view.List.SetCurrentItem(i)
		}
	}
	c.pendingLocation = c.launch.Location
	c.activeConfig = prof
	c.Duck()
}

// takePendingLocation hands Duck the launch location exactly once: Ctrl+P
// and reopening the profile land where the profile remembers, as usual.
func (c *Controller) takePendingLocation() *cli.S3URI {
	loc := c.pendingLocation
	c.pendingLocation = nil
	return loc
}

// locationTarget is where an s3:// key lands: the folder to list and the row
// to put the cursor on ("" for the top).
//
// A key ending in "/" (or no key) is a folder. Any other key is ambiguous —
// "logs" may be an object or the folder "logs/" — and isFolder, the answer
// to "does anything live under key/?", settles it. An object opens its
// folder with the object highlighted; the row key for a file is its full key.
func locationTarget(key string, isFolder bool) (prefix, selectKey string) {
	if key == "" || strings.HasSuffix(key, "/") {
		return key, ""
	}
	if isFolder {
		return key + "/", ""
	}
	return parentPrefix(key), key
}

// openLocation navigates the active pane to bucket/key, probing an ambiguous
// key once the client is pinned to the bucket's region. A failed probe falls
// back to "object": the parent folder is listed either way, so the worst case
// is a cursor at the top of the right folder.
func (c *Controller) openLocation(bucket, key string) {
	mdl := c.model
	c.jumpWith(bucket, func(b *model.Object) (string, string) {
		if key == "" || strings.HasSuffix(key, "/") {
			return locationTarget(key, false)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		isFolder, err := mdl.HasPrefix(ctx, b, key+"/")
		return locationTarget(key, err == nil && isFolder)
	})
	c.logActivity("go to %s", cli.FormatS3URI(bucket, key))
}

// resolveGoto turns what was typed into the go-to box into a bucket and key.
//
//   - s3://bucket/key — absolute, and taken verbatim: only a pasted line break
//     or leading blanks are dropped, since a trailing space can be part of a key;
//   - /key            — from the root of the current bucket;
//   - anything else   — relative to the current folder, with "." and ".."
//     segments resolved; on the bucket list, its first segment is the bucket.
//
// The current folder is used exactly as it is — "a//b/" stays "a//b/", and
// ".." steps up one real segment of it — and only what was typed is
// normalised (empty and "." segments dropped). So a key with empty or dot
// segments of its own has to be typed as an s3:// URI.
func resolveGoto(input, curBucket, curPath string) (bucket, key string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", fmt.Errorf("type a location: s3://bucket/prefix/, /prefix/ or a path relative to here")
	}
	if cli.IsS3URI(trimmed) {
		uri, err := cli.ParseS3URI(strings.TrimLeft(strings.TrimRight(input, "\r\n"), " \t"))
		if err != nil {
			return "", "", err
		}
		return uri.Bucket, uri.Key, nil
	}
	input = trimmed

	rest, base := input, ""
	switch {
	case strings.HasPrefix(input, "/"):
		rest = strings.TrimLeft(input, "/")
		bucket = curBucket // "" on the bucket list: the first segment is one
	case curBucket != "":
		bucket, base = curBucket, curPath
	}
	lastWasName := false
	for _, seg := range strings.Split(rest, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if bucket == "" || base == "" {
				return "", "", fmt.Errorf("%q leads above the bucket", input)
			}
			base = parentPrefix(base)
			lastWasName = false
		default:
			if bucket == "" {
				bucket = seg
				continue
			}
			base += seg + "/"
			lastWasName = true
		}
	}
	if bucket == "" {
		return "", "", fmt.Errorf("%q names no bucket", input)
	}
	key = base
	if lastWasName && !strings.HasSuffix(input, "/") {
		// The last name typed is an object, or a folder openLocation will
		// recognise; either way it is written without the slash.
		key = strings.TrimSuffix(key, "/")
	}
	return bucket, key, nil
}

// GoTo asks for a location and opens it (g). The box starts with where the
// pane is now, written as a URI, so editing the tail of it is the common case.
func (c *Controller) GoTo() {
	if c.remoteOnly("Go to") {
		return
	}
	curBucket, curPath := "", c.pane().Path()
	if b := c.pane().Bucket(); b != nil && b.Key != nil {
		curBucket = *b.Key
	}
	start := ""
	if curBucket != "" {
		start = cli.FormatS3URI(curBucket, curPath)
	}

	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	form := c.view.NewInputForm(" Go to ", "Location", start)
	form.GetFormItem(0).(*tview.InputField).SetFieldWidth(70)
	submit := func() {
		input := form.GetFormItem(0).(*tview.InputField).GetText()
		bucket, key, err := resolveGoto(input, curBucket, curPath)
		if err != nil {
			c.error("Go to", err)
			return
		}
		lay.Close()
		c.openLocation(bucket, key)
	}
	form.AddButton("Go", submit)
	form.AddButton("Cancel", func() { lay.Close() })
	// Enter goes straight there: a go-to box is typed into and left, and
	// tabbing to a button first is a keystroke this flow does not need.
	form.GetFormItem(0).(*tview.InputField).SetDoneFunc(func(k tcell.Key) {
		if k == tcell.KeyEnter {
			submit()
		}
	})
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 90, 7)
	c.view.App.SetFocus(form)
}

// objectURIs renders the URIs copy-URI puts on the clipboard: one per row
// (a folder keeps its trailing slash), or the current location when no row
// is chosen.
func objectURIs(bucket, curPath string, objs []*model.Object) []string {
	if len(objs) == 0 {
		return []string{cli.FormatS3URI(bucket, curPath)}
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		switch {
		case o == nil || o.Key == nil:
			continue
		case o.Ot == model.Bucket:
			out = append(out, cli.FormatS3URI(*o.Key, ""))
		case o.FullPath != nil:
			out = append(out, cli.FormatS3URI(bucket, *o.FullPath))
		}
	}
	return out
}

// copyURIsMessage is the confirmation shown after a copy.
func copyURIsMessage(uris []string) string {
	if len(uris) == 1 {
		return "Copied " + uris[0]
	}
	return fmt.Sprintf("Copied %d URIs, one per line", len(uris))
}

// CopyURI copies the s3:// URI of the marked rows, the highlighted one, or —
// on ".." or an empty folder — the folder being shown (leader c).
func (c *Controller) CopyURI() {
	if c.remoteOnly("Copy URI") {
		return
	}
	bucket := ""
	if b := c.pane().Bucket(); b != nil && b.Key != nil {
		bucket = *b.Key
	}

	names := c.selectedNames()
	if len(names) == 0 {
		if n := c.getSelectedObjectName(); n != "" && n != ".." {
			names = []string{n}
		}
	}
	var objs []*model.Object
	for _, n := range names {
		if o, ok := c.pane().Lookup(n); ok {
			objs = append(objs, o)
		}
	}
	if bucket == "" && len(objs) == 0 {
		return // the bucket list itself has no URI
	}

	uris := objectURIs(bucket, c.pane().Path(), objs)
	text := strings.Join(uris, "\n")
	go func() {
		if err := u.CopyToClipboard(text); err != nil {
			c.error("Clipboard unavailable", fmt.Errorf("%v\n\n%s", err, text))
			return
		}
		c.success(copyURIsMessage(uris))
	}()
}
