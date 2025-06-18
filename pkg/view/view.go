package view

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const versionText = "S3Duck 🦆 TUI v.0.0.9 - preview"

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
	tv.SetBorder(true)
	main := tview.NewFlex()
	main.AddItem(list, 0, 4, true)
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

func (v *View) NewErrorMessageQ(header string, details string) *tview.Modal {
	errorQ := tview.NewModal()
	errorQ.SetText(header + ": " + details).SetBackgroundColor(tcell.ColorRed).AddButtons([]string{"ok"})
	return errorQ
}

func (v *View) SetFrameText(helpText string) {
	v.Frame.Clear()
	v.SetHeaderVersionText(versionText)
	v.Frame.AddText(helpText, false, tview.AlignCenter, tcell.ColorWhite)
}

func (v *View) SetHeaderVersionText(versionText string) {
	v.Frame.AddText(fmt.Sprintf(versionText), true, tview.AlignCenter, tcell.ColorGreen)
}

func (v *View) NewConfirm() *tview.Modal {
	return tview.NewModal().AddButtons([]string{"OK", "Cancel"})
}

func (v *View) NewCreateProfileForm(header string) *tview.Form {
	form := tview.NewForm()

	form.SetTitle(header)
	form.AddInputField("Name", "", 52, nil, nil)
	form.AddInputField("Url", "", 52, nil, nil)
	form.AddInputField("Region", "", 52, nil, nil)
	form.AddInputField("Access key", "", 52, nil, nil)
	form.AddPasswordField("Secret key", "", 52, '*', nil)
	form.AddCheckbox("Disable ssl check", false, func(checked bool) {
	})
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

func (v *View) NewCreateLocalFileListForm() (tview.Primitive, *tview.List) {
	var localList *tview.List
	localList = tview.NewList().
		ShowSecondaryText(false)

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(localList, 0, 2, true)
	return flex, localList
}

func (v *View) NewSuccessMessageQ(header string) *tview.Modal {
	successQ := tview.NewModal()
	successQ.SetText(header).SetBackgroundColor(tcell.ColorLime).AddButtons([]string{"ok"})
	return successQ
}

func (v *View) NewCreateForm(header string, disablePublic bool) *tview.Form {
	form := tview.NewForm()

	form.SetTitle(header)
	form.AddInputField("Name", "", 52, nil, nil)
	if disablePublic {
		form.AddCheckbox("Public?", false, func(checked bool) {})
	}
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}
