package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
	u "github.com/nexusriot/s3duck-tui/internal/utils"
)

// The profiles screen: the list of saved endpoints and everything that edits
// it. It shares the browser's widgets (both screens drive the same
// tview.List, which is what c.browsing distinguishes), so it lives in the
// controller package rather than in ui/screens — but it is its own screen and
// no longer interleaved with the browser's code.

func (c *Controller) Profiles() {
	c.browsing = false
	// Leaving a profile is also a good moment to record where it was.
	c.rememberLocation()
	c.persistProfiles()
	c.activeConfig = nil
	c.showIdentity()
	c.resetPanes() // collapse to single-pane; the filter box is inert here
	// The browser's column captions have no meaning over the profile list.
	c.view.Header.SetText("")
	c.setConfigInput()
	c.fillConfigData()
}

// parseMaxBps reads the profile form's "max bytes/sec" field. Blank, invalid,
// or negative input means unlimited (0).
func parseMaxBps(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (c *Controller) CreateConfigEntry() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	cForm := c.view.NewCreateProfileForm("Create config entry")
	cForm.AddButton("Save", func() {
		var region *string

		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		sessionToken := cForm.GetFormItem(5).(*tview.InputField).GetText()
		downloadDir := cForm.GetFormItem(6).(*tview.InputField).GetText()
		ignoreSsl := cForm.GetFormItem(7).(*tview.Checkbox).IsChecked()
		maxBps := parseMaxBps(cForm.GetFormItem(8).(*tview.InputField).GetText())

		if reg != "" {
			region = &reg
		}
		conf := cfg.Config{
			Name:           name,
			BaseUrl:        url,
			Region:         region,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			SessionToken:   strings.TrimSpace(sessionToken),
			IgnoreSsl:      ignoreSsl,
			DownloadDir:    strings.TrimSpace(downloadDir),
			MaxBytesPerSec: maxBps,
		}
		err := c.params.NewConfiguration(&conf)

		lay.Close()
		if err != nil {
			c.error("Error creating config entry", err)
		}
		c.fillConfigData()
		c.view.List.SetCurrentItem(len(c.params.Config) - 1)
	})

	cForm.AddButton("Cancel", func() {
		lay.Close()
	})

	cForm.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(cForm, 75, 23)
}

func (c *Controller) EditConfigEntry() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	entry := c.configAt(i)
	if entry == nil {
		return
	}
	cForm := c.view.NewCreateProfileForm("Edit config entry")

	cForm.GetFormItem(0).(*tview.InputField).SetText(entry.Name)
	cForm.GetFormItem(1).(*tview.InputField).SetText(entry.BaseUrl)
	if entry.Region != nil {
		cForm.GetFormItem(2).(*tview.InputField).SetText(*entry.Region)
	}
	cForm.GetFormItem(3).(*tview.InputField).SetText(entry.AccessKey)
	cForm.GetFormItem(4).(*tview.InputField).SetText(entry.SecretKey)
	cForm.GetFormItem(5).(*tview.InputField).SetText(entry.SessionToken)
	cForm.GetFormItem(6).(*tview.InputField).SetText(entry.DownloadDir)
	cForm.GetFormItem(7).(*tview.Checkbox).SetChecked(entry.IgnoreSsl)
	if entry.MaxBytesPerSec > 0 {
		cForm.GetFormItem(8).(*tview.InputField).SetText(strconv.FormatInt(entry.MaxBytesPerSec, 10))
	}

	cForm.AddButton("Save", func() {
		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		sessionToken := cForm.GetFormItem(5).(*tview.InputField).GetText()
		downloadDir := cForm.GetFormItem(6).(*tview.InputField).GetText()
		ignoreSsl := cForm.GetFormItem(7).(*tview.Checkbox).IsChecked()
		var region *string
		if reg != "" {
			region = &reg
		}

		entry.Name = name
		entry.BaseUrl = url
		entry.Region = region
		entry.AccessKey = accessKey
		entry.SecretKey = secretKey
		entry.SessionToken = strings.TrimSpace(sessionToken)
		entry.IgnoreSsl = ignoreSsl
		entry.DownloadDir = strings.TrimSpace(downloadDir)
		entry.MaxBytesPerSec = parseMaxBps(cForm.GetFormItem(8).(*tview.InputField).GetText())

		err := c.params.WriteConfig()
		lay.Close()
		if err != nil {
			c.error("Failed to save config", err)
		}
		c.fillConfigData()
		c.view.List.SetCurrentItem(i)
	})
	cForm.AddButton("Cancel", func() {
		lay.Close()
	})

	cForm.SetInputCapture(escapeCloses(&lay))
	lay = c.pushModal(cForm, 75, 23)
}

