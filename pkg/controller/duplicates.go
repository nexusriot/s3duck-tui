package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

// dupMember is one copy of a duplicated object.
type dupMember struct {
	Key          string
	Size         int64
	LastModified *time.Time
}

// dupGroup is a set of objects that share size and ETag. ETag+size is the
// strongest content signal S3 offers without downloading: for single-part
// uploads the ETag is the body's MD5, so equal means equal content; multipart
// ETags depend on the part split, so identical files uploaded differently
// won't group (a miss, never a false positive for honest backends).
type dupGroup struct {
	ETag    string
	Size    int64
	Members []dupMember
}

// wasted is the storage the group's redundant copies occupy: everything
// beyond the one copy that must stay.
func (g dupGroup) wasted() int64 {
	if len(g.Members) < 2 {
		return 0
	}
	return int64(len(g.Members)-1) * g.Size
}

// findDuplicates groups a recursive listing by (size, ETag) and keeps only the
// groups with two or more members. Folder markers and objects without an ETag
// (some backends omit it) contribute nothing. Groups are ordered by wasted
// bytes descending — the group worth looking at first frees the most — with
// the ETag as a deterministic tiebreak. Members are ordered oldest first: the
// oldest copy is the likeliest original, so the ones after it are the natural
// deletion candidates.
func findDuplicates(objs []s3t.Object) []dupGroup {
	type sig struct {
		etag string
		size int64
	}
	bySig := map[sig][]dupMember{}

	for _, o := range objs {
		if o.Key == nil || strings.HasSuffix(*o.Key, "/") {
			continue
		}
		etag := strings.Trim(strings.TrimSpace(deref(o.ETag)), `"`)
		if etag == "" {
			continue
		}
		k := sig{etag: etag, size: o.Size}
		bySig[k] = append(bySig[k], dupMember{
			Key:          *o.Key,
			Size:         o.Size,
			LastModified: o.LastModified,
		})
	}

	var groups []dupGroup
	for k, members := range bySig {
		if len(members) < 2 {
			continue
		}
		sort.SliceStable(members, func(i, j int) bool {
			a, b := members[i], members[j]
			switch {
			case a.LastModified == nil && b.LastModified == nil:
				return a.Key < b.Key
			case a.LastModified == nil:
				return false
			case b.LastModified == nil:
				return true
			case a.LastModified.Equal(*b.LastModified):
				return a.Key < b.Key
			default:
				return a.LastModified.Before(*b.LastModified)
			}
		})
		groups = append(groups, dupGroup{ETag: k.etag, Size: k.size, Members: members})
	}

	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].wasted() != groups[j].wasted() {
			return groups[i].wasted() > groups[j].wasted()
		}
		return groups[i].ETag < groups[j].ETag
	})
	return groups
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// dupTotalWasted sums the reclaimable bytes across all groups.
func dupTotalWasted(groups []dupGroup) int64 {
	var total int64
	for _, g := range groups {
		total += g.wasted()
	}
	return total
}

// dupSummary is the headline over the group list.
func dupSummary(groups []dupGroup) string {
	copies := 0
	for _, g := range groups {
		copies += len(g.Members) - 1
	}
	return fmt.Sprintf("%d group(s), %d redundant cop%s, %s reclaimable",
		len(groups), copies, pluralYIes(copies), humanize.IBytes(uint64(dupTotalWasted(groups))))
}

func pluralYIes(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// dupGroupRow renders one group for the list: count × size, wasted bytes, and
// the first member's key as the group's face.
func dupGroupRow(g dupGroup) (primary, secondary string) {
	face := ""
	if len(g.Members) > 0 {
		face = g.Members[0].Key
	}
	primary = fmt.Sprintf("%d × %-10s  [gray]wastes %s[-]  %s",
		len(g.Members), humanize.IBytes(uint64(g.Size)), humanize.IBytes(uint64(g.wasted())), face)
	return primary, g.ETag
}

// dupMemberRow renders one copy inside a group. The oldest member is marked as
// the likely original so the deletion candidates are visually distinct.
func dupMemberRow(m dupMember, oldest bool) (primary, secondary string) {
	when := "unknown date"
	if m.LastModified != nil {
		when = m.LastModified.In(time.Local).Format(colDateLayout)
	}
	tag := "  "
	if oldest {
		tag = "[green]* [-]"
	}
	return fmt.Sprintf("%s%s  %s", tag, when, m.Key), m.Key
}

// dropDupMember removes the member with the given key from groups[gi],
// dissolving the group when fewer than two copies remain. It returns the
// updated groups and whether groups[gi] still denotes the SAME group with two
// or more members — after a dissolve, gi indexes the NEXT group (or past the
// end), so the caller must return to the group list rather than silently
// present an unrelated group's members under the same delete binding.
func dropDupMember(groups []dupGroup, gi int, key string) ([]dupGroup, bool) {
	kept := groups[gi].Members[:0:0]
	for _, m := range groups[gi].Members {
		if m.Key != key {
			kept = append(kept, m)
		}
	}
	groups[gi].Members = kept
	if len(kept) < 2 {
		return append(groups[:gi:gi], groups[gi+1:]...), false
	}
	return groups, true
}

// FindDuplicates scans the current bucket (from the current prefix down) and
// opens a browser over the duplicate groups it finds. Runs on the UI goroutine
// (key handler); the listing happens behind a modal.
func (c *Controller) FindDuplicates() {
	if c.currentBucket == nil {
		go c.error("Duplicates", fmt.Errorf("open a bucket first"))
		return
	}
	bucket := c.currentBucket
	prefix := model.NormalizePrefix(c.currentPath)
	mdl := c.model

	scanning := tview.NewModal().SetText("Scanning for duplicates...")
	c.view.Pages.AddPage("progress", scanning, true, true)

	go func() {
		objs, err := mdl.ListObjects(prefix, bucket)
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			c.error("Duplicate scan failed", err)
			return
		}
		groups := findDuplicates(objs)

		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress")
			if len(groups) == 0 {
				go c.success(fmt.Sprintf("No duplicates under %s/%s", *bucket.Key, prefix))
				return
			}
			c.presentDupGroups(mdl, bucket, prefix, groups)
		})
	}()
}

