package controller

import (
	"fmt"
	"sort"
	"strings"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/pkg/model"
	"github.com/nexusriot/s3duck-tui/pkg/view"
)

// usageNode is one prefix in the tree. Children are keyed by their own name
// segment ("photos/" or "report.pdf").
type usageNode struct {
	name     string
	fullPath string
	isDir    bool
	bytes    int64
	objects  int
	classes  map[string]int64
	children map[string]*usageNode
}

func newUsageNode(name, fullPath string, isDir bool) *usageNode {
	return &usageNode{
		name:     name,
		fullPath: fullPath,
		isDir:    isDir,
		classes:  map[string]int64{},
		children: map[string]*usageNode{},
	}
}

// buildUsageTree folds a recursive listing into a prefix tree rooted at
// prefix. Sizes and counts accumulate up the tree, so every node reports its
// whole subtree — the number a "where did the space go" question wants.
//
// Pure: the whole point of the split is that the arithmetic here is testable
// without a bucket.
func buildUsageTree(objs []s3t.Object, prefix string) *usageNode {
	root := newUsageNode(prefix, prefix, true)
	addUsageObjects(root, objs, prefix)
	return root
}

// addUsageObjects folds one page of a listing into an existing tree, so the
// scan can accumulate the tree instead of every object it was built from — a
// prefix with millions of objects is a tree of a few thousand nodes and a
// slice the size of the whole bucket.
func addUsageObjects(root *usageNode, objs []s3t.Object, prefix string) {
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		key := *o.Key
		if key == prefix || strings.HasSuffix(key, "/") {
			continue // folder markers are an encoding, not content
		}
		rel := strings.TrimPrefix(key, prefix)
		if rel == "" {
			continue
		}
		size := o.Size
		if size < 0 {
			size = 0
		}
		class := string(o.StorageClass)
		if class == "" {
			class = "STANDARD"
		}

		node := root
		node.bytes += size
		node.objects++
		node.classes[class] += size

		segments := strings.Split(rel, "/")
		path := prefix
		for i, seg := range segments {
			if seg == "" {
				continue
			}
			isDir := i < len(segments)-1
			path += seg
			if isDir {
				path += "/"
			}
			child, ok := node.children[seg]
			if !ok {
				child = newUsageNode(seg, path, isDir)
				node.children[seg] = child
			}
			// S3 lets "foo" and "foo/bar" both exist, in either order. The
			// node has to end up a directory whichever arrived first, or the
			// browser refuses to open it and everything under it is invisible.
			if isDir && !child.isDir {
				child.isDir = true
				child.fullPath = path
			}
			child.bytes += size
			child.objects++
			child.classes[class] += size
			node = child
		}
	}
}

// sortedChildren returns a node's children largest first, with directories and
// files intermixed — unlike the browser listing, where folders come first,
// because here size *is* the ordering the user came for.
func (n *usageNode) sortedChildren() []*usageNode {
	out := make([]*usageNode, 0, len(n.children))
	for _, c := range n.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].bytes != out[j].bytes {
			return out[i].bytes > out[j].bytes
		}
		return out[i].name < out[j].name
	})
	return out
}

// usageBar renders a proportional bar. Fixed width, so rows line up and the
// eye can compare them without reading the numbers.
func usageBar(part, whole int64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := 0
	if whole > 0 && part > 0 {
		filled = int(float64(part) / float64(whole) * float64(width))
		if filled == 0 {
			filled = 1 // never render a non-zero share as empty
		}
		if filled > width {
			filled = width
		}
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat(" ", width-filled) + "]"
}

// usageRow formats one child line: share bar, size, object count and name.
func usageRow(n *usageNode, parentBytes int64) (primary, secondary string) {
	pct := 0.0
	if parentBytes > 0 {
		pct = float64(n.bytes) / float64(parentBytes) * 100
	}
	icon := "📄"
	name := n.name
	if n.isDir {
		icon = "📁"
		name += "/"
	}
	primary = fmt.Sprintf("%s %9s %5.1f%%  %s  %s %s",
		usageBar(n.bytes, parentBytes, 16),
		view.HumanizeBytes(n.bytes),
		pct,
		countCell(n.objects),
		icon,
		name)
	return primary, n.fullPath
}

// countCell right-pads an object count so the names after it line up.
func countCell(n int) string {
	s := fmt.Sprintf("%d obj", n)
	for len(s) < 10 {
		s += " "
	}
	return s
}

// classBreakdown renders a node's storage-class split, largest first. This is
// the closest thing to a cost view the app can offer: it is what says "3.9 TB
// of this prefix is still STANDARD".
func classBreakdown(n *usageNode) string {
	type row struct {
		class string
		bytes int64
	}
	rows := make([]row, 0, len(n.classes))
	for c, b := range n.classes {
		rows = append(rows, row{c, b})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].bytes != rows[j].bytes {
			return rows[i].bytes > rows[j].bytes
		}
		return rows[i].class < rows[j].class
	})
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s %s", r.class, view.HumanizeBytes(r.bytes)))
	}
	return strings.Join(parts, " · ")
}