func (c *Controller) CopyProfile() {
	// confirmLay is assigned below; every handler here runs after that.
	var confirmLay *shell.Layer

	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	newName := fmt.Sprintf("%s_%s", cur, u.RandStr(4))

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to copy config %s -> %s", cur, newName)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			confirmLay.Close()

			if buttonLabel != widgets.ConfirmOK {
				return
			}
			// Inline, on the UI goroutine the done handler already runs on:
			// c.params.Config is read by every profile screen handler, so
			// rewriting it from a background goroutine raced them. Saving is
			// one small local file, which is why every other config write —
			// options, edit, bookmarks — is done the same way.
			entry := c.configAt(i)
			if entry == nil {
				return
			}
			conf := *entry
			conf.Name = newName
			if err := c.params.CopyConfig(conf); err != nil {
				c.error("Failed to copy config", err)
				return
			}
			c.fillConfigData()
			c.view.List.SetCurrentItem(len(c.params.Config) - 1)
		})
	confirmLay = c.pushRaw(confirm)
}

// uniqueProfileName returns base, or base-2/base-3/... when a profile of that
// name already exists, so an import can never silently shadow a saved profile.
func uniqueProfileName(base string, existing []*cfg.Config) string {
	taken := make(map[string]bool, len(existing))
	for _, e := range existing {
		if e != nil {
			taken[e.Name] = true
		}
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// awsProfileConfig converts an AWS shared profile into an s3duck profile
// pointed at the regional AWS S3 endpoint.
func awsProfileConfig(p cfg.AWSProfile, existing []*cfg.Config) cfg.Config {
	conf := cfg.Config{
		Name:         uniqueProfileName("aws-"+p.Name, existing),
		BaseUrl:      p.EndpointURL(),
		AccessKey:    p.AccessKey,
		SecretKey:    p.SecretKey,
		SessionToken: p.SessionToken,
	}
	// A delegating profile stores no key material at all: only the name of
	// the ~/.aws profile the SDK should resolve each time, which is what makes
	// SSO and assume-role work here — and keeps working when they expire.
	if p.Delegates != "" {
		conf.AWSProfile = p.Name
		conf.AccessKey, conf.SecretKey, conf.SessionToken = "", "", ""
	}
	if p.Region != "" {
		region := p.Region
		conf.Region = &region
	}
	return conf
}

// awsProfileRow renders one AWS profile for the import list: the name plus
// either its region/credential kind or the reason it can't be imported.
func awsProfileRow(p cfg.AWSProfile) (primary, secondary string) {
	if !p.Usable() {
		return fmt.Sprintf("[gray]%s  (%s)", p.Name, p.Err), p.Name
	}
	kind := p.Kind()
	region := p.Region
	if region == "" {
		region = "no region"
	}
	return fmt.Sprintf("%s  [gray](%s, %s)", p.Name, region, kind), p.Name
}

// ImportAWSProfiles lists the profiles in ~/.aws/credentials and ~/.aws/config
// and imports the selected one as an s3duck profile. This is the supported path
// for temporary credentials (assume-role / SSO / MFA), which carry a session
// token that a hand-typed key pair cannot express.
func (c *Controller) ImportAWSProfiles() {
	// lay is assigned below; every handler here runs after that.
	var lay *shell.Layer
	go func() {
		profiles, err := cfg.LoadAWSProfiles(c.params.HomeDir)
		if err != nil {
			c.error("Import from AWS failed", err)
			return
		}

		c.shell.Do(func() {
			list := tview.NewList()
			list.SetBorder(true).SetTitle(" Import AWS profile ")
			for _, p := range profiles {
				primary, secondary := awsProfileRow(p)
				list.AddItem(primary, secondary, 0, func() {
					if !p.Usable() {
						lay.Close()
						c.error("Cannot import "+p.Name, fmt.Errorf("%s", p.Err))
						return
					}
					conf := awsProfileConfig(p, c.params.Config)
					lay.Close()
					if err := c.params.NewConfiguration(&conf); err != nil {
						c.error("Failed to save imported profile", err)
						return
					}
					c.fillConfigData()
					c.view.List.SetCurrentItem(len(c.params.Config) - 1)
					c.success(fmt.Sprintf("imported AWS profile %s as %s", p.Name, conf.Name))
				})
			}
			list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
				if event.Key() == tcell.KeyEsc {
					lay.Close()
					return nil
				}
				return event
			})
			lay = c.pushModal(list, 76, 20)
		})
	}()
}