const dupPage = "modal-dups"

// presentDupGroups shows the group list. Runs on the UI goroutine.
func (c *Controller) presentDupGroups(mdl *model.Model, bucket *model.Object, prefix string, groups []dupGroup) {
	c.view.Pages.RemovePage(dupPage)
	if len(groups) == 0 {
		go c.success("No duplicates remain")
		return
	}

	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" Duplicates under %s/%s — %s ", *bucket.Key, prefix, dupSummary(groups)))
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	for _, g := range groups {
		primary, secondary := dupGroupRow(g)
		list.AddItem(primary, secondary, 0, nil)
	}

	help := tview.NewTextView().SetDynamicColors(true).SetText(
		"  [::b]Enter[::-] open group   [::b]Esc[::-] close   [gray]grouped by size + ETag; multipart uploads match only when split identically[-]")

	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			c.view.Pages.RemovePage(dupPage)
			return nil
		case tcell.KeyEnter:
			if i := list.GetCurrentItem(); i >= 0 && i < len(groups) {
				c.presentDupMembers(mdl, bucket, prefix, groups, i)
			}
			return nil
		}
		return event
	})

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true).
		AddItem(help, 1, 0, false)

	c.view.Pages.AddPage(dupPage, c.view.ModalEdit(flex, 96, 24), true, true)
	c.view.App.SetFocus(list)
}

// presentDupMembers shows one group's copies. Deleting a member updates the
// group in place; when fewer than two copies remain the group dissolves and
// the view returns to the (rebuilt) group list. Runs on the UI goroutine.
func (c *Controller) presentDupMembers(mdl *model.Model, bucket *model.Object, prefix string, groups []dupGroup, gi int) {
	c.view.Pages.RemovePage(dupPage)
	g := groups[gi]

	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" %d × %s — ETag %s ",
		len(g.Members), humanize.IBytes(uint64(g.Size)), g.ETag))
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	for i, m := range g.Members {
		primary, secondary := dupMemberRow(m, i == 0)
		list.AddItem(primary, secondary, 0, nil)
	}

	help := tview.NewTextView().SetDynamicColors(true).SetText(
		"  [::b]Enter[::-] reveal in browser   [::b]d[::-] delete this copy   [::b]Esc[::-] back   [gray]* = oldest (likely original)[-]")

	selected := func() (dupMember, bool) {
		i := list.GetCurrentItem()
		if i < 0 || i >= len(g.Members) {
			return dupMember{}, false
		}
		return g.Members[i], true
	}

	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			c.presentDupGroups(mdl, bucket, prefix, groups)
			return nil
		case tcell.KeyEnter:
			if m, ok := selected(); ok {
				c.view.Pages.RemovePage(dupPage)
				c.revealKey(m.Key)
			}
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'd' {
				if m, ok := selected(); ok {
					c.confirmDeleteDup(mdl, bucket, prefix, groups, gi, m)
				}
				return nil
			}
		}
		return event
	})

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true).
		AddItem(help, 1, 0, false)

	c.view.Pages.AddPage(dupPage, c.view.ModalEdit(flex, 96, 20), true, true)
	c.view.App.SetFocus(list)
}

// confirmDeleteDup deletes one copy after confirmation and re-renders the
// member view (or the group list, if the group dissolved).
func (c *Controller) confirmDeleteDup(mdl *model.Model, bucket *model.Object, prefix string, groups []dupGroup, gi int, m dupMember) {
	others := len(groups[gi].Members) - 1
	verb := "remain"
	if others == 1 {
		verb = "remains"
	}
	confirm := c.view.NewConfirm()
	confirm.SetText(fmt.Sprintf("Delete this copy?\n\n%s\n\n%d other cop%s of the same content %s.",
		m.Key, others, pluralYIes(others), verb)).
		SetDoneFunc(func(_ int, label string) {
			c.view.Pages.RemovePage("confirm")
			if label != "OK" {
				return
			}
			go func() {
				if err := mdl.DeleteKey(context.Background(), m.Key, bucket); err != nil {
					c.error("Failed to delete duplicate", err)
					return
				}
				c.logActivity("Duplicate deleted: %s", m.Key)

				// The groups surgery runs ON the UI goroutine: the member
				// list stays live during the network round-trip, and its Esc
				// and delete handlers read the same slice — mutating it here
				// would race them.
				c.view.App.QueueUpdateDraw(func() {
					updated, sameGroup := dropDupMember(groups, gi, m.Key)
					if sameGroup {
						c.presentDupMembers(mdl, bucket, prefix, updated, gi)
					} else {
						c.presentDupGroups(mdl, bucket, prefix, updated)
					}
				})
				c.updateList()
			}()
		})
	c.view.Pages.AddPage("confirm", confirm, true, true)
}
