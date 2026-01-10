package controller

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/model"
	u "github.com/nexusriot/s3duck-tui/pkg/utils"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

type overwriteDecision int

const (
	decOverwrite overwriteDecision = iota
	decSkip
	decOverwriteAll
	decSkipAll
	decCancel
)

type Controller struct {
	debug         bool
	view          *view.View
	model         *model.Model
	buckets       []*model.Object
	objs          map[string]*model.Object
	currentPath   string
	currentBucket *model.Object
	bucketPos     int
	position      map[string]int
	params        *cfg.Params
}

func NewController(debug bool) *Controller {

	v := view.NewView()
	params := cfg.NewParams()

	controller := Controller{
		debug:       debug,
		view:        v,
		model:       nil,
		currentPath: "",
		bucketPos:   0,
		position:    make(map[string]int),
		params:      params,
	}
	return &controller
}

func (c *Controller) askOverwrite(path string) overwriteDecision {
	ch := make(chan overwriteDecision, 1)

	c.view.App.QueueUpdateDraw(func() {
		m := tview.NewModal().
			SetText(fmt.Sprintf("File already exists:\n%s\n\nWhat do you want to do?", path)).
			AddButtons([]string{"Overwrite", "Skip", "Overwrite All", "Skip All", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				c.view.Pages.RemovePage("overwrite")
				switch label {
				case "Overwrite":
					ch <- decOverwrite
				case "Skip":
					ch <- decSkip
				case "Overwrite All":
					ch <- decOverwriteAll
				case "Skip All":
					ch <- decSkipAll
				default:
					ch <- decCancel
				}
			})

		c.view.Pages.AddPage("overwrite", c.view.ModalEdit(m, 80, 10), true, true)
		c.view.App.SetFocus(m)
	})

	return <-ch
}

func (c *Controller) makeObjectMap() error {
	var list []*model.Object
	var err error
	dirs := make(map[string]*model.Object)

	if c.currentBucket == nil {
		list, err = c.model.ListBuckets()
		if err != nil {
			return err
		}
	} else {
		list, err = c.model.List(c.currentPath, c.currentBucket)
	}
	if err != nil {
		return err
	}
	for _, obj := range list {
		dirs[*obj.Key] = obj
	}
	c.objs = dirs
	return nil
}

func localDownloadPath(currentPath, destPath, s3Key string) string {
	key := filepath.ToSlash(s3Key)
	prefix := filepath.ToSlash(currentPath)

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	relativeKey := key
	if strings.HasPrefix(key, prefix) {
		relativeKey = strings.TrimPrefix(key, prefix)
	}

	return filepath.Join(destPath, relativeKey)
}

func getPosition(element string, slice []string) int {
	for k, v := range slice {
		if element == v {
			return k
		}
	}
	return 0
}

func (c *Controller) getSelectedObjectName() string {
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	return strings.TrimSpace(cur)
}

func (c *Controller) Profiles() {
	c.setConfigInput()
	c.fillConfigData()
}

func (c *Controller) Delete() error {
	if c.view.List.GetItemCount() == 0 {
		return nil
	}
	cur := c.getSelectedObjectName()

	if val, ok := c.objs[cur]; ok {
		op := path.Join(c.currentPath, cur)

		confirm := c.view.NewConfirm()
		confirm.SetText(fmt.Sprintf("Do you want to delete to %s", op)).
			SetDoneFunc(func(buttonIndex int, buttonLabel string) {
				c.view.Pages.RemovePage("confirm").SwitchToPage("main")

				if buttonLabel == "OK" {
					go func() {
						var err error
						if val.Ot == model.Bucket {
							err = c.model.DeleteBucket(&cur)
						} else {
							if val.Ot == model.Folder {
								op = op + "/"
							}
							err = c.model.Delete(&op, c.currentBucket)
						}
						go func() {
							if err != nil {
								c.error(fmt.Sprintf("Failed to delete %s", cur), err, false)
							} else {
								c.updateList()
								c.clearDetailsIfNoSelection()
							}
						}()

					}()
				}
			})
		c.view.Pages.AddPage("confirm", confirm, true, true)
	}
	return nil
}

