package controller

import (
	"fmt"
	"github.com/nexusriot/s3duck-tui/pkg/view"
	"github.com/rivo/tview"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/pkg/model"
	u "github.com/nexusriot/s3duck-tui/pkg/utils"
)

type Controller struct {
	debug         bool
	view          *view.View
	model         *model.Model
	buckets       []*model.Object
	objs          map[string]*model.Object
	currentPath   string
	currentBucket *model.Object
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
		position:    make(map[string]int),
		params:      params,
	}
	return &controller
}

func (c *Controller) makeObjectMap() error {
	var list []*model.Object
	var err error
	dirs := make(map[string]*model.Object)

	if c.currentBucket == nil {
		list, err = c.model.ListBuckets()
		if err != nil {
			c.error("Failed to list buckets", err, true)
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
						if err != nil {
							c.error(fmt.Sprintf("Failed to delete %s", cur), err, false)
						}
						c.updateList()
					}()
				}
			})
		c.view.Pages.AddAndSwitchToPage("confirm", confirm, true)
	}
	return nil
}

func (c *Controller) Download() error {

	cur := c.getSelectedObjectName()
	if val, ok := c.objs[cur]; ok {
		if val.Ot == model.Folder || val.Ot == model.File {

			cwd := path.Join(c.params.HomeDir, "Downloads")
			cwd = cwd + fmt.Sprintf("%c", filepath.Separator)
			key := c.currentPath + cur
			if val.Ot == model.Folder {
				key = key + "/"
			}
			totalSize := int64(0)
			objects := c.model.ListObjects(key, c.currentBucket)

			for _, o := range objects {
				totalSize += o.Size
			}
			nos := len(objects)
			progress := c.view.NewProgressMessage()

			confirm := c.view.NewConfirm()
			confirm.SetText(fmt.Sprintf("Do you want to download to %s\n%d object(s)\ntotal size %s",
				cwd,
				nos,
				humanize.IBytes(uint64(totalSize)),
			)).
				SetDoneFunc(func(buttonIndex int, buttonLabel string) {
					c.view.Pages.RemovePage("confirm").SwitchToPage("main")

					if buttonLabel == "OK" {
						c.view.Pages.AddAndSwitchToPage("progress", progress, true)

						go func() {
							downloadedSize := int64(0)
							title := "Downloading"

							for i, object := range objects {
								n, err := c.model.Download(object, c.currentPath, cwd, c.currentBucket.Key)

								if err != nil {
									c.view.Pages.RemovePage("progress").SwitchToPage("main")
									c.error(fmt.Sprintf("Failed to download %s", *object.Key), err, false)
								}
								downloadedSize += n
								if i+1 == nos {
									title = "Downloaded"
								}
								c.view.App.QueueUpdateDraw(func() {
									progress.SetText(fmt.Sprintf("%s\n%d/%d objects\n%s/%s",
										title,
										i+1,
										nos,
										humanize.IBytes(uint64(downloadedSize)),
										humanize.IBytes(uint64(totalSize)),
									))
								})
							}
							progress.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
								c.view.Pages.RemovePage("progress").SwitchToPage("main")
							})
						}()
					}
				})
			c.view.Pages.AddAndSwitchToPage("confirm", confirm, true)
		}
	}
	return nil
}

func (c *Controller) updateList() ([]string, error) {
	c.view.List.Clear()
	var title string
	var suff string
	if c.currentBucket == nil {
		title = "(buckets)"
		suff = ""
	} else {
		title = fmt.Sprintf("(%s)/%s", *c.currentBucket.Key, c.currentPath)
		suff = "[::b][d[][::-]Download [::b] "
	}
	fText := fmt.Sprintf("[::b][↓,↑][::-]Down/Up [::b][Enter/Backspace][::-]Lower/Upper %s[Del[][::-]Delete [P[][::-]Profiles [::b][Ctrl+q][::-]Quit", suff)
	c.view.SetFrameText(fText)
	c.view.List.SetTitle(title)
	err := c.makeObjectMap()
	if err != nil {
		c.view.Pages.RemovePage("modal")
		c.error(fmt.Sprintf("Failed to fetch"), err, true)
		return nil, err
	}
	keys := make([]string, 0, len(c.objs))
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

	for _, v := range objs {
		keys = append(keys, *v.Key)

	}
	for _, key := range keys {
		c.view.List.AddItem(key, key, 0, func() {
			i := c.view.List.GetCurrentItem()
			_, cur := c.view.List.GetItemText(i)
			cur = strings.TrimSpace(cur)
			if val, ok := c.objs[cur]; ok {
				if val.Ot == model.Folder || val.Ot == model.Bucket {
					if val.Ot == model.Folder {
						c.position[c.currentPath] = c.view.List.GetCurrentItem()
					}
					c.Down(cur)
				}
			}
		})
	}
	if c.currentBucket != nil {
		if val, ok := c.position[c.currentPath]; ok {
			c.view.List.SetCurrentItem(val)
			delete(c.position, c.currentPath)
		}
	}
	return keys, err
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
	c.updateList()
}

