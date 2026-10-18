package controller

import (
	"testing"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

// newOverlayController builds a controller wired far enough to push an overlay
// and report an error: a real view and shell, no model and no network.
func newOverlayController(t *testing.T) *Controller {
	t.Helper()
	v := widgets.NewView()
	c := &Controller{
		view:   v,
		params: &cfg.Params{HomeDir: t.TempDir()},
		panes:  [2]*session.Pane{session.New(), session.New()},
	}
	c.shell = shell.New(v.App, v.Pages)
	return c
}

// localFilePane points the active pane at a local directory holding one file,
// and returns that file's objKey — which is what every action is handed.
func localFilePane(c *Controller, dir string) string {
	name := "notes.txt"
	full := dir + "/" + name
	size := int64(12)
	obj := &model.Object{Key: &name, Ot: model.File, Size: &size, FullPath: &full}

	c.pane().SetLocalDir(dir)
	c.pane().SetObjs(map[string]*model.Object{objKey(obj): obj})
	return objKey(obj)
}

// A local pane holds File objects, but no bucket — and the properties view
// reads the bucket name, the ETag and the S3 link off them. Reaching it over a
// local file dereferenced three nil pointers and took the whole TUI down with
// it, from Ctrl+L and from the command palette alike.
func TestFilePropertiesOverALocalFileDoesNotPanic(t *testing.T) {
	c := newOverlayController(t)
	key := localFilePane(c, t.TempDir())

	if c.pane().Bucket() != nil {
		t.Fatal("a local pane must not carry a bucket; the test proves nothing otherwise")
	}
	c.ShowFileProperties(key) // panicked before the guard
}

// Presign reached the model with a nil bucket and came back with a bare
// "bucket is nil", which says nothing about why.
func TestPresignOverALocalFileIsRefusedNotCrashed(t *testing.T) {
	c := newOverlayController(t)
	key := localFilePane(c, t.TempDir())

	c.PresignLink(key)
}

// The same two actions over a remote file must still open: the guards are
// about the local pane, not about the object.
func TestFilePropertiesStillOpensForARemoteFile(t *testing.T) {
	c := newOverlayController(t)
	c.model = &model.Model{Cf: &model.Config{Url: "http://127.0.0.1:1"}}

	bucket := "bkt"
	name := "notes.txt"
	full := "logs/notes.txt"
	size := int64(12)
	etag := "abc"
	obj := &model.Object{Key: &name, Ot: model.File, Size: &size, Etag: &etag, FullPath: &full}

	c.pane().SetBucket(&model.Object{Key: &bucket, Ot: model.Bucket})
	c.pane().SetObjs(map[string]*model.Object{objKey(obj): obj})

	before := c.shell.Depth()
	c.ShowFileProperties(objKey(obj))
	if c.shell.Depth() != before+1 {
		t.Errorf("overlay depth = %d, want %d: the properties modal did not open", c.shell.Depth(), before+1)
	}
}

// A listing that omits the size or the ETag — some S3-compatible endpoints do —
// must render a placeholder rather than dereference nil.
func TestFilePropertiesToleratesAMissingSizeAndETag(t *testing.T) {
	c := newOverlayController(t)
	c.model = &model.Model{Cf: &model.Config{Url: "http://127.0.0.1:1"}}

	bucket := "bkt"
	name := "notes.txt"
	full := "logs/notes.txt"
	obj := &model.Object{Key: &name, Ot: model.File, FullPath: &full}

	c.pane().SetBucket(&model.Object{Key: &bucket, Ot: model.Bucket})
	c.pane().SetObjs(map[string]*model.Object{objKey(obj): obj})

	c.ShowFileProperties(objKey(obj))
}
