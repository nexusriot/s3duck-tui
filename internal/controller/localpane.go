package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// localMode reports whether the active pane is showing a local directory.
func (c *Controller) localMode() bool { return c.pane().LocalDir() != "" }

// otherPaneLocalDir returns the inactive pane's directory, or "" when it is a
// remote pane. Used to aim a download at "the other side".
func (c *Controller) otherPaneLocalDir() string {
	if !c.dual {
		return ""
	}
	return c.other().LocalDir()
}

// otherPaneRemote returns the inactive pane's bucket and prefix when it is a
// remote pane inside a bucket.
func (c *Controller) otherPaneRemote() (*model.Object, string, bool) {
	if !c.dual {
		return nil, "", false
	}
	o := c.other()
	if o.IsLocal() || o.Bucket() == nil {
		return nil, "", false
	}
	return o.Bucket(), o.Path(), true
}

// remoteOnly refuses an action that has no meaning over a local directory. One
// guard called from each entry point, rather than a mode flag every action
// would have to interpret — and it names the action, because "nothing
// happened" is the failure mode this replaces.
func (c *Controller) remoteOnly(action string) bool {
	if !c.localMode() {
		return false
	}
	c.error("Local pane", fmt.Errorf(
		"%s works on objects; this pane is the local directory %s.\n\nTab to the remote pane, or use Ctrl+Y / Ctrl+T to transfer between the panes", action, c.pane().LocalDir()))
	return true
}

// ToggleLocalPane points the *other* pane at a local directory (opening the
// dual-pane layout if needed), or turns it back into a remote pane.
//
// The other pane rather than this one: the point is to have both sides at
// once, and the user's current remote location is what they would otherwise
// have to navigate back to.
func (c *Controller) ToggleLocalPane() {
	if !c.dual {
		c.ToggleDualPane()
		// A local pane hidden by Ctrl+O is still there: showing it again is
		// the whole toggle. Carrying on would turn the remaining remote pane
		// local too, leaving no bucket on screen at all.
		if c.hasLocalPane() {
			return
		}
	}
	// Pressed from the local pane itself (its footer offers "l Close local"):
	// close THIS pane. Acting on the other one unconditionally turned the
	// remote pane into a second local pane and lost its location.
	if c.localMode() {
		c.pane().SetLocalDir("")
		go c.updateList() // a remote pane again, on the buckets screen
		return
	}
	// After ToggleDualPane the active pane is the newly focused one; act on
	// whichever pane is inactive now.
	other := c.other()
	if other.IsLocal() {
		other.SetLocalDir("")
		c.swapAndFocus() // re-fetch: it is a remote pane again
		return
	}
	// Enter(nil, "", "") first so the pane is never a local dir still holding
	// a bucket; the listing that follows reads one consistent location.
	other.Enter(nil, "", "")
	other.SetLocalDir(c.startLocalDir())
	c.swapAndFocus()
}

// startLocalDir picks where a fresh local pane opens: the profile's download
// directory (~/Downloads when it sets none) if that exists, else the home
// directory.
func (c *Controller) startLocalDir() string {
	if dir := strings.TrimSuffix(c.resolveDownloadDir(), string(os.PathSeparator)); dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return c.params.HomeDir
}

// makeLocalObjectMap reads one local directory into a fresh object map. It
// mirrors makeObjectMap's contract: the caller stores the map with
// Pane.SetListing, and an unreadable directory is an error the caller reports.
func (c *Controller) makeLocalObjectMap(dir string) (map[string]*model.Object, error) {
	list, err := model.WalkDir(dir)
	if err != nil {
		return nil, err
	}
	dirs := make(map[string]*model.Object, len(list))
	for _, o := range list {
		dirs[objKey(o)] = o
	}
	return dirs, nil
}

// localTitle is the pane title for a local directory: the path, plus what it
// holds at this level.
func (c *Controller) localTitle() string {
	bytes, files := model.LocalDirSize(c.pane().LocalDir())
	// "[local]" would be read as a tview colour tag and swallowed — the
	// bracket has to be escaped ("[local[]") for a literal one, which is
	// exactly the trap listRow already documents for object names.
	return fmt.Sprintf("[local[] %s  [gray]%d file(s), %s",
		model.DisplayDir(c.pane().LocalDir(), c.params.HomeDir), files, humanize.IBytes(uint64(bytes)))
}

// localDown enters a directory (or does nothing on a file — a local pane has
// no "open").
func (c *Controller) localDown(fullPath string) {
	fi, err := os.Stat(fullPath)
	if err != nil || !fi.IsDir() {
		return
	}
	c.view.Details.Clear()
	c.clearFilterUI()
	c.recordHistory()
	c.pane().SetLocalDir(filepath.Clean(fullPath))
	go c.updateList()
}

