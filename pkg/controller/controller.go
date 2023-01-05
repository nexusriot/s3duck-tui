package controller

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
	"github.com/rivo/tview"
)

type Controller struct {
	debug bool
	view  *view.View
	model *model.Model
}

func splitFunc(r rune) bool {
	return r == '/'
}

func NewController(
	debug bool,
) *Controller {
	m := model.NewModel(nil)
	bucket := "test-bkt-euc"
	m.Bucket = &bucket
	v := view.NewView()
	v.Frame.AddText(fmt.Sprintf("S3Duck TUI v.0.0.1"), true, tview.AlignCenter, tcell.ColorGreen)

	controller := Controller{
		debug: debug,
		view:  v,
		model: m,
	}
	return &controller
}

func (c *Controller) updateList() []*model.Object {
	c.view.List.Clear()
	// m := make(map[string]*model.Object)
	list, _ := c.model.List("")
	keys := make([]string, 0, len(list))
	for _, k := range list {
		keys = append(keys, *k.Key)
	}
	for _, key := range keys {
		c.view.List.AddItem(key, key, 0, func() {
		})
	}
	return list
}

func (c *Controller) Run() error {
	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		//c.fillDetails(cur)
	})
	c.updateList()
	//c.setInput()
	return c.view.App.Run()
}