// configAt is the profile shown at list row i, or nil when the row does not
// name one. Every profile action is addressed by row, and the row is captured
// when a dialog opens and used after it closes — so the list and the stored
// slice can disagree by the time it is read, and an unchecked index would take
// the terminal down with it.
func (c *Controller) configAt(i int) *cfg.Config {
	if i < 0 || i >= len(c.params.Config) {
		return nil
	}
	return c.params.Config[i]
}

func (c *Controller) CheckProfile() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	cf := c.configAt(i)
	if cf == nil {
		return
	}

	// A throwaway client: verifying a profile must never replace c.model —
	// a queued/backgrounded transfer that reads the model later would run
	// against the verified profile's endpoint instead of its own.
	//
	// Built through modelConfigFor, not field by field: a profile that
	// delegates to an AWS named profile has no static keys, so a hand-rolled
	// config checked the default credential chain instead of the profile the
	// user was pointing at — a pass or a failure that said nothing about it.
	probe, err := model.NewModel(modelConfigFor(cf))
	if err != nil {
		c.error(fmt.Sprintf("error checking profile %s", cf.Name), err)
		return
	}

	// ListBuckets is a network round-trip; run it off the UI goroutine so a
	// slow or unreachable endpoint can't freeze the TUI.
	go func() {
		if _, err := probe.ListBuckets(context.Background()); err != nil {
			c.error(fmt.Sprintf("error checking profile %s", cf.Name), err)
		} else {
			c.success(fmt.Sprintf("successfully checked profile %s", cf.Name))
		}
	}()
}

func (c *Controller) DeleteConfigEntry() {
	// confirmLay is assigned below; every handler here runs after that.
	var confirmLay *shell.Layer

	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to delete %s", cur)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			// No SwitchToPage here: that would hide EVERY other page,
			// including a profile form the user was in the middle of.
			confirmLay.Close()

			if buttonLabel != widgets.ConfirmOK {
				return
			}
			// Inline for the same reason as CopyProfile: the profile list is
			// UI state, and a background goroutine rewriting it raced every
			// handler that reads it.
			if err := c.params.DeleteConfig(i); err != nil {
				c.error("Failed to delete config", err)
				return
			}
			c.fillConfigData()
		})
	confirmLay = c.pushRaw(confirm)
}

func (c *Controller) setConfigInput() {
	// aboutLay / helpLay are assigned below; every handler here runs after that.
	var aboutLay *shell.Layer
	var helpLay *shell.Layer
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		}
		return event
	})

	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		// Delete belongs on the LIST, not the application: an app-level
		// capture fires before the focused widget, so forward-delete while
		// editing a profile form would pop the delete-profile confirm.
		case tcell.KeyDelete:
			c.DeleteConfigEntry()
			return nil
		case tcell.KeyCtrlN:
			c.CreateConfigEntry()
			return nil
		case tcell.KeyCtrlE:
			c.EditConfigEntry()
			return nil
		case tcell.KeyCtrlY:
			c.CopyProfile()
			return nil
		case tcell.KeyCtrlI:
			c.ImportAWSProfiles()
			return nil
		case tcell.KeyCtrlH:
			help := c.view.HotkeysModal(true, nil, func() {
				helpLay.Close()
			})
			helpLay = c.pushRaw(help)
			return nil
		case tcell.KeyCtrlV:
			c.CheckProfile()
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'o' {
				c.EditProfileOptions()
				return nil
			}
		case tcell.KeyCtrlA:
			about := c.view.AboutModal()
			about.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				aboutLay.Close()
				return nil
			})
			aboutLay = c.pushModal(about, 70, 19)
			return nil

		}
		return event
	})
}