func (c *Controller) Download() error {
	overwriteAll := false
	skipAll := false

	if c.view.List.GetItemCount() == 0 {
		return nil
	}

	cur := c.getSelectedObjectName()
	val, ok := c.objs[cur]
	if !ok || (val.Ot != model.File && val.Ot != model.Folder) {
		return nil
	}

	cwd := filepath.Join(c.params.HomeDir, "Downloads") + string(os.PathSeparator)
	key := c.currentPath + cur

	objects, totalSize, err := c.model.ResolveDownloadObjects(key, val.Ot == model.Folder, val.Size, c.currentBucket)
	if err != nil || len(objects) == 0 {
		return err
	}

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to download to %s\n%d object(s)\ntotal size %s",
		cwd,
		len(objects),
		humanize.IBytes(uint64(totalSize)),
	)).SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("confirm").SwitchToPage("main")

		if buttonLabel != "OK" {
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		progress := tview.NewModal().
			SetText("Starting download...\n").
			AddButtons([]string{"Cancel"}).
			SetDoneFunc(func(buttonIndex int, buttonLabel string) {
				cancel()
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
			})
		c.view.Pages.AddPage("progress", progress, true, true)

		go func() {
			downloadedSize := int64(0)

			for i, object := range objects {
				select {
				case <-ctx.Done():
					return
				default:
				}
				// TODO -> avoid using aws lib in controller
				dst := localDownloadPath(c.currentPath, cwd, aws.ToString(object.Key))

				// Directory marker (S3 "folders"): let model handle mkdir logic, no prompt
				if strings.HasSuffix(aws.ToString(object.Key), "/") {
					// proceed as usual
				} else {
					// Existence check
					if _, err := os.Stat(dst); err == nil {
						if skipAll {
							continue
						}
						if !overwriteAll {
							d := c.askOverwrite(dst)
							switch d {
							case decSkip:
								continue
							case decOverwrite:
								_ = os.Remove(dst)
							case decOverwriteAll:
								overwriteAll = true
								_ = os.Remove(dst)
							case decSkipAll:
								skipAll = true
								continue
							default:
								cancel()
								c.view.App.QueueUpdateDraw(func() {
									c.view.Pages.RemovePage("progress").SwitchToPage("main")
								})
								return
							}
						} else {
							// overwriteAll already chosen
							_ = os.Remove(dst)
						}
					}
				}

				n, err := c.model.Download(ctx, object, c.currentPath, cwd, c.currentBucket.Key, totalSize, func(written, total int64, key string) {
					percentage := float64(downloadedSize+written) / float64(totalSize) * 100
					c.view.App.QueueUpdateDraw(func() {
						progress.SetText(fmt.Sprintf(
							"Downloading\n%d/%d object(s)\n%s/%s (%.1f%%)\nCurrent: %s",
							i+1, len(objects),
							humanize.IBytes(uint64(downloadedSize+written)),
							humanize.IBytes(uint64(totalSize)),
							percentage,
							key,
						))
					})
				})

				if err != nil {
					c.view.App.QueueUpdateDraw(func() {
						c.view.Pages.RemovePage("progress").SwitchToPage("main")
					})
					go c.error(fmt.Sprintf("Failed to download %s", *object.Key), err, false)
					return
				}
				downloadedSize += n
			}

			select {
			case <-ctx.Done():
				return
			default:
				c.view.App.QueueUpdateDraw(func() {
					progress.ClearButtons()
					progress.AddButtons([]string{"Done"})
					progress.SetText("Download complete.\n\nPress Done to return.")
					progress.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
						c.view.Pages.RemovePage("progress").SwitchToPage("main")
					})
					c.view.App.SetFocus(progress)
				})
			}
		}()
	})

	c.view.Pages.AddPage("confirm", confirm, true, true)
	return nil
}

// coloredLabelFor returns a colorized icon + plain name for best selection contrast.
func coloredLabelFor(o *model.Object) string {
	if o.Key == nil {
		return ""
	}
	name := *o.Key
	switch o.Ot {
	case model.Folder:
		// 📁 folder icon (cyan) + plain name with trailing slash
		return "[cyan]📁[-] " + name + "/"
	case model.File:
		// 📄 file icon + plain name (no inline filename color for selection contrast)
		return "📄 " + name
	case model.Bucket:
		// yellow bucket dot + plain name
		return "[yellow]●[-] " + name
	default:
		return "  " + name
	}
}

