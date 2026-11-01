package ops

import (
	"context"
	"errors"
	"fmt"
)

// KindBatch is a per-object change applied to a list of keys: a storage
// class, a restore request, a metadata or tag edit.
const KindBatch Kind = "batch"

// ErrUnchanged is what a Batch's Apply returns for a key it looked at and left
// alone (already in the asked-for state): counted as skipped, not done.
var ErrUnchanged = errors.New("unchanged")

// Batch applies one change to each key in turn. The keys are resolved before
// it starts — marked folders expanded to the objects in them — so the
// confirmation can say how many objects it touches, and a retry re-runs
// exactly the keys that failed.
type Batch struct {
	// Verb names the change for the job label and the log ("storage class →
	// GLACIER", "metadata").
	Verb string
	// Confirm is the pre-flight question; empty asks nothing.
	Confirm string
	Keys    []string
	// Apply changes one key. Returning ErrUnchanged counts it as skipped.
	Apply func(ctx context.Context, key string) error
}

func (b *Batch) Describe() Descriptor {
	return Descriptor{
		Kind:         KindBatch,
		Title:        fmt.Sprintf("%s: %d object(s)", b.Verb, len(b.Keys)),
		ConfirmText:  b.Confirm,
		TotalItems:   len(b.Keys),
		RefreshAfter: true,
	}
}

func (b *Batch) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}
	var failed []string
	for i, key := range b.Keys {
		if ctx.Err() != nil {
			led.cancel()
			break
		}
		rep.Progress(Progress{Items: i, TotalItems: len(b.Keys), ByObject: true, Active: []string{key}})
		err := b.Apply(ctx, key)
		switch {
		case err == nil:
			led.ok(0, false)
		case errors.Is(err, ErrUnchanged):
			led.skip()
		case ctx.Err() != nil:
			led.cancel()
		default:
			led.fail(key, err)
			failed = append(failed, key)
		}
		if ctx.Err() != nil {
			led.cancel()
			break
		}
	}
	res := led.result()
	rep.Progress(Progress{Items: res.Done + res.Skipped + res.Failed(), TotalItems: len(b.Keys), ByObject: true})
	rep.Log("%s: %d changed, %d already so, %d failed", b.Verb, res.Done, res.Skipped, res.Failed())
	if len(failed) > 0 {
		next := *b
		next.Keys = failed
		next.Confirm = "" // the user already confirmed this change
		res.Retry = &next
	}
	return res
}
