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

	for _, it := range c.Items {
		if ctx.Err() != nil {
			led.cancel()
			break
		}

		dstKey := c.DstPrefix + it.ShortName
		if it.IsFolder {
			dstKey += "/"
		}

		n, err := c.apply(ctx, rep, it, dstKey)
		if err != nil {
			// The item itself is kept on the failure, so a retry re-runs
			// exactly what failed rather than the whole selection.
			led.fail(it.ShortName, err)
			continue
		}

		led.ok(0, false)
		led.addObjects(n)
		if c.Move {
			moved = append(moved, Transfer{
				SrcBucket: c.SrcBucket, DstBucket: c.DstBucket,
				SrcKey: it.SrcKey, DstKey: dstKey, IsFolder: it.IsFolder,
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

	return c.withRetry(res)
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
// failed. The undo callback is dropped: a retry's moves are recorded by the
// retry itself, and replaying the original's would double-count.
func (c *Copy) withRetry(res Result) Result {
	if len(res.Failures) == 0 {
		return res
	}
	byName := make(map[string]CopyItem, len(c.Items))
	for _, it := range c.Items {
		byName[it.ShortName] = it
	}
	again := make([]CopyItem, 0, len(res.Failures))
	for _, f := range res.Failures {
		if it, ok := byName[f.Label]; ok {
			again = append(again, it)
		}
	}
	if len(again) == 0 {
		return res
	}
	next := *c
	next.Items = again
	res.Retry = &next
	return res
}