func (c *Controller) updateList() ([]string, error) {
	err := c.makeObjectMap()
	if err != nil {
		go c.error("Failed to fetch folder", err, false)
		return nil, err
	}

	var keys []string
	var title string
	var suff string

	if c.currentBucket == nil {
		title = "(buckets)"
		suff = ""
	} else {
		title = fmt.Sprintf("(%s)/%s", *c.currentBucket.Key, c.currentPath)
		suff = "[::b][Ctrl+D[][::-]Download [::b][::b][Ctrl+U[][::-]Upload [::b]"
	}

	fText := fmt.Sprintf("[::b][↓,↑][::-]Down/Up [::b][Enter/Backspace][::-]Lower/Upper %s[::b][Del[][::-]Delete [::b][Ctrl+N][::-]Create [::b][Ctrl+P][::-]Profiles [::b][Ctrl+L][::-]Properties [::b][Ctrl+H][::-]Hotkeys [::b][Ctrl+Q][::-]Quit", suff)

	objs := make([]*model.Object, 0, len(c.objs))
	for _, k := range c.objs {
		objs = append(objs, k)
	}
	sort.Slice(objs, func(i, j int) bool {
		if objs[i].Ot != objs[j].Ot {
			return objs[i].Ot > objs[j].Ot
		}
		return *objs[i].Key < *objs[j].Key
	})

	// Queue UI update
	c.view.App.QueueUpdateDraw(func() {
		c.view.List.Clear()
		c.view.List.SetTitle(title)
		c.view.SetFrameText(fText)

		// Add classic "[..]" at top when inside a bucket (go up one level)
		if c.currentBucket != nil {
			c.view.List.AddItem("[..]", "..", 0, func() {
				c.Up()
			})
		}

		for _, o := range objs {
			if o.Key == nil {
				continue
			}
			raw := *o.Key               // secondary text (plain) – used for lookups
			label := coloredLabelFor(o) // primary (icon colored, name plain)

			c.view.List.AddItem(label, raw, 0, func() {
				i := c.view.List.GetCurrentItem()
				_, cur := c.view.List.GetItemText(i) // secondary text
				cur = strings.TrimSpace(cur)

				// Special entry: "[..]" uses secondary text ".."
				if cur == ".." {
					c.Up()
					return
				}

				if val, ok := c.objs[cur]; ok {
					if val.Ot == model.Folder || val.Ot == model.Bucket {
						if val.Ot == model.Folder {
							c.position[c.currentPath] = c.view.List.GetCurrentItem()
						}
						if val.Ot == model.Bucket {
							c.bucketPos = c.view.List.GetCurrentItem()
						}
						c.Down(cur)
					}
				}
			})

			keys = append(keys, raw)
		}

		// Restore position if available
		if c.currentBucket != nil {
			if val, ok := c.position[c.currentPath]; ok {
				c.view.List.SetCurrentItem(val)
				delete(c.position, c.currentPath)
			}
		}
	})

	return keys, nil
}

func (c *Controller) findBucketByName(name string) *model.Object {
	list, _ := c.model.ListBuckets()
	c.buckets = list
	for _, v := range c.buckets {
		if name == *v.Key {
			return v
		}
	}
	return nil
}

func (c *Controller) Down(name string) {
	if c.currentBucket == nil {
		bucket := c.findBucketByName(name)
		if bucket != nil {
			c.currentBucket = bucket
			c.model.RefreshClient(&name)
		}
	} else {
		newDir := c.currentPath + name + "/"
		c.currentPath = newDir
	}
	c.view.Details.Clear()
	go c.updateList()
}

func (c *Controller) Up() {
	c.view.Details.Clear()
	if c.currentPath == "" {
		c.currentBucket = nil
	}
	fields := strings.FieldsFunc(strings.TrimSpace(c.currentPath), u.SplitFunc)
	if len(fields) == 0 {
		go c.updateList()
		// TODO: do we really need this check?
		if c.currentBucket == nil {
			c.view.List.SetCurrentItem(c.bucketPos)
		}
		return
	}
	newDir := strings.Join(fields[:len(fields)-1], "/")
	if len(fields) > 1 {
		newDir = newDir + "/"
	}
	c.currentPath = newDir
	go c.updateList()
}

func (c *Controller) Stop() {
	c.view.App.Stop()
}

