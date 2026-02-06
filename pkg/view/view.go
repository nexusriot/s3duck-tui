package view

import (
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const versionText = "S3Duck 🦆 TUI v.0.0.40"

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

	// Selection style: mid-blue background with white text to avoid clashes on light/dark terms
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

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
	errorQ.SetText(header + ": " + details).
		SetBackgroundColor(tcell.ColorRed).
		AddButtons([]string{"ok"})
	return errorQ
}

func (v *View) SetFrameText(helpText string) {
	v.Frame.Clear()
	v.SetHeaderVersionText(versionText)
	v.Frame.AddText(helpText, false, tview.AlignCenter, tcell.ColorWhite)
}

func (v *View) SetHeaderVersionText(version string) {
	v.Frame.AddText(fmt.Sprintf(version), true, tview.AlignCenter, tcell.ColorGreen)
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
	form.AddCheckbox("Disable ssl check", false, func(bool) {})
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
	localList := tview.NewList().
		ShowSecondaryText(false)

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(localList, 0, 2, true)
	return flex, localList
}

func (v *View) NewSuccessMessageQ(header string) *tview.Modal {
	successQ := tview.NewModal()
	successQ.SetText(header).
		SetBackgroundColor(tcell.ColorLime).
		AddButtons([]string{"ok"})
	return successQ
}