// localUp goes to the parent directory. At the filesystem root there is
// nowhere to go, and the pane stays put rather than turning into something
// else.
func (c *Controller) localUp() {
	parent := model.ParentDir(c.pane().LocalDir())
	if parent == "" {
		return
	}
	c.view.Details.Clear()
	c.clearFilterUI()
	c.recordHistory()
	// Restore the cursor onto the directory being left, whose objKey is its
	// absolute path with a trailing separator.
	c.pane().SetRestoreNext(c.pane().LocalDir() + string(filepath.Separator))
	c.pane().SetLocalDir(parent)
	go c.updateList()
}

// localSelection resolves the marked local paths, falling back to the
// highlighted one — the same rule every remote action follows.
func (c *Controller) localSelection() []string {
	names := c.selectedNames()
	if len(names) == 0 {
		if n := c.getSelectedObjectName(); n != "" && n != ".." {
			names = []string{n}
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if o, ok := c.pane().Lookup(n); ok && o.FullPath != nil {
			out = append(out, strings.TrimSuffix(*o.FullPath, string(filepath.Separator)))
		}
	}
	return out
}

// uploadFromLocalPane sends the local pane's selection to the other pane's
// bucket and prefix — the copy/move keys, read in the direction the panes are
// pointing. This is the local pane's whole reason for existing: the same
// Ctrl+Y that copies between two remote prefixes uploads when the source is
// local.
func (c *Controller) uploadFromLocalPane() {
	// The selection below is read from the ACTIVE pane as local paths. From a
	// remote pane (the palette offers this entry unconditionally) that turned
	// object keys into paths relative to the process's working directory —
	// "Documents/" in a bucket uploaded ~/Documents.
	if !c.localMode() {
		c.error("Upload", fmt.Errorf("switch to the local pane first (Tab): the upload sends what is marked there"))
		return
	}
	dstBucket, dstPrefix, ok := c.otherPaneRemote()
	if !ok {
		c.error("Upload", fmt.Errorf("the other pane must be inside a bucket to receive files"))
		return
	}
	if c.readOnlyBlocked("upload") {
		return
	}
	paths := c.localSelection()
	if len(paths) == 0 {
		return
	}

	mdl := c.model
	prefix := model.NormalizePrefix(dstPrefix)
	files, _, err := model.LocalTargets(paths, prefix)
	if err != nil {
		c.error("Upload", err)
		return
	}
	if len(files) == 0 {
		c.error("Upload", fmt.Errorf("nothing to send: the selection holds no regular files"))
		return
	}

	// The keys are known up front, so the overwrite scan needs no listing of
	// the source — the same shortcut the modal upload takes.
	plan := func() ([]string, error) { return overwriteCandidates(files), nil }
	c.confirmOverwrites(mdl, dstBucket, "Upload", plan, func(skip map[string]bool) {
		kept, keptSize := unskippedUploads(files, skip)
		if len(kept) == 0 {
			c.success("Nothing to upload")
			return
		}
		// The upload sends exactly kept — the targets just confirmed. The
		// directory is only the job's label: it used to be handed to a walk,
		// which uploaded everything in the pane's directory (one marked file
		// in ~ sent all of ~) past an overwrite scan that never saw those keys.
		c.runUpload(mdl, filepath.Dir(paths[0]), prefix, dstBucket, kept, keptSize, skip)
	})
}

// hasLocalPane reports whether a local pane is open at all. (Download itself
// needs no helper: it prefers the other pane's directory over the profile's
// download directory on its own.)
func (c *Controller) hasLocalPane() bool {
	return c.localMode() || c.otherPaneLocalDir() != ""
}

// localPaneLabel is the palette's wording for the toggle, which has to say
// which way it will go — a toggle labelled only "local pane" is a coin flip.
func (c *Controller) localPaneLabel() string {
	if c.hasLocalPane() {
		return "Local pane: close it (back to two remote panes)"
	}
	return "Local pane: browse the filesystem beside the bucket"
}

// localBrowseHelp is the frame's hotkey line for a local pane: the keys that
// do something here, and no others.
func localBrowseHelp() string {
	return "[::b][↓,↑][::-]D/U [::b][Ent/Bck][::-]In/Out [::b][/][::-]Filter [::b][Space][::-]Mark " +
		"[::b][Ctrl+Y][::-]Upload→ [::b][l][::-]Close local [::b][Tab][::-]Other pane [::b][Ctrl+H][::-]Hotkeys"
}