func (c *Controller) CreateConfigEntry() {
	cForm := c.view.NewCreateProfileForm("Create config entry")
	cForm.AddButton("Save", func() {
		var region *string

		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		ignoreSsl := cForm.GetFormItem(5).(*tview.Checkbox).IsChecked()

		if reg != "" {
			region = &reg
		}
		conf := cfg.Config{
			Name:      name,
			BaseUrl:   url,
			Region:    region,
			AccessKey: accessKey,
			SecretKey: secretKey,
			IgnoreSsl: ignoreSsl,
		}
		err := c.params.NewConfiguration(&conf)

		c.view.Pages.RemovePage("modal")
		if err != nil {
			go c.error("Error creating config entry", err, false)
		}
		c.fillConfigData()
		c.view.List.SetCurrentItem(len(c.params.Config) - 1)
	})

	cForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 75, 17), true, true)
}

func (c *Controller) EditConfigEntry() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	entry := c.params.Config[i]
	cForm := c.view.NewCreateProfileForm("Edit config entry")

	cForm.GetFormItem(0).(*tview.InputField).SetText(entry.Name)
	cForm.GetFormItem(1).(*tview.InputField).SetText(entry.BaseUrl)
	if entry.Region != nil {
		cForm.GetFormItem(2).(*tview.InputField).SetText(*entry.Region)
	}
	cForm.GetFormItem(3).(*tview.InputField).SetText(entry.AccessKey)
	cForm.GetFormItem(4).(*tview.InputField).SetText(entry.SecretKey)
	cForm.GetFormItem(5).(*tview.Checkbox).SetChecked(entry.IgnoreSsl)

	cForm.AddButton("Save", func() {
		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		ignoreSsl := cForm.GetFormItem(5).(*tview.Checkbox).IsChecked()
		var region *string
		if reg != "" {
			region = &reg
		}

		entry.Name = name
		entry.BaseUrl = url
		entry.Region = region
		entry.AccessKey = accessKey
		entry.SecretKey = secretKey
		entry.IgnoreSsl = ignoreSsl

		c.params.WriteConfig()
		c.view.Pages.RemovePage("modal")
		c.fillConfigData()
		c.view.List.SetCurrentItem(i)
	})
	cForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 75, 17), true, true)
}

func (c *Controller) CopyProfile() {

	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	newName := fmt.Sprintf("%s_%s", cur, u.RandStr(4))

	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to copy config %s -> %s", cur, newName)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")

			if buttonLabel == "OK" {
				go func() {
					conf := *c.params.Config[i]
					conf.Name = newName
					c.params.CopyConfig(conf)
					c.fillConfigData()
					c.view.List.SetCurrentItem(len(c.params.Config) - 1)
				}()

			}
		})
	c.view.Pages.AddAndSwitchToPage("confirm", confirm, true)
}

func (c *Controller) create(isBucket bool) {
	var oTp string
	var disableBool bool
	if isBucket {
		oTp = "bucket"
		disableBool = true
	} else {
		oTp = "folder"
		disableBool = false
	}
	cForm := c.view.NewCreateForm(fmt.Sprintf("Create %s", oTp), disableBool)
	cForm.AddButton("Save", func() {
		var err error
		name := cForm.GetFormItem(0).(*tview.InputField).GetText()

		if name == "" {
			return
		}

		if isBucket {
			public := cForm.GetFormItem(1).(*tview.Checkbox).IsChecked()
			err = c.model.CreateBucket(&name, public)
		} else {
			key := path.Join(c.currentPath, name) + "/"
			err = c.model.CreateFolder(&key, c.currentBucket)
		}
		if err != nil {
			c.view.Pages.RemovePage("modal")
			go c.error("Error creating object", err, false)
			return
		}

		c.view.Pages.RemovePage("modal")

		// Run updateList in a goroutine
		go func() {
			keys, err := c.updateList()
			if err != nil {
				return
			}
			pos := getPosition(name, keys)

			// Set current item on UI thread
			c.view.App.QueueUpdateDraw(func() {
				c.view.List.SetCurrentItem(pos)
			})
		}()
	})

	cForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 65, 9), true, true)
}

func (c *Controller) Create() {
	if c.currentBucket == nil {
		c.create(true)
		return
	}
	c.create(false)
}

