package view

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View ...
type View struct {
	App       *tview.Application
	Frame     *tview.Frame
	Pages     *tview.Pages
	List      *tview.List
	Details   *tview.TextView
	ModalEdit func(p tview.Primitive, width, height int) tview.Primitive
}

// NewView ...
func NewView() *View {
	app := tview.NewApplication()
	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitleAlign(tview.AlignLeft)

	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	tv.SetBorder(true).SetTitle("Details")

	main := tview.NewFlex()
	main.AddItem(list, 0, 2, true)
	main.AddItem(tv, 0, 3, false)

	pages := tview.NewPages().
		AddPage("main", main, true, true)

	modal := func(p tview.Primitive, width, height int) tview.Primitive {
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 0, 1, false).
				AddItem(p, height, 1, true).
				AddItem(nil, 0, 1, false), width, 1, true).
			AddItem(nil, 0, 1, false)
	}

	frame := tview.NewFrame(pages)
	frame.AddText("[::b][↓,↑][::-] Down/Up [::b][Enter,l/u][::-] Lower/Upper [::b][c[][::-] Create [::b][d[][::-] Delete [::b][e[][::-] [::b][Ctrl+q][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
		tv,
		modal,
	}

	return &v
}