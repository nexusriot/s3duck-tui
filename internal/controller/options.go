package controller

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
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

	// lay is assigned at the bottom; the buttons below fire only after that.
	var lay *shell.Layer
	form := c.view.NewProfileOptionsForm(conf.Name, optionsFromConfig(conf),
		model.SSEOptions(), model.ChecksumOptions())

	form.AddButton("Save", func() {
		opts, err := readProfileOptions(form, model.SSEOptions(), model.ChecksumOptions())
		if err != nil {
			lay.Close()
			c.error("Options", err)
			return
		}
		applyOptionsToConfig(c.params.Config[idx], opts)
		lay.Close()
		if err := c.params.WriteConfig(); err != nil {
			c.error("Could not save the profile", err)
			return
		}
		c.logActivity("profile %s options updated (%s)", conf.Name, optionsSummary(c.params.Config[idx]))
		c.fillConfigData()
		// fillConfigData rebuilds the list with the cursor on row 0; put it
		// back on the edited profile, or the details pane (showing this one)
		// and Enter (opening row 0) would disagree about which is selected.
		c.view.List.SetCurrentItem(idx)
		c.fillConfigDetails(conf.Name)
		c.success("Options saved")
	})
	form.AddButton("Cancel", func() { lay.Close() })

	form.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModalClamped(form, 76, widgets.ProfileOptionsHeight)
	c.view.App.SetFocus(form)
}

// optionsFromConfig projects a stored profile into the form's value.
func optionsFromConfig(p *cfg.Config) widgets.ProfileOptions {
	return widgets.ProfileOptions{
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
		Workers:    workersText(p.TransferWorkers),
	}
}

// maxTransferWorkers caps the per-job width. Past a few dozen parallel GETs
// one client gains nothing but throttling responses, and each worker in a
// download holds a 5 MiB buffer.
const maxTransferWorkers = 16

// workersText renders the stored width for the form: blank for the default,
// so the field shows that nothing was chosen rather than a number that looks
// like a choice.
func workersText(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// parseWorkers reads the form's width: blank is the default (0), anything else
// must be a whole number from 1 to maxTransferWorkers.
func parseWorkers(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxTransferWorkers {
		return 0, fmt.Errorf("parallel transfers must be a number from 1 to %d (blank for the default of %d)", maxTransferWorkers, ops.DownloadWorkers)
	}
	return n, nil
}

// transferWorkers is the open profile's per-job transfer width; 0 lets each
// operation use its built-in default.
func (c *Controller) transferWorkers() int {
	if c.activeConfig == nil {
		return 0
	}
	return c.activeConfig.TransferWorkers
}

// readProfileOptions reads the form back by label and validates it.
func readProfileOptions(form *tview.Form, sseChoices, checksumChoices []string) (widgets.ProfileOptions, error) {
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

	opts := widgets.ProfileOptions{
		AWSProfile:  text(widgets.FieldOptAWSProfile),
		ReadOnly:    checked(widgets.FieldOptReadOnly),
		Trash:       checked(widgets.FieldOptTrash),
		TrashPrefix: text(widgets.FieldOptTrashPfx),
		DetectMime:  checked(widgets.FieldOptMime),
		SSE:         choice(widgets.FieldOptSSE, sseChoices),
		KMSKey:      text(widgets.FieldOptKMSKey),
		Checksum:    choice(widgets.FieldOptChecksum, checksumChoices),
		Verify:      checked(widgets.FieldOptVerify),
		Workers:     text(widgets.FieldOptWorkers),
	}
	return opts, validateProfileOptions(opts)
}

// validateProfileOptions catches the combinations that would fail later, at a
// point where the message can still name the field.
func validateProfileOptions(o widgets.ProfileOptions) error {
	if o.KMSKey != "" && o.SSE != "aws:kms" {
		return fmt.Errorf("an SSE-KMS key id only applies when encryption is aws:kms")
	}
	if o.Checksum != "" && !model.ValidChecksum(o.Checksum) {
		return fmt.Errorf("%q is not a checksum algorithm S3 accepts", o.Checksum)
	}
	if o.Trash && strings.HasPrefix(strings.TrimSpace(o.TrashPrefix), "/") {
		return fmt.Errorf("a trash prefix is a key prefix, so it cannot start with /")
	}
	if _, err := parseWorkers(o.Workers); err != nil {
		return err
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
func applyOptionsToConfig(p *cfg.Config, o widgets.ProfileOptions) {
	p.AWSProfile = o.AWSProfile
	p.ReadOnly = o.ReadOnly
	p.Trash = o.Trash
	p.TrashPrefix = o.TrashPrefix
	p.NoMimeDetect = !o.DetectMime
	p.SSE = o.SSE
	p.SSEKMSKeyID = o.KMSKey
	p.ChecksumAlgo = o.Checksum
	p.VerifyDownloads = o.Verify
	// Validated before this is reached; a value that still fails to parse
	// falls back to the default rather than storing nonsense.
	p.TransferWorkers, _ = parseWorkers(o.Workers)
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
	if p.TransferWorkers > 0 {
		parts = append(parts, fmt.Sprintf("%d parallel transfers", p.TransferWorkers))
	}
	return strings.Join(parts, ", ")
}