func (c *Controller) CheckProfile() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	cf := c.params.Config[i]

	mCf := model.NewConfig(cf.BaseUrl, cf.Region, cf.AccessKey, cf.SecretKey, !cf.IgnoreSsl)
	c.model = model.NewModel(mCf)

	_, err := c.model.ListBuckets()

	if err != nil {
		go c.error(fmt.Sprintf("error checking profile %s", cf.Name), err, false)
	} else {
		go c.success(fmt.Sprintf("successfully checked profile %s", cf.Name))
	}

}

func (c *Controller) DeleteConfigEntry() {

	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Do you want to delete to %s", cur)).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("confirm").SwitchToPage("main")

			if buttonLabel == "OK" {
				go func() {
					c.params.DeleteConfig(i)
					c.fillConfigData()
				}()

			}
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
}

func (c *Controller) setConfigInput() {
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		case tcell.KeyDelete:
			c.DeleteConfigEntry()
			return nil
		}
		return event
	})

	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlN:
			c.CreateConfigEntry()
			return nil
		case tcell.KeyCtrlE:
			c.EditConfigEntry()
			return nil
		case tcell.KeyCtrlY:
			c.CopyProfile()
			return nil
		case tcell.KeyCtrlH:
			help := c.view.HotkeysModal(true)

			// Close help on any key press inside the help view
			help.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-help")
				return nil
			})

			c.view.Pages.AddPage("modal-help", c.view.ModalEdit(help, 70, 18), true, true)
			return nil
		case tcell.KeyCtrlV:
			c.CheckProfile()
			return nil
		case tcell.KeyCtrlA:
			about := c.view.AboutModal()
			about.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-about")
				return nil
			})
			c.view.Pages.AddPage("modal-about", c.view.ModalEdit(about, 70, 19), true, true)
			return nil

		}
		return event
	})
}

func (c *Controller) setInput() {
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
		case tcell.KeyDelete:
			c.Delete()
			return nil
		case tcell.KeyBackspace2:
			c.Up()
			return nil
		case tcell.KeyCtrlN:
			c.Create()
			return nil
		case tcell.KeyCtrlD:
			c.Download()
			return nil
		case tcell.KeyCtrlP:
			c.Profiles()
			return nil
		case tcell.KeyCtrlL:
			cur := c.getSelectedObjectName()
			c.ShowFileProperties(cur)
			return nil
		case tcell.KeyCtrlU:
			c.ShowLocalFSModal(c.params.HomeDir)
			return nil
		case tcell.KeyCtrlH:
			help := c.view.HotkeysModal(false)

			// Close help on any key press inside the help view
			help.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-help")
				return nil
			})

			c.view.Pages.AddPage("modal-help", c.view.ModalEdit(help, 70, 18), true, true)
			return nil
		case tcell.KeyCtrlA:
			about := c.view.AboutModal()
			about.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-about")
				return nil
			})
			c.view.Pages.AddPage("modal-about", c.view.ModalEdit(about, 70, 19), true, true)
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
			i := c.view.List.GetCurrentItem()
			conf := c.params.Config[i]
			c.Duck(conf.BaseUrl, conf.Region, conf.AccessKey, conf.SecretKey, !conf.IgnoreSsl)
		})
	}
	c.view.SetFrameText("[::b][↓,↑][::-]Down/Up [::b][Enter[][::-]Use [::b][Ctrl+N[][::-]New [::b][Ctrl+Y[][::-]Yank(Copy) [::b][Ctrl+E[][::-]Edit [::b][Ctrl+V[][::-]Verify [::b][Del[][::-]Delete [::b][Ctrl+H[][::-]Hotkeys [::b][Ctrl+Q][::-]Quit")
}

func (c *Controller) fillDetails(key string) {
	c.view.Details.Clear()
	var otype string
	if val, ok := c.objs[key]; ok {
		switch ot := val.Ot; ot {
		case model.File:
			otype = "File"
		case model.Folder:
			otype = "Folder"
		case model.Bucket:
			otype = "Bucket"
		default:
			otype = "Unknown"
		}
		fmt.Fprintf(c.view.Details, "[green] Type: [white] %v\n", otype)
		if val.Ot == model.File {
			fmt.Fprintf(c.view.Details, "[green] Size: [white] %s\n", humanize.IBytes(uint64(*val.Size)))
		}
		if val.LastModified != nil {
			fmt.Fprintf(c.view.Details, "[green] Modified: [white] %v\n", val.LastModified)
		}
		if val.Etag != nil {
			fmt.Fprintf(c.view.Details, "[green] Etag: [white] %s\n\n", *val.Etag)
		}
		if val.Ot != model.Bucket {
			fmt.Fprintf(c.view.Details, "[green] FullPath: [white] %s\n\n", *val.FullPath)
		}
		if val.StorageClass != nil {
			fmt.Fprintf(c.view.Details, "[green] Storage class: [white] %s\n", *val.StorageClass)
		}
	}
}

