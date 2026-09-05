package controller

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

// EditProfileOptions edits the highlighted profile's behaviour settings:
// where its credentials come from, whether it may be written to at all, what
// deleting means, and what every write should claim about content type,
// encryption and checksums.
//
// Bound to "o" on the profiles screen. Runs on the UI goroutine.
func (c *Controller) EditProfileOptions() {
	cur := c.getSelectedObjectName()
	conf := c.ConfigEntryByName(cur)
	if conf == nil {
		return
	}
	idx := -1
	for i, p := range c.params.Config {
		if p == conf {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}

	form := c.view.NewProfileOptionsForm(conf.Name, optionsFromConfig(conf),
		model.SSEOptions(), model.ChecksumOptions())

	form.AddButton("Save", func() {
		opts, err := readProfileOptions(form, model.SSEOptions(), model.ChecksumOptions())
		if err != nil {
			c.view.Pages.RemovePage("modal")
			go c.error("Options", err)
			return
		}
		applyOptionsToConfig(c.params.Config[idx], opts)
		c.view.Pages.RemovePage("modal")
		if err := c.params.WriteConfig(); err != nil {
			go c.error("Could not save the profile", err)
			return
		}
		c.logActivity("profile %s options updated (%s)", conf.Name, optionsSummary(c.params.Config[idx]))
		c.fillConfigData()
		c.fillConfigDetails(conf.Name)
		go c.success("Options saved")
	})
	form.AddButton("Cancel", func() { c.view.Pages.RemovePage("modal") })

	c.view.Pages.AddPage("modal",
		c.view.ModalClamped(form, 76, view.ProfileOptionsHeight), true, true)
	c.view.App.SetFocus(form)
}

// optionsFromConfig projects a stored profile into the form's value.
func optionsFromConfig(p *cfg.Config) view.ProfileOptions {
	return view.ProfileOptions{
		AWSProfile:  p.AWSProfile,
		ReadOnly:    p.ReadOnly,
		Trash:       p.Trash,
		TrashPrefix: p.TrashPrefix,
		// Stored inverted (NoMimeDetect) so that an existing config file,
		// which has no such key, gets detection *on* — the behaviour being
		// fixed rather than the one being preserved.
		DetectMime: !p.NoMimeDetect,
		SSE:        p.SSE,
		KMSKey:     p.SSEKMSKeyID,
		Checksum:   p.ChecksumAlgo,
		Verify:     p.VerifyDownloads,
	}
}

// readProfileOptions reads the form back by label and validates it.
func readProfileOptions(form *tview.Form, sseChoices, checksumChoices []string) (view.ProfileOptions, error) {
	text := func(label string) string {
		f, ok := form.GetFormItemByLabel(label).(*tview.InputField)
		if !ok {
			return ""
		}
		return strings.TrimSpace(f.GetText())
	}
	checked := func(label string) bool {
		f, ok := form.GetFormItemByLabel(label).(*tview.Checkbox)
		return ok && f.IsChecked()
	}
	choice := func(label string, choices []string) string {
		f, ok := form.GetFormItemByLabel(label).(*tview.DropDown)
		if !ok {
			return ""
		}
		i, _ := f.GetCurrentOption()
		if i < 0 || i >= len(choices) {
			return ""
		}
		return choices[i]
	}

	opts := view.ProfileOptions{
		AWSProfile:  text(view.FieldOptAWSProfile),
		ReadOnly:    checked(view.FieldOptReadOnly),
		Trash:       checked(view.FieldOptTrash),
		TrashPrefix: text(view.FieldOptTrashPfx),
		DetectMime:  checked(view.FieldOptMime),
		SSE:         choice(view.FieldOptSSE, sseChoices),
		KMSKey:      text(view.FieldOptKMSKey),
		Checksum:    choice(view.FieldOptChecksum, checksumChoices),
		Verify:      checked(view.FieldOptVerify),
	}
	return opts, validateProfileOptions(opts)
}

// validateProfileOptions catches the combinations that would fail later, at a
// point where the message can still name the field.
func validateProfileOptions(o view.ProfileOptions) error {
	if o.KMSKey != "" && o.SSE != "aws:kms" {
		return fmt.Errorf("an SSE-KMS key id only applies when encryption is aws:kms")
	}
	if o.Checksum != "" && !model.ValidChecksum(o.Checksum) {
		return fmt.Errorf("%q is not a checksum algorithm S3 accepts", o.Checksum)
	}
	if o.Trash && strings.HasPrefix(strings.TrimSpace(o.TrashPrefix), "/") {
		return fmt.Errorf("a trash prefix is a key prefix, so it cannot start with /")
	}
	if o.Verify && o.Checksum == "" {
		// Not an error: an ETag comparison still verifies single-part
		// objects, which is most of what a TUI user downloads. Worth saying
		// once, though, because the multipart case will report "not
		// verifiable" and that would otherwise look like a bug.
		return nil
	}
	return nil
}

// applyOptionsToConfig writes the form's value back into the stored profile.
func applyOptionsToConfig(p *cfg.Config, o view.ProfileOptions) {
	p.AWSProfile = o.AWSProfile
	p.ReadOnly = o.ReadOnly
	p.Trash = o.Trash
	p.TrashPrefix = o.TrashPrefix
	p.NoMimeDetect = !o.DetectMime
	p.SSE = o.SSE
	p.SSEKMSKeyID = o.KMSKey
	p.ChecksumAlgo = o.Checksum
	p.VerifyDownloads = o.Verify
}

// optionsSummary is the one-line description used in the details pane and the
// activity log.
func optionsSummary(p *cfg.Config) string {
	parts := []string{modelConfigFor(p).CredentialSource()}
	if p.ReadOnly {
		parts = append(parts, "read-only")
	}
	if p.Trash {
		parts = append(parts, "trash "+p.Trashed())
	}
	if p.NoMimeDetect {
		parts = append(parts, "no mime detection")
	}
	if p.SSE != "" {
		parts = append(parts, "sse:"+p.SSE)
	}
	if p.ChecksumAlgo != "" {
		parts = append(parts, "checksum:"+p.ChecksumAlgo)
	}
	if p.VerifyDownloads {
		parts = append(parts, "verify downloads")
	}
	return strings.Join(parts, ", ")
}
