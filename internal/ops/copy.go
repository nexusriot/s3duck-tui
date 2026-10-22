package ops

import (
	"context"
	"fmt"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// Mover is the server-side copy/move port. Both calls return the number of
// objects actually written, which is not the number of items: one folder item
// is many objects, and a destination the user chose to skip is none. It is
// model.Model.
type Mover interface {
	CopyKeys(ctx context.Context, srcBucket, dstBucket *model.Object, srcKey, dstKey string,
		isFolder bool, skip map[string]bool, progress func(done, total int, key string)) (int, error)
	MoveKeys(ctx context.Context, srcBucket, dstBucket *model.Object, srcKey, dstKey string,
		isFolder bool, skip map[string]bool, progress func(done, total int, key string)) (int, error)
}

// CopyItem is one thing the user picked: a file or a whole folder.
type CopyItem struct {
	// ShortName is the display name and the destination's last segment.
	ShortName string
	SrcKey    string
	IsFolder  bool
}

// Transfer records one completed move, source and destination, so it can be
// inverted into an undo.
type Transfer struct {
	SrcBucket, DstBucket *model.Object
	SrcKey, DstKey       string
	IsFolder             bool
	// Skip is the run's skip set: destination keys the overwrite scan left
	// alone. For a folder, the ones under DstKey were NOT moved — their
	// sources are still in place and their destinations hold what was there
	// before — so an undo must leave both alone rather than move the whole
	// destination folder back over the sources the user chose to keep.
	Skip map[string]bool
}

// Copy copies — or, with Move set, moves — items between prefixes or buckets
// on the same endpoint. The bytes never pass through this process: the server
// does the work, and all this reports is how many objects it has acknowledged.
// That is why Copy takes no Gate; the transfer slots exist to bound local
// bandwidth, and this uses none.
type Copy struct {
	Move bool
	// Title is the verb for the job label and the report ("Copy", "Move",
	// "Paste"). The caller picks it because the same operation is reached
	// three ways.
	Title string

	Items                    []CopyItem
	SrcBucket, DstBucket     *model.Object
	DstBucketName, DstPrefix string
	// Skip holds destination keys the overwrite scan decided to leave alone.
	Skip map[string]bool

	Mover Mover

	// OnMoved is called once after the run with the pairs that actually moved,
	// so the caller can record a one-step undo. Never called for a copy, and
	// never with an empty slice.
	OnMoved func([]Transfer)
}

func (c *Copy) Describe() Descriptor {
	kind := KindCopy
	if c.Move {
		kind = KindMove
	}
	return Descriptor{
		Kind:         kind,
		Title:        fmt.Sprintf("%d item(s) → %s/%s", len(c.Items), c.DstBucketName, c.DstPrefix),
		TotalItems:   len(c.Items),
		RefreshAfter: true,
	}
}

func (c *Copy) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}
	var moved []Transfer
	// failed keeps the items themselves: their labels are display names, and
	// a file "docs" and a folder "docs/" share one.
	var failed []CopyItem

	for _, it := range c.Items {
		if ctx.Err() != nil {
			led.cancel()
			break
		}

		dstKey := c.DstPrefix + it.ShortName
		if it.IsFolder {
			dstKey += "/"
		}
		if !it.IsFolder && c.Skip[dstKey] {
			// The user kept the existing destination, so this item is not
			// written and — on a move — its source stays put. It is counted
			// once, in Skipped below; counting it as done as well, or
			// recording it as moved, would make an undo "move it back": copy
			// the kept destination over the untouched source, then delete it.
			continue
		}

		n, err := c.apply(ctx, rep, it, dstKey)
		if err != nil {
			if ctx.Err() != nil {
				// A cancel, not a failure: reporting it as one would offer a
				// "Retry failed" covering only the item that was in flight and
				// silently dropping the ones never dispatched.
				led.cancel()
				break
			}
			// The item itself is kept on the failure, so a retry re-runs
			// exactly what failed rather than the whole selection.
			led.fail(it.ShortName, err)
			failed = append(failed, it)
			continue
		}

		led.ok(0, false)
		led.addObjects(n)
		if c.Move && n > 0 {
			moved = append(moved, Transfer{
				SrcBucket: c.SrcBucket, DstBucket: c.DstBucket,
				SrcKey: it.SrcKey, DstKey: dstKey, IsFolder: it.IsFolder,
				Skip: c.Skip,
			})
		}
	}

	if c.Move && len(moved) > 0 && c.OnMoved != nil {
		c.OnMoved(moved)
	}

	res := led.result()
	// Skipped reports the destinations the overwrite scan excluded up front.
	// The server is never asked about them, so there is nothing to count
	// during the run.
	res.Skipped = len(c.Skip)
	rep.Log("%s: %d item(s), %d object(s) written, %d failed → %s/%s",
		c.Title, res.Done, res.Objects, res.Failed(), c.DstBucketName, c.DstPrefix)

	return c.withRetry(res, failed)
}

// apply runs one item through the mover, reporting the server's per-object
// acknowledgements as they arrive.
func (c *Copy) apply(ctx context.Context, rep Reporter, it CopyItem, dstKey string) (int, error) {
	progress := func(done, total int, key string) {
		rep.Progress(Progress{
			Items:      done,
			TotalItems: total,
			ByObject:   true,
			Active:     []string{fmt.Sprintf("%s → %s", it.ShortName, c.DstBucketName), key},
		})
	}
	if c.Move {
		return c.Mover.MoveKeys(ctx, c.SrcBucket, c.DstBucket, it.SrcKey, dstKey, it.IsFolder, c.Skip, progress)
	}
	return c.Mover.CopyKeys(ctx, c.SrcBucket, c.DstBucket, it.SrcKey, dstKey, it.IsFolder, c.Skip, progress)
}

// withRetry stamps the result with an operation covering exactly the items that
// failed.
//
// OnMoved is cleared, because it is not only an undo ledger: the controller
// also uses it to consume the cut clipboard and to clear the marks in the
// scope the items were moved out of. Those are one-shot effects belonging to
// the run that moved them. A retry inheriting the callback applies them again,
// to whatever state exists by then — wiping a clipboard the user filled after
// the first pass, and marks they re-made in the source folder.
//
// The cost is that a retry's own moves are not added to the one-step undo,
// which still reverses what the first pass moved. Accumulating across the two
// would need the undo to know the runs belong together; an unrelated move in
// between would otherwise be folded into the same entry.
func (c *Copy) withRetry(res Result, failed []CopyItem) Result {
	if len(failed) == 0 {
		return res
	}
	next := *c
	next.Items = failed
	next.OnMoved = nil
	res.Retry = &next
	return res
}