func (c *Controller) Up() {
	c.view.Details.Clear()
	if c.currentPath == "" {
		c.currentBucket = nil
	}
	fields := strings.FieldsFunc(strings.TrimSpace(c.currentPath), u.SplitFunc)
	if len(fields) == 0 {
		c.updateList()
		return
	}
	newDir := strings.Join(fields[:len(fields)-1], "/")
	if len(fields) > 1 {
		newDir = newDir + "/"
	}
	c.currentPath = newDir
	c.updateList()
}

func (c *Controller) Stop() {
	c.view.App.Stop()
}

func (c *Controller) CreateConfigEntry() {
	cForm := c.view.NewCreateForm("Create config entry")
	cForm.AddButton("Save", func() {
		var region *string

		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()

		if reg != "" {
			region = &reg
		}

		fmt.Println(name)
		conf := cfg.Config{
			Name:      name,
			BaseUrl:   url,
			Region:    region,
			AccessKey: accessKey,
			SecretKey: secretKey,
		}
		err := c.params.NewConfiguration(&conf)

		c.view.Pages.RemovePage("modal")
		if err != nil {
			c.error("Error creating config entry", err, false)
		}
		c.fillConfigData()
		c.view.List.SetCurrentItem(len(c.params.Config) - 1)
	})

	cForm.AddButton("Quit", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 60, 15), true, true)
}

func (c *Controller) EditConfigEntry() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	entry := c.params.Config[i]
	cForm := c.view.NewCreateForm("Edit config entry")

	cForm.GetFormItem(0).(*tview.InputField).SetText(entry.Name)
	cForm.GetFormItem(1).(*tview.InputField).SetText(entry.BaseUrl)
	if entry.Region != nil {
		cForm.GetFormItem(2).(*tview.InputField).SetText(*entry.Region)
	}
	cForm.GetFormItem(3).(*tview.InputField).SetText(entry.AccessKey)
	cForm.GetFormItem(4).(*tview.InputField).SetText(entry.SecretKey)

	cForm.AddButton("Save", func() {
		name := cForm.GetFormItem(0).(*tview.InputField).GetText()
		url := cForm.GetFormItem(1).(*tview.InputField).GetText()
		reg := cForm.GetFormItem(2).(*tview.InputField).GetText()
		accessKey := cForm.GetFormItem(3).(*tview.InputField).GetText()
		secretKey := cForm.GetFormItem(4).(*tview.InputField).GetText()
		var region *string
		if reg != "" {
			region = &reg
		}

		entry.Name = name
		entry.BaseUrl = url
		entry.Region = region
		entry.AccessKey = accessKey
		entry.SecretKey = secretKey

		c.params.WriteConfig()
		c.view.Pages.RemovePage("modal")
		c.fillConfigData()
		c.view.List.SetCurrentItem(i)
	})
	cForm.AddButton("Quit", func() {
		c.view.Pages.RemovePage("modal")
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(cForm, 60, 15), true, true)
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

func (c *Controller) CheckProfile() {
	if c.view.List.GetItemCount() == 0 {
		return
	}
	i := c.view.List.GetCurrentItem()

	cf := c.params.Config[i]

	mCf := model.NewConfig(cf.BaseUrl, cf.Region, cf.AccessKey, cf.SecretKey)
	c.model = model.NewModel(mCf)

	_, err := c.model.ListBuckets()

	if err != nil {
		c.error(fmt.Sprintf("error checking profile %s", cf.Name), err, false)
	} else {
		c.success(fmt.Sprintf("successfully checked profile %s", cf.Name))
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
	c.view.Pages.AddAndSwitchToPage("confirm", confirm, true)
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
		case tcell.KeyRune:
			switch event.Rune() {
			// TODO: ctrl + key
			case 'n':
				c.CreateConfigEntry()
				return nil
			case 'e':
				c.EditConfigEntry()
				return nil
			case 'c':
				c.CopyProfile()
				return nil
			case 'v':
				c.CheckProfile()
				return nil
			}
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
		case tcell.KeyDelete:
			c.Delete()
			return nil
		case tcell.KeyBackspace2:
			c.Up()
			return nil
		}
		return event
	})
	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyRune:
			switch event.Rune() {
			case 'd':
				c.Download()
				return nil
			case 'p':
				c.Profiles()
				return nil
			}
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
			_ = c.Duck(conf.BaseUrl, conf.Region, conf.AccessKey, conf.SecretKey)
		})
	}
	c.view.SetFrameText("[::b][↓,↑][::-]Down/Up [::b][Enter[][::-]Use Profile [::b][n[][::-]Create [::b][c[][::-]Copy [::b][e[][::-]Edit [::b][v[][::-]Verify [::b][Del[][::-]Delete")
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

func (c *Controller) Duck(url string, region *string, acc string, sec string) error {
	mCf := model.NewConfig(url, region, acc, sec)
	c.model = model.NewModel(mCf)
	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		c.fillDetails(cur)
	})
	c.currentBucket = nil
	c.currentPath = ""
	c.updateList()
	c.setInput()
	return nil
}

func (c *Controller) Run() error {
	c.Profiles()
	return c.view.App.Run()
}

func (c *Controller) error(header string, err error, fatal bool) {
	errMsg := c.view.NewErrorMessageQ(header, err.Error())
	errMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
		if fatal {
			c.view.App.Stop()
		}
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(errMsg, 8, 3), true, true)
}

func (c *Controller) success(header string) {
	succMsg := c.view.NewSuccessMessageQ(header)
	succMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(succMsg, 8, 3), true, true)
}