// usageTitle is the browser's title for one node.
func usageTitle(bucket string, n *usageNode) string {
	scope := bucket
	if n.fullPath != "" {
		scope = bucket + "/" + strings.TrimSuffix(n.fullPath, "/")
	}
	return fmt.Sprintf(" Usage: %s — %s in %d object(s) ", scope, view.HumanizeBytes(n.bytes), n.objects)
}

const usagePage = "modal-usage"

// ShowUsage scans the current prefix recursively and opens the usage browser.
func (c *Controller) ShowUsage() {
	if c.remoteOnly("The usage browser") {
		return
	}
	if c.currentBucket == nil {
		go c.error("Usage", fmt.Errorf("open a bucket first"))
		return
	}
	bucket := c.currentBucket
	prefix := model.NormalizePrefix(c.currentPath)
	mdl := c.model

	scanning, ctx, cancel := c.cancellableWait("progress", "Scanning usage...")
	go func() {
		defer cancel()
		progress := c.searchProgress(scanning, "usage")
		// Fold each page into the tree as it arrives: the tree is all the UI
		// ever reads, and holding the whole listing as well doubled the cost
		// of the one screen that exists for buckets too big to eyeball.
		root := newUsageNode(prefix, prefix, true)
		err := mdl.ListObjectsStream(ctx, prefix, bucket, func(page []s3t.Object, total int) bool {
			addUsageObjects(root, page, prefix)
			progress(total, 0)
			return true
		})
		if err != nil {
			c.view.App.QueueUpdateDraw(func() { c.view.Pages.RemovePage("progress") })
			if ctx.Err() == nil {
				c.error("Usage scan failed", err)
			}
			return
		}
		c.view.App.QueueUpdateDraw(func() {
			c.view.Pages.RemovePage("progress")
			if root.objects == 0 {
				go c.success(fmt.Sprintf("No objects under %s/%s", *bucket.Key, prefix))
				return
			}
			c.presentUsage(bucket, root, []*usageNode{root})
		})
	}()
}

// presentUsage shows one level of the tree. stack is the path from the root to
// the node being shown, which is what makes Backspace/Left go back up without
// re-scanning anything.
func (c *Controller) presentUsage(bucket *model.Object, root *usageNode, stack []*usageNode) {
	node := stack[len(stack)-1]
	children := node.sortedChildren()

	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(usageTitle(*bucket.Key, node))
	list.SetSelectedBackgroundColor(tcell.ColorBlue)
	list.SetSelectedTextColor(tcell.ColorWhite)

	if len(stack) > 1 {
		list.AddItem("[..]", "", 0, nil)
	}
	for _, ch := range children {
		primary, secondary := usageRow(ch, node.bytes)
		list.AddItem(primary, secondary, 0, nil)
	}

	footer := tview.NewTextView().SetDynamicColors(true)
	footer.SetText(fmt.Sprintf(
		"[gray]%s\n[white]Enter[gray] open · [white]g[gray] go to in browser · [white]Bksp/←[gray] up · [white]Esc[gray] close",
		classBreakdown(node)))

	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true).
		AddItem(footer, 2, 0, false)

	offset := 0
	if len(stack) > 1 {
		offset = 1
	}
	selected := func() *usageNode {
		i := list.GetCurrentItem() - offset
		if i < 0 || i >= len(children) {
			return nil
		}
		return children[i]
	}
	up := func() {
		if len(stack) > 1 {
			c.presentUsage(bucket, root, stack[:len(stack)-1])
		}
	}
	open := func() {
		if list.GetCurrentItem() == 0 && offset == 1 {
			up()
			return
		}
		n := selected()
		if n == nil || !n.isDir {
			return
		}
		c.presentUsage(bucket, root, append(stack, n))
	}

	list.SetSelectedFunc(func(int, string, string, rune) { open() })
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEsc:
			c.view.Pages.RemovePage(usagePage)
			c.view.App.SetFocus(c.view.List)
			return nil
		case tcell.KeyBackspace2, tcell.KeyBackspace, tcell.KeyLeft:
			up()
			return nil
		case tcell.KeyRight:
			open()
			return nil
		case tcell.KeyRune:
			if ev.Rune() == 'g' {
				// Hand the location to the ordinary browser: the usage view
				// answers "where are the bytes", and every action that
				// follows from that answer already exists one screen over.
				n := selected()
				if n == nil {
					return nil
				}
				c.view.Pages.RemovePage(usagePage)
				c.view.App.SetFocus(c.view.List)
				if n.isDir {
					c.jumpTo(*bucket.Key, n.fullPath, "")
				} else {
					c.revealKey(n.fullPath)
				}
				return nil
			}
		}
		return ev
	})

	c.view.Pages.AddPage(usagePage, c.view.ModalClamped(layout, 110, 30), true, true)
	c.view.App.SetFocus(list)
}
