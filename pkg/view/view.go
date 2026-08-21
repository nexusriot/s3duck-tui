package view

import (
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const versionText = "S3Duck 🦆 TUI v.0.8.0"

// View ...
type View struct {
	App       *tview.Application
	Frame     *tview.Frame
	Pages     *tview.Pages
	List      *tview.List       // the active pane's list (repointed on pane switch)
	Filter    *tview.InputField // the active pane's filter
	Header    *tview.TextView   // the active pane's column header
	Details   *tview.TextView
	ModalEdit func(p tview.Primitive, width, height int) tview.Primitive

	// Dual-pane widgets. Pane i always owns lists[i]/filters[i]/cols[i]; the
	// active pane's widgets are mirrored into List/Filter. main is the root
	// content flex, rebuilt when toggling single/dual layout.
	lists   [2]*tview.List
	filters [2]*tview.InputField
	headers [2]*tview.TextView
	cols    [2]*tview.Flex
	main    *tview.Flex
}

// newPaneColumn builds one browser pane: a column-header line above a bordered
// list above a one-line filter box.
func newPaneColumn() (*tview.List, *tview.InputField, *tview.TextView, *tview.Flex) {
	header := tview.NewTextView()
	header.SetDynamicColors(true)
	header.SetTextColor(tcell.ColorGray)

	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitleAlign(tview.AlignLeft)
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	filter := tview.NewInputField()
	filter.SetLabel(" Filter: ")
	filter.SetPlaceholder("press / to filter, Enter to keep, Esc to clear")
	filter.SetFieldWidth(0)
	filter.SetFieldBackgroundColor(tcell.ColorBlack)
	filter.SetPlaceholderTextColor(tcell.ColorGray)

	col := tview.NewFlex().SetDirection(tview.FlexRow)
	col.AddItem(header, 1, 0, false)
	col.AddItem(list, 0, 1, true)
	col.AddItem(filter, 1, 0, false)
	return list, filter, header, col
}

// PaneList / PaneFilter / PaneHeader expose a specific pane's widgets to the
// controller.
func (v *View) PaneList(i int) *tview.List         { return v.lists[i] }
func (v *View) PaneFilter(i int) *tview.InputField { return v.filters[i] }
func (v *View) PaneHeader(i int) *tview.TextView   { return v.headers[i] }

// ShowSinglePane lays out the primary pane beside the details panel (the
// classic layout).
func (v *View) ShowSinglePane() {
	v.main.Clear()
	v.main.AddItem(v.cols[0], 0, 4, true)
	v.main.AddItem(v.Details, 0, 3, false)
	v.lists[0].SetBorderColor(tcell.ColorWhite)
}

// ShowDualPane lays out the two panes side by side (details hidden), MC-style.
func (v *View) ShowDualPane() {
	v.main.Clear()
	v.main.AddItem(v.cols[0], 0, 1, true)
	v.main.AddItem(v.cols[1], 0, 1, false)
}

// SetActivePane highlights the active pane's border (dual-pane only).
func (v *View) SetActivePane(active int) {
	for i, l := range v.lists {
		if i == active {
			l.SetBorderColor(tcell.ColorGreen)
		} else {
			l.SetBorderColor(tcell.ColorGray)
		}
	}
}

// NewView ...
func NewView() *View {
	app := tview.NewApplication()

	list0, filter0, header0, col0 := newPaneColumn()
	list1, filter1, header1, col1 := newPaneColumn()

	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	tv.SetBorder(true)

	// Start in single-pane layout: primary pane beside the details panel.
	main := tview.NewFlex()
	main.AddItem(col0, 0, 4, true)
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
		App:       app,
		Frame:     frame,
		Pages:     pages,
		List:      list0,
		Filter:    filter0,
		Header:    header0,
		Details:   tv,
		ModalEdit: modal,
		lists:     [2]*tview.List{list0, list1},
		filters:   [2]*tview.InputField{filter0, filter1},
		headers:   [2]*tview.TextView{header0, header1},
		cols:      [2]*tview.Flex{col0, col1},
		main:      main,
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
	v.Frame.AddText(version, true, tview.AlignCenter, tcell.ColorGreen)
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
	form.AddPasswordField("Session token (optional)", "", 52, '*', nil)
	form.AddInputField("Download dir", "", 52, nil, nil)
	form.AddCheckbox("Disable ssl check", false, func(bool) {})
	form.AddInputField("Max bytes/sec (0=unltd)", "", 52, nil, nil)
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

// NewInputForm builds a single-field form pre-filled with value, used by
// rename. Esc closes the "modal" page.
func (v *View) NewInputForm(header, label, value string) *tview.Form {
	form := tview.NewForm()
	form.SetTitle(header)
	form.AddInputField(label, value, 50, nil, nil)
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

// NewSearchForm builds the recursive-search form: a query input (item 0) and an
// "All buckets" checkbox (item 1). Esc closes the "modal" page.
func (v *View) NewSearchForm(header string) *tview.Form {
	form := tview.NewForm()
	form.SetTitle(header)
	form.AddInputField("Query", "", 50, nil, nil)
	form.AddCheckbox("All buckets", false, nil)
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

// NewBatchRenameForm builds the batch/pattern rename form: a pattern template
// (item 0) with {name}/{ext}/{n} tokens, and an optional find (item 1) /
// replacement (item 2) pair applied to the name. Esc closes the "modal" page.
func (v *View) NewBatchRenameForm(header string) *tview.Form {
	form := tview.NewForm()
	form.SetTitle(header)
	form.AddInputField("Pattern ({name} {ext} {n})", "{name}{ext}", 50, nil, nil)
	form.AddInputField("Find (optional)", "", 50, nil, nil)
	form.AddInputField("Replace with", "", 50, nil, nil)
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

// NewCopyMoveForm builds the copy/move destination form: a bucket dropdown
// (item 0, defaulting to currentBucket) plus a destination-prefix input
// (item 1). Buckets other than the current one make the transfer cross-bucket.
// Esc closes the "modal" page.
func (v *View) NewCopyMoveForm(header string, buckets []string, currentBucket, prefix string) *tview.Form {
	initial := 0
	for i, b := range buckets {
		if b == currentBucket {
			initial = i
			break
		}
	}

	form := tview.NewForm()
	form.SetTitle(header)
	form.AddDropDown("Destination bucket", buckets, initial, nil)
	form.AddInputField("Destination prefix", prefix, 50, nil, nil)
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

// Field labels for the sync form, read back with GetFormItemByLabel.
const (
	FieldSyncDirection = "Direction"
	FieldSyncLocalDir  = "Local directory"
	FieldSyncDstBucket = "Dest bucket (remote → remote)"
	FieldSyncDstPrefix = "Dest prefix (remote → remote)"
	FieldSyncDelete    = "Delete extraneous at destination"
)

// NewSyncForm builds the sync dialog. The current bucket+prefix shown in the
// title is always one side of the transfer — the source, except for an upload.
// The local-directory and destination-bucket rows are both always present:
// which one applies follows from the direction, and hiding rows on selection
// would mean rebuilding the form mid-interaction. Esc closes the "modal" page.
func (v *View) NewSyncForm(current, localDir string, directions, buckets []string, dstBucket, dstPrefix string) *tview.Form {
	initialBucket := 0
	for i, b := range buckets {
		if b == dstBucket {
			initialBucket = i
			break
		}
	}

	form := tview.NewForm()
	form.SetTitle(fmt.Sprintf(" Sync — this pane: %s ", current))
	form.AddDropDown(FieldSyncDirection, directions, 0, nil)
	form.AddInputField(FieldSyncLocalDir, localDir, 56, nil, nil)
	form.AddDropDown(FieldSyncDstBucket, buckets, initialBucket, nil)
	form.AddInputField(FieldSyncDstPrefix, dstPrefix, 56, nil, nil)
	form.AddCheckbox(FieldSyncDelete, false, nil)
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

// Field labels for the metadata and storage-class forms. Both forms are read
// back with GetFormItemByLabel rather than by index: they mix TextViews with
// inputs, and a positional read silently returns the wrong widget the moment a
// row is inserted (a trap the profile form already sprung once).
const (
	FieldContentType        = "Content-Type"
	FieldCacheControl       = "Cache-Control"
	FieldContentDisposition = "Content-Disposition"
	FieldContentEncoding    = "Content-Encoding"
	FieldUserMetadata       = "Metadata (key=value per line)"
	FieldTags               = "Tags (key=value per line)"
	FieldStorageClass       = "Storage class"
	FieldRestoreDays        = "Restore for (days)"
	FieldRestoreTier        = "Retrieval tier"
)

// NewObjectMetaForm builds the metadata + tags editor. Read its fields with
// GetFormItemByLabel and the Field* constants above. tagsUnavailable notes a
// backend that rejected GetObjectTagging, so the user knows an empty tag box
// means "not supported" rather than "no tags". Esc closes the "modal" page.
func (v *View) NewObjectMetaForm(key, summary string, tagsUnavailable bool) *tview.Form {
	form := tview.NewForm()
	form.SetTitle(fmt.Sprintf(" Metadata & tags — %s ", key))

	form.AddTextView("", summary, 0, 2, true, false)
	form.AddInputField(FieldContentType, "", 56, nil, nil)
	form.AddInputField(FieldCacheControl, "", 56, nil, nil)
	form.AddInputField(FieldContentDisposition, "", 56, nil, nil)
	form.AddInputField(FieldContentEncoding, "", 56, nil, nil)
	form.AddTextArea(FieldUserMetadata, "", 56, 5, 0, nil)
	form.AddTextArea(FieldTags, "", 56, 5, 0, nil)
	if tagsUnavailable {
		form.AddTextView("", "[red]this endpoint reported no tagging support[-]", 0, 1, true, false)
	}
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			// The editor lives on its own page (see metaFormPage in the
			// controller) so error modals can't replace it.
			v.Pages.RemovePage("modal-meta")
		}
		return event
	})
	return form
}

// NewStorageClassForm builds the storage-class / restore dialog: a class
// dropdown plus, only when the object is archived, restore days and a retrieval
// tier. Read its fields with GetFormItemByLabel. Esc closes the "modal" page.
func (v *View) NewStorageClassForm(key, summary string, classes []string, current string, tiers []string, archived bool) *tview.Form {
	initial := 0
	for i, cl := range classes {
		if cl == current {
			initial = i
			break
		}
	}

	form := tview.NewForm()
	form.SetTitle(fmt.Sprintf(" Storage class — %s ", key))
	form.AddTextView("", summary, 0, 2, true, false)
	form.AddDropDown(FieldStorageClass, classes, initial, nil)
	if archived {
		form.AddInputField(FieldRestoreDays, "1", 8, tview.InputFieldInteger, nil)
		form.AddDropDown(FieldRestoreTier, tiers, 0, nil)
	}
	form.SetBorder(true)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
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
          Ctrl+I        Import profile from ~/.aws (incl. session token)
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
          [ / ]         History back / forward (also Alt+left/right)
        Ctrl+O        Toggle dual-pane
        Tab           Switch active pane (dual-pane)
        Ctrl+B        Bookmarks (go / add / remove)
        Ctrl+K        Command palette
        Ctrl+P        Show Profiles

		[::b]Actions[::-]
		  Ctrl+N        Create bucket / folder
		  Ctrl+D        Download file/folder (for files and folders)
          Ctrl+R        Rename (pattern rename when >1 marked)
          Ctrl+Y        Copy selected/marked to a destination bucket/prefix
          Ctrl+T        Move selected/marked to a destination bucket/prefix
          Ctrl+G        Bucket/folder summary
          Ctrl+L        File properties (size, ETag, link)
          v             Version history (restore / download / delete)
          m             Edit metadata & object tags
          c             Storage class / Glacier restore
          Ctrl+W        Copy presigned (time-limited) share link
          Ctrl+U        Open local file manager (for upload)
          Ctrl+E        Sync: local ⇄ this prefix, or this prefix → another
          =             Compare the two panes (dual-pane, read-only)
          D             Find duplicates under this prefix (size + ETag)
          e             Edit object in $EDITOR (small text objects)
          >             Copy marked items to another profile
          y / x / p     Clipboard: copy / cut / paste objects
          u             Undo last move/rename
          t             Transfers panel (background jobs)
          /             Filter the current listing (live)
          s / S         Sort: cycle name/size/date / reverse direction
          r / F5        Refresh the current listing
          Ctrl+F        Recursive search (checkbox: all buckets)
          Ctrl+K        Palette: abort uploads, bucket config, log…
          Space			Select object for download
          Ctrl+S        Select all objects for download
          Ctrl+X        Unselect all objects for download
		  Del           Delete marked/highlighted (recursive for dirs)

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

			A tiny TUI browser for S3-compatible object storage.
			Github: https://github.com/nexusriot/s3duck-tui

			(C)2023-2026 Vladislav Ananev

                    _  [dim](quack)[-]
				 __( )>
				 \__\      [::b]Features[::-]
							• Profiles, incl. ~/.aws import
							• Walking dirs, filter, search
							• Download / upload files & dirs
							• Copy / move / rename / delete
							• Directory sync (dry run first)
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

	return &SummaryGraph{Root: grid, Cats: catT, Groups: grpT}
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