func (c *Controller) Duck(url string, region *string, acc string, sec string, ssl bool) {
	mCf := model.NewConfig(url, region, acc, sec, ssl)
	c.model = model.NewModel(mCf)
	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		c.fillDetails(cur)
	})
	c.currentBucket = nil
	c.currentPath = ""
	c.bucketPos = 0
	go func() {
		if _, err := c.updateList(); err == nil {
			c.setInput()
		}
	}()
}

func (c *Controller) Run() error {
	c.Profiles()
	return c.view.App.Run()
}

func (c *Controller) error(header string, err error, fatal bool) {
	errMsg := c.view.NewErrorMessageQ(header, err.Error())
	errMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
	})

	// Call QueueUpdateDraw ONLY for the update
	c.view.App.QueueUpdateDraw(func() {
		c.view.Pages.AddPage("modal", c.view.ModalEdit(errMsg, 8, 3), true, true)
	})
}

func (c *Controller) success(header string) {
	succMsg := c.view.NewSuccessMessageQ(header)
	succMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
	})

	// Same here: just view update
	c.view.App.QueueUpdateDraw(func() {
		c.view.Pages.AddPage("modal", c.view.ModalEdit(succMsg, 8, 3), true, true)
	})
}

func (c *Controller) ShowLocalFSModal(startPath string) {
	if c.currentBucket == nil {
		return
	}
	currentPath := startPath
	layout, localList := c.view.NewCreateLocalFileListForm()

	app := c.view.App

	okBtn := tview.NewButton("Upload").SetSelectedFunc(func() {
		i := localList.GetCurrentItem()
		name, _ := localList.GetItemText(i)
		fullPath := filepath.Join(currentPath, strings.TrimSuffix(name, "/"))

		c.view.Pages.RemovePage("modal")
		err := c.Upload(fullPath)
		if err != nil {
			go c.error("Upload failed", err, false)
		}
	})

	cancelBtn := tview.NewButton("Cancel").SetSelectedFunc(func() {
		c.view.Pages.RemovePage("modal")
	})

	buttonRow := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(okBtn, 0, 1, false).
		AddItem(tview.NewBox(), 2, 0, false).
		AddItem(cancelBtn, 0, 1, false)

	flex, _ := layout.(*tview.Flex)
	flex.AddItem(buttonRow, 1, 0, false)

	// Maintain focusable order
	focusables := []tview.Primitive{localList, okBtn, cancelBtn}
	focusIndex := 0
	setNextFocus := func() {
		focusIndex = (focusIndex + 1) % len(focusables)
		app.SetFocus(focusables[focusIndex])
	}

	var renderList func(string)
	renderList = func(curPath string) {
		currentPath = curPath
		localList.Clear()
		localList.SetTitle(fmt.Sprintf("Local FS: %s", curPath)).SetBorder(true)

		entries, err := os.ReadDir(curPath)
		if err != nil {
			go c.error("Failed to read directory", err, false)
			return
		}

		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})

		for _, entry := range entries {
			name := entry.Name()
			fullPath := filepath.Join(curPath, name)
			display := name
			if entry.IsDir() {
				display += "/"
			}
			localList.AddItem(display, "", 0, func(p string, isDir bool) func() {
				return func() {
					if isDir {
						renderList(p)
					}
				}
			}(fullPath, entry.IsDir()))
		}
	}

	localList.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab:
			setNextFocus()
			return nil
		case tcell.KeyEsc:
			c.view.Pages.RemovePage("modal")
			return nil
		case tcell.KeyBackspace2:
			parent := filepath.Dir(currentPath)
			renderList(parent)
			return nil
		}
		return event
	})

	okBtn.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			setNextFocus()
			return nil
		}
		return event
	})
	cancelBtn.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			setNextFocus()
			return nil
		}
		return event
	})

	modal := c.view.ModalEdit(layout, 60, 25)
	c.view.Pages.AddPage("modal", modal, true, true)
	renderList(startPath)
}