func (c *Controller) ConfigEntryByName(name string) *cfg.Config {
	for _, v := range c.params.Config {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func (c *Controller) fillConfigDetails(cur string) {
	c.view.Details.Clear()
	item := c.ConfigEntryByName(cur)

	if item != nil {
		fmt.Fprintf(c.view.Details, "[green] Config: [white]%s\n", item.Name)
		fmt.Fprintf(c.view.Details, "[blue] Url: [gray] %s\n", item.BaseUrl)
		if item.Region != nil {
			fmt.Fprintf(c.view.Details, "[blue] Region: [white] %s\n", *item.Region)
		}
		fmt.Fprintf(c.view.Details, "[blue] Ssl: [white] %v\n", !item.IgnoreSsl)
		// The options are what decide whether this profile can be written to
		// at all, so they belong where the profile is chosen — not only
		// behind the form that edits them.
		fmt.Fprintf(c.view.Details, "[blue] Credentials: [white] %s\n",
			modelConfigFor(item).CredentialSource())
		if item.ReadOnly {
			fmt.Fprintf(c.view.Details, "[red] READ-ONLY[white] — every write is refused\n")
		}
		fmt.Fprintf(c.view.Details, "[blue] Safe delete: [white] %s\n", trashSummaryLine(item))
		fmt.Fprintf(c.view.Details, "[blue] Writes: [white] %s\n",
			modelConfigFor(item).Write.WriteSummary())
		if item.VerifyDownloads {
			fmt.Fprintf(c.view.Details, "[blue] Downloads: [white] verified against the object checksum\n")
		}
		if item.LastBucket != "" {
			fmt.Fprintf(c.view.Details, "[blue] Reopens at: [white] %s/%s\n", item.LastBucket, item.LastPrefix)
		}
	}
}

func (c *Controller) fillConfigData() {
	c.view.Details.Clear()
	c.view.List.Clear()
	c.view.List.SetTitle("(profiles)")

	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		c.fillConfigDetails(cur)
	})

	for _, cf := range c.params.Config {
		c.view.List.AddItem(cf.Name, cf.Name, 0, func() {
			conf := c.configAt(c.view.List.GetCurrentItem())
			if conf == nil {
				return
			}
			c.activeConfig = conf
			c.Duck()
		})
	}
	c.view.SetFrameText("[::b][↓,↑][::-]Down/Up [::b][Enter[][::-]Use [::b][Ctrl+N[][::-]New [::b][Ctrl+I[][::-]Import AWS [::b][Ctrl+Y[][::-]Yank(Copy) [::b][Ctrl+E[][::-]Edit [::b][o[][::-]Options [::b][Ctrl+V[][::-]Verify [::b][Del[][::-]Delete [::b][Ctrl+H[][::-]Hotkeys [::b][Ctrl+Q][::-]Quit")
}

// Duck opens the profile in c.activeConfig for browsing. Every connection
// option (credentials, throughput cap, write options) is read from that
// profile through modelConfigFor rather than passed in piecemeal.
func (c *Controller) Duck() {
	if c.activeConfig == nil {
		return
	}
	mdl, err := model.NewModel(modelConfigFor(c.activeConfig))
	if err != nil {
		c.error("Cannot open profile", err)
		return
	}
	c.model = mdl
	// The clipboard and the one-step undo carry bucket/key names from the
	// profile they were made in; surviving a profile switch, a paste or undo
	// would copy from — and a cut-paste DELETE from — a same-named bucket on
	// the new endpoint.
	c.clip = clipboard{}
	c.setUndo(nil)
	c.browsing = true // the browser owns the shared list widget from here on
	c.showIdentity()  // the header now names the account being browsed
	c.resetPanes()    // fresh single-pane browser for this profile
	c.wireListChanged(c.view.PaneList(0))
	c.pane().SetBucket(nil)
	c.pane().SetPath("")
	c.pane().SetBucketPos(0)
	c.setInput()
	c.logActivity("opened profile %s (%s)", c.activeConfig.Name,
		modelConfigFor(c.activeConfig).CredentialSource())
	// Land where this profile was last browsing, if it remembers.
	if c.restoreLocation() {
		return
	}
	go c.updateList()
}
