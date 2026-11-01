package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	u "github.com/nexusriot/s3duck-tui/internal/utils"
)

// presignTTL is one expiry choice offered for a presigned link.
type presignTTL struct {
	label string
	ttl   time.Duration
}

// presignTTLs are the expiries both link kinds offer; the last is the SigV4
// ceiling.
var presignTTLs = []presignTTL{
	{"15 min", 15 * time.Minute},
	{"1 hour", time.Hour},
	{"24 hours", 24 * time.Hour},
	{"7 days", model.PresignMaxTTL},
}

// Labels of the presigned-upload form, read back by label.
const (
	fieldPutKey    = "Object key"
	fieldPutExpiry = "Expires in"
)

// presignUploadDefaultKey is what the key field starts with: the highlighted
// object's key (a link that replaces it), or the folder being shown with a
// name still to type.
func presignUploadDefaultKey(curPath string, highlighted *model.Object) string {
	if highlighted != nil && highlighted.Ot == model.File && highlighted.FullPath != nil {
		return *highlighted.FullPath
	}
	return curPath
}

// uploadLinkText is the result panel: where the link writes, for how long,
// whether it replaces something, and how to use it.
//
// temporary is set when the profile signs with temporary credentials (an SSO
// or assumed-role aws_profile, or a stored session token): a SigV4 link dies
// with the credentials that signed it, often within the hour, whatever expiry
// was chosen.
func uploadLinkText(uri, link, expiry string, replaces, temporary bool, copyErr error) string {
	var b strings.Builder
	if copyErr != nil {
		fmt.Fprintf(&b, "[red]The clipboard is unavailable (%s); copy the link below by hand.[-]\n\n", tview.Escape(copyErr.Error()))
	} else {
		b.WriteString("[green]Upload link copied to the clipboard.[-]\n\n")
	}
	fmt.Fprintf(&b, "Writes:   %s\nExpires:  in %s\n", tview.Escape(uri), expiry)
	if replaces {
		b.WriteString("[yellow]An object already exists at this key: an upload through the link replaces it.[-]\n")
	}
	if temporary {
		b.WriteString("[yellow]This profile signs with temporary credentials: the link stops working when they expire, which can be well before the expiry above.[-]\n")
	}
	b.WriteString("\nAnyone holding the link can upload ONE object to this key, as often as they like, until it expires. ")
	b.WriteString("The profile's encryption and checksum settings do not apply; the bucket's defaults do.\n\n")
	b.WriteString("Upload with:\n  curl -T FILE '<link>'\n\n")
	b.WriteString(tview.Escape(link))
	b.WriteString("\n")
	return b.String()
}

// PresignUpload makes a presigned PUT link: a way for someone with no
// credentials to deliver one file into the bucket ("send me the logs").
// Bound to leader p; the GET counterpart is Ctrl+W.
func (c *Controller) PresignUpload() {
	if c.remoteOnly("Upload links") {
		return
	}
	if c.readOnlyBlocked("hand out upload links") {
		return
	}
	bucket := c.pane().Bucket()
	if bucket == nil || bucket.Key == nil {
		c.error("Upload link", fmt.Errorf("open a bucket first: the link writes to a key inside one"))
		return
	}
	if c.model.Cf != nil && c.model.Cf.Anonymous {
		c.error("Upload link", model.ErrAnonymousCannotSign)
		return
	}
	var highlighted *model.Object
	if o, ok := c.pane().Lookup(c.getSelectedObjectName()); ok {
		highlighted = o
	}
	mdl := c.model

	labels := make([]string, len(presignTTLs))
	for i, t := range presignTTLs {
		labels[i] = t.label
	}

	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	form := tview.NewForm()
	form.SetTitle(" Presigned upload link — " + tview.Escape(*bucket.Key) + " ")
	form.AddInputField(fieldPutKey, presignUploadDefaultKey(c.pane().Path(), highlighted), 60, nil, nil)
	form.AddDropDown(fieldPutExpiry, labels, 1, nil)
	form.SetBorder(true)
	form.AddButton("Create link", func() {
		key := form.GetFormItemByLabel(fieldPutKey).(*tview.InputField).GetText()
		i, _ := form.GetFormItemByLabel(fieldPutExpiry).(*tview.DropDown).GetCurrentOption()
		if i < 0 || i >= len(presignTTLs) {
			i = 1
		}
		choice := presignTTLs[i]
		if key == "" || strings.HasSuffix(key, "/") {
			c.error("Upload link", fmt.Errorf("type the object's name after the folder: a link uploads one object to one key"))
			return
		}
		lay.Close()
		go c.makeUploadLink(mdl, bucket, key, choice)
	})
	form.AddButton("Cancel", func() { lay.Close() })
	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(form, 80, 9)
	c.view.App.SetFocus(form)
}

// makeUploadLink signs the link, checks whether it would replace an object,
// copies it, and shows the result. Runs off the UI goroutine.
func (c *Controller) makeUploadLink(mdl *model.Model, bucket *model.Object, key string, choice presignTTL) {
	link, err := mdl.PresignPutURL(bucket, key, choice.ttl)
	if err != nil {
		c.error("Upload link", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, headErr := mdl.HeadObject(ctx, bucket, key)
	cancel()
	replaces := headErr == nil

	copyErr := u.CopyToClipboard(link)
	uri := cli.FormatS3URI(*bucket.Key, key)
	// The link itself is a credential: the log says one was made, not what it is.
	c.logActivity("presigned upload link for %s (%s)", uri, choice.label)
	temporary := mdl.Cf != nil && (mdl.Cf.AWSProfile != "" || mdl.Cf.SessionToken != "")
	text := uploadLinkText(uri, link, choice.label, replaces, temporary, copyErr)
	c.shell.Do(func() {
		var lay *shell.Layer
		tv := tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetWordWrap(false)
		tv.SetText(text)
		tv.SetBorder(true).SetTitle(" Upload link (Esc/Enter closes) ")
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEsc || ev.Key() == tcell.KeyEnter {
				lay.Close()
				return nil
			}
			return ev
		})
		lay = c.pushModalClamped(tv, 100, 22)
		c.view.App.SetFocus(tv)
	})
}