func (v *View) NewCreateForm(header string, disablePublic bool) *tview.Form {
	form := tview.NewForm()

	form.SetTitle(header)
	form.AddInputField("Name", "", 52, nil, nil)
	if disablePublic {
		form.AddCheckbox("Public?", false, func(bool) {})
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

func (v *View) HotkeysModal(profiles bool) *tview.TextView {
	helpText := `
		[::b]Navigation[::-]
          [↓,↑]Down/Up 
		  Enter         Open selected profile

		[::b]Actions[::-]
		  Ctrl+N        Create new profile
          Ctrl+Y        Copy profile
		  Ctrl+E        Edit profile
          Ctrl+V        Verify profile (test connection)
		  Del           Delete profile

		[::b]Misc[::-]
		  Ctrl+H        This help
	      Ctrl+A        Show About
		  Ctrl+Q        Quit
		
		[dim]Press any key to close.[-]
	`
	if !profiles {
		helpText = `
		[::b]Navigation[::-]
		  [↓,↑]Down/Up 
		  Enter         Open folder / select
		  Backspace     Up ([..])
          Ctrl+P        Show Profiles

		[::b]Actions[::-]
		  Ctrl+N        Create bucket / folder
		  Ctrl+D        Download file/folder (for files and folders)
          Ctrl+G        Bucket/folder summary
          Ctrl+U        Open local file manager (for upload)
          Space			Select object for download
          Ctrl+S        Select all objects for download
          Ctrl+X        Unselect all objects for download
		  Del           Delete (recursive for dirs)

		[::b]Misc[::-]
		  Ctrl+H        This help
          Ctrl+A        Show About
		  Ctrl+Q        Quit
		
		[dim]Press any key to close.[-]
	`
	}

	tv := tview.NewTextView()
	tv.SetDynamicColors(true)
	tv.SetTextAlign(tview.AlignLeft)
	tv.SetWordWrap(true)
	tv.SetText(helpText)
	tv.SetBorder(true)
	tv.SetTitle(" Hotkeys ")

	return tv
}

func (v *View) AboutModal() *tview.TextView {
	about := `
                         [::b]%s[::-]

			A tiny TUI browser for etcd S3-like storage.
			Github: https://github.com/nexusriot/s3duck-tui

			(C)2023-2026 Vladislav Ananev
			
                    _  [dim](quack)[-]
				 __( )> 
				 \__\      [::b]Features[::-]
							• Profiles support
							• Walking dirs support
							• Download files/dirs support
							• Uploads files/dirs support
                            • Summary view support
         [dim]Press any key to close.[-]
			`

	tv := tview.NewTextView()
	tv.SetDynamicColors(true)
	tv.SetTextAlign(tview.AlignLeft)
	tv.SetWordWrap(true)
	tv.SetText(fmt.Sprintf(about, versionText))
	tv.SetBorder(true)
	tv.SetTitle(" About ")

	// ensure redraw on content changes
	tv.SetChangedFunc(func() { v.App.Draw() })

	return tv
}

func HumanizeBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	return humanize.Bytes(uint64(b))
}

// SummaryRow is a generic row for summary charts/tables.
type SummaryRow struct {
	Name  string
	Bytes int64
}

// SummaryGraph holds primitives of a graphical summary modal.
type SummaryGraph struct {
	Root   tview.Primitive
	Header *tview.TextView
	Cats   *tview.Table
	Groups *tview.Table
}

func (v *View) NewSummaryGraph(title string, scope string, total int64, categories []SummaryRow, groups []SummaryRow, onSelectGroup func(groupName string)) *SummaryGraph {
	h := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft).
		SetWordWrap(true)
	if title == "" {
		title = " Summary "
	}
	h.SetBorder(true).SetTitle(title)
	h.SetText(fmt.Sprintf("[::b]%s[::-]\nTotal: [::b]%s[::-] (%d bytes)\n\n[gray]Tab: switch focus • Enter: drill-down • Esc/q: close[::-]",
		scope, HumanizeBytes(total), total))

	catT := tview.NewTable().SetBorders(false).SetSelectable(true, false)
	catT.SetBorder(true).SetTitle(" Categories ")
	catT.SetFixed(1, 0)

	catT.SetCell(0, 0, tview.NewTableCell("[::b]Type[::-]").SetSelectable(false))
	catT.SetCell(0, 1, tview.NewTableCell("[::b]Size[::-]").SetSelectable(false).SetAlign(tview.AlignRight))
	catT.SetCell(0, 2, tview.NewTableCell("[::b]%[::-]").SetSelectable(false).SetAlign(tview.AlignRight))
	catT.SetCell(0, 3, tview.NewTableCell("[::b]Chart[::-]").SetSelectable(false))

	style := map[string]tcell.Color{
		"documents": tcell.ColorGreen,
		"archives":  tcell.ColorYellow,
		"media":     tcell.ColorBlue,
		"other":     tcell.ColorGray,
	}

	for i, row := range categories {
		r := i + 1
		name := row.Name
		key := strings.ToLower(name)
		if strings.HasPrefix(key, "doc") {
			key = "documents"
		} else if strings.HasPrefix(key, "arch") {
			key = "archives"
		} else if strings.HasPrefix(key, "med") {
			key = "media"
		} else if strings.HasPrefix(key, "oth") {
			key = "other"
		}

		pct := 0.0
		if total > 0 {
			pct = (float64(row.Bytes) / float64(total)) * 100.0
		}

		catT.SetCell(r, 0, tview.NewTableCell(name))
		catT.SetCell(r, 1, tview.NewTableCell(HumanizeBytes(row.Bytes)).SetAlign(tview.AlignRight))
		catT.SetCell(r, 2, tview.NewTableCell(fmt.Sprintf("%.1f", pct)).SetAlign(tview.AlignRight))
		catT.SetCell(r, 3, tview.NewTableCell(renderColorBar(pct, style[key])).SetExpansion(1))
	}

	grpT := tview.NewTable().SetBorders(false).SetSelectable(true, false)
	grpT.SetBorder(true).SetTitle(" Top groups ")
	grpT.SetFixed(1, 0)

	grpT.SetCell(0, 0, tview.NewTableCell("[::b]Group[::-]").SetSelectable(false))
	grpT.SetCell(0, 1, tview.NewTableCell("[::b]Size[::-]").SetSelectable(false).SetAlign(tview.AlignRight))
	grpT.SetCell(0, 2, tview.NewTableCell("[::b]%[::-]").SetSelectable(false).SetAlign(tview.AlignRight))

	for i, row := range groups {
		r := i + 1
		pct := 0.0
		if total > 0 {
			pct = (float64(row.Bytes) / float64(total)) * 100.0
		}
		grpT.SetCell(r, 0, tview.NewTableCell(row.Name))
		grpT.SetCell(r, 1, tview.NewTableCell(HumanizeBytes(row.Bytes)).SetAlign(tview.AlignRight))
		grpT.SetCell(r, 2, tview.NewTableCell(fmt.Sprintf("%.1f", pct)).SetAlign(tview.AlignRight))
	}

	if onSelectGroup != nil {
		grpT.SetSelectedFunc(func(row, _ int) {
			if row <= 0 {
				return
			}
			name := grpT.GetCell(row, 0).Text
			onSelectGroup(name)
		})
	}

	grid := tview.NewGrid().SetRows(5, 0).SetColumns(0, 0).SetBorders(false)
	grid.AddItem(h, 0, 0, 1, 2, 0, 0, false)
	grid.AddItem(catT, 1, 0, 1, 1, 0, 0, true)
	grid.AddItem(grpT, 1, 1, 1, 1, 0, 0, false)

	return &SummaryGraph{Root: grid, Header: h, Cats: catT, Groups: grpT}
}

func renderColorBar(pct float64, col tcell.Color) string {
	const cells = 26
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int((pct / 100.0) * float64(cells))
	if filled < 0 {
		filled = 0
	}
	if filled > cells {
		filled = cells
	}

	tag := "white"
	switch col {
	case tcell.ColorGreen:
		tag = "green"
	case tcell.ColorYellow:
		tag = "yellow"
	case tcell.ColorBlue:
		tag = "blue"
	case tcell.ColorGray:
		tag = "gray"
	}

	var b strings.Builder
	b.WriteString("[")
	b.WriteString(tag)
	b.WriteString("]")
	for i := 0; i < filled; i++ {
		b.WriteRune('█')
	}
	b.WriteString("[-]")
	for i := filled; i < cells; i++ {
		b.WriteRune('░')
	}
	return b.String()
}