func (c *Controller) Upload(localPath string) error {
	ctx, cancel := context.WithCancel(context.Background())

	progress := tview.NewModal().
		SetText("Starting upload...\n").
		AddButtons([]string{"Cancel"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cancel()
			c.view.Pages.RemovePage("progress").SwitchToPage("main")
		})

	files, totalSize, err := c.model.PrepareUpload(localPath, c.currentPath, c.currentBucket)
	if err != nil || len(files) == 0 {
		c.error("No files to upload", fmt.Errorf("nothing selected"), false)
		return nil
	}

	first := files[0]

	c.view.Pages.AddPage("progress", progress, true, true)
	progress.SetText(fmt.Sprintf(
		"Uploading\n0/%d file(s)\n0B/%s (0.0%%)\nLast: %s\n-> %s",
		len(files),
		humanize.IBytes(uint64(totalSize)),
		first.LocalPath,
		first.RemotePath,
	))

	var lastDraw time.Time
	throttle := 100 * time.Millisecond

	go func() {
		err := c.model.Upload(ctx, localPath, c.currentPath, c.currentBucket, func(n, total int64, i, count int, local, remote string) {
			select {
			case <-ctx.Done():
				return
			default:
			}

			now := time.Now()
			if now.Sub(lastDraw) < throttle {
				return
			}
			lastDraw = now

			percentage := float64(n) / float64(total) * 100
			c.view.App.QueueUpdateDraw(func() {
				progress.SetText(fmt.Sprintf(
					"Uploading\n%d/%d file(s)\n%s/%s (%.1f%%)\nLast: %s\n-> %s",
					i, count,
					humanize.IBytes(uint64(n)),
					humanize.IBytes(uint64(total)),
					percentage,
					local,
					remote,
				))
			})
		})

		select {
		case <-ctx.Done():
			go c.updateList()
			return
		default:
			if err != nil {
				c.view.Pages.RemovePage("progress").SwitchToPage("main")
				c.error("Upload failed", err, false)
				return
			}

			c.view.App.QueueUpdateDraw(func() {
				progress.ClearButtons()
				progress.SetText("Upload complete.\n\nPress Done to return.")
				progress.AddButtons([]string{"Done"})
				progress.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
					c.view.Pages.RemovePage("progress").SwitchToPage("main")
					go c.updateList()
				})
				c.view.App.SetFocus(progress)
			})
		}
	}()

	return nil
}

func (c *Controller) ShowFileProperties(key string) {
	obj, ok := c.objs[key]
	if !ok || obj.Ot != model.File {
		return
	}

	bucketName := *c.currentBucket.Key
	fullPath := *obj.FullPath

	var url string
	if strings.Contains(c.model.Cf.Url, "amazonaws.com") && c.model.Cf.Region != nil {
		url = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucketName, *c.model.Cf.Region, fullPath)
	} else {
		url = fmt.Sprintf("%s/%s/%s", c.model.Cf.Url, bucketName, fullPath)
	}

	text := fmt.Sprintf(
		"[black]Name: [white]%s\n[black]Size: [white]%s\n[black]Modified: [white]%v\n[black]Etag: [white]%s\n[black]Link: [black]%s",
		*obj.Key,
		humanize.IBytes(uint64(*obj.Size)),
		obj.LastModified,
		*obj.Etag,
		url,
	)

	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"Copy Link", "Close"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("modal")
			if buttonLabel == "Copy Link" {
				u.CopyToClipboard(url)
				go c.success("Link copied to clipboard")
			}
		})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(modal, 75, 12), true, true)
}

func (c *Controller) clearDetailsIfNoSelection() {
	c.view.App.QueueUpdateDraw(func() {
		if c.view.List.GetItemCount() == 0 {
			c.view.Details.Clear()
			return
		}
		i := c.view.List.GetCurrentItem()
		if i < 0 {
			c.view.Details.Clear()
			return
		}
		_, cur := c.view.List.GetItemText(i)
		if strings.TrimSpace(cur) == "" {
			c.view.Details.Clear()
		}
	})
}
