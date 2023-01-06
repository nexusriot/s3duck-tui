package controller

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
	u "github.com/nexusriot/s3duck-tui/pkg/utils"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

type Controller struct {
	debug         bool
	view          *view.View
	model         *model.Model
	buckets       []*model.Object
	objs          map[string]*model.Object
	currentPath   string
	currentBucket *model.Object
}

func NewController(debug bool) *Controller {
	m := model.NewModel(nil)
	v := view.NewView()
	v.Frame.AddText(fmt.Sprintf("S3Duck TUI v.0.0.1 - PoC"), true, tview.AlignCenter, tcell.ColorGreen)

	controller := Controller{
		debug:       debug,
		view:        v,
		model:       m,
		currentPath: "",
	}
	return &controller
}

func (c *Controller) makeObjectMap() error {
	var list []*model.Object
	var err error
	dirs := make(map[string]*model.Object)

	if c.currentBucket == nil {
		list, err = c.model.ListBuckets()
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

func (c *Controller) updateList() error {
	c.view.List.Clear()
	var title string
	if c.currentBucket == nil {
		title = "(buckets)"
	} else {
		title = fmt.Sprintf("(%s)/%s", *c.currentBucket.Key, c.currentPath)
	}
	c.view.List.SetTitle(title)
	err := c.makeObjectMap()

	keys := make([]string, 0, len(c.objs))
	for _, k := range c.objs {
		keys = append(keys, *k.Key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c.view.List.AddItem(key, key, 0, func() {
			i := c.view.List.GetCurrentItem()
			_, cur := c.view.List.GetItemText(i)
			cur = strings.TrimSpace(cur)
			if val, ok := c.objs[cur]; ok {
				if val.Ot == model.Folder || val.Ot == model.Bucket {
					c.Down(cur)
				}
			}
		})
	}
	return err
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

func (c *Controller) setInput() {
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
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
			case 'u':
				c.Up()
				return nil
			}
		}
		return event
	})
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
			fmt.Fprintf(c.view.Details, "[green] Size: [white] %d\n", val.Size)
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

func (c *Controller) Run() error {
	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		c.fillDetails(cur)
	})
	c.updateList()
	c.setInput()
	return c.view.App.Run()
}
