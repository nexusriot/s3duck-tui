package ops

import (
	"context"
	"testing"
)

// queueRecorder is a recorder that also wants to hear about slot waits.
type queueRecorder struct {
	recorder
	waits []bool
}

func (q *queueRecorder) Queued(waiting bool) { q.waits = append(q.waits, waiting) }

type freeGate struct{}

func (freeGate) Acquire(context.Context) bool { return true }
func (freeGate) Release()                     {}

// A job waiting behind the gate must be shown as queued, and as running again
// once it holds a slot; without the hook it read "running" all along.
func TestReserveReportsTheWait(t *testing.T) {
	rep := &queueRecorder{}
	ran := false
	if !Reserve(context.Background(), freeGate{}, rep, func() { ran = true }) || !ran {
		t.Fatal("Reserve did not run f with a free slot")
	}
	if len(rep.waits) != 2 || !rep.waits[0] || rep.waits[1] {
		t.Errorf("Queued calls = %v, want [true false]", rep.waits)
	}

	// A wait that ends in a cancel still ends the queued state.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep = &queueRecorder{}
	if Reserve(ctx, blockingGate{}, rep, func() { t.Error("f ran without a slot") }) {
		t.Error("Reserve reported a slot it never got")
	}
	if len(rep.waits) != 2 || rep.waits[1] {
		t.Errorf("Queued calls = %v, want the wait closed with false", rep.waits)
	}

	// A plain Reporter and a nil gate need nothing from the hook.
	if !Reserve(context.Background(), nil, &recorder{}, func() {}) {
		t.Error("a nil gate means no limit")
	}
}
