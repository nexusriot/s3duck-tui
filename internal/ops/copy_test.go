package ops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// fakeMover stands in for the server-side copy/move.
type fakeMover struct {
	mu sync.Mutex
	// objectsFor is how many objects each source key expands to.
	objectsFor map[string]int
	failFor    map[string]error
	calls      []moverCall
	// emit drives the per-object progress callback.
	emit bool
}

type moverCall struct {
	move           bool
	srcKey, dstKey string
	isFolder       bool
	skip           map[string]bool
}

func (m *fakeMover) run(move bool, ctx context.Context, srcKey, dstKey string, isFolder bool,
	skip map[string]bool, progress func(done, total int, key string)) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	m.mu.Lock()
	m.calls = append(m.calls, moverCall{move, srcKey, dstKey, isFolder, skip})
	err := m.failFor[srcKey]
	n, ok := m.objectsFor[srcKey]
	if !ok {
		n = 1
	}
	m.mu.Unlock()

	if err != nil {
		return 0, err
	}
	if m.emit && progress != nil {
		for i := 1; i <= n; i++ {
			progress(i, n, srcKey)
		}
	}
	return n, nil
}

func (m *fakeMover) CopyKeys(ctx context.Context, _, _ *model.Object, srcKey, dstKey string,
	isFolder bool, skip map[string]bool, progress func(int, int, string)) (int, error) {
	return m.run(false, ctx, srcKey, dstKey, isFolder, skip, progress)
}

func (m *fakeMover) MoveKeys(ctx context.Context, _, _ *model.Object, srcKey, dstKey string,
	isFolder bool, skip map[string]bool, progress func(int, int, string)) (int, error) {
	return m.run(true, ctx, srcKey, dstKey, isFolder, skip, progress)
}

func (m *fakeMover) dstKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.calls))
	for _, c := range m.calls {
		out = append(out, c.dstKey)
	}
	return out
}

func newCopy(mv *fakeMover, items ...CopyItem) *Copy {
	return &Copy{
		Title:         "Copy",
		Items:         items,
		SrcBucket:     bucketObj("src"),
		DstBucket:     bucketObj("dst"),
		DstBucketName: "dst",
		DstPrefix:     "to/",
		Mover:         mv,
	}
}

func file(name string) CopyItem { return CopyItem{ShortName: name, SrcKey: "from/" + name} }
func folder(name string) CopyItem {
	return CopyItem{ShortName: name, SrcKey: "from/" + name + "/", IsFolder: true}
}

func TestCopyHappyPath(t *testing.T) {
	mv := &fakeMover{}
	c := newCopy(mv, file("a.txt"), file("b.txt"))

	res := c.Run(context.Background(), &recorder{})

	if res.Done != 2 || res.Failed() != 0 || res.Canceled {
		t.Fatalf("result = %+v", res)
	}
	if res.Objects != 2 {
		t.Errorf("objects = %d, want 2", res.Objects)
	}
	if res.Retry != nil {
		t.Error("a clean run should offer no retry")
	}
}

// A folder's destination key must end in "/" or the server writes the whole
// tree under a key that is a file name.
func TestCopyAppendsSlashForFolders(t *testing.T) {
	mv := &fakeMover{}
	c := newCopy(mv, file("a.txt"), folder("docs"))

	c.Run(context.Background(), &recorder{})

	got := mv.dstKeys()
	want := []string{"to/a.txt", "to/docs/"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("destination keys = %v, want %v", got, want)
	}
}

// Items are the user's selection; objects are what the server wrote. A folder
// item is many objects, so reporting items alone would hide the real scale.
func TestCopyCountsObjectsSeparatelyFromItems(t *testing.T) {
	mv := &fakeMover{objectsFor: map[string]int{"from/docs/": 412}}
	c := newCopy(mv, folder("docs"), file("a.txt"))

	res := c.Run(context.Background(), &recorder{})

	if res.Done != 2 {
		t.Errorf("done = %d items, want 2", res.Done)
	}
	if res.Objects != 413 {
		t.Errorf("objects = %d, want 413", res.Objects)
	}
}

func TestCopyUsesMoveKeysWhenMoving(t *testing.T) {
	mv := &fakeMover{}
	c := newCopy(mv, file("a.txt"))
	c.Move = true

	c.Run(context.Background(), &recorder{})

	mv.mu.Lock()
	defer mv.mu.Unlock()
	if len(mv.calls) != 1 || !mv.calls[0].move {
		t.Errorf("calls = %+v, want one move", mv.calls)
	}
}

func TestCopyDescribeKind(t *testing.T) {
	if got := newCopy(&fakeMover{}, file("a")).Describe().Kind; got != KindCopy {
		t.Errorf("kind = %q, want copy", got)
	}
	m := newCopy(&fakeMover{}, file("a"))
	m.Move = true
	if got := m.Describe().Kind; got != KindMove {
		t.Errorf("kind = %q, want move", got)
	}
	// Both change the bucket, so the browser has to re-list afterwards.
	if !m.Describe().RefreshAfter {
		t.Error("a move should ask for a refresh")
	}
}

// The undo ledger is the caller's business, but it can only be built from what
// actually moved — a failed item must not appear in it.
func TestCopyReportsOnlyWhatMoved(t *testing.T) {
	mv := &fakeMover{failFor: map[string]error{"from/b.txt": errors.New("boom")}}
	c := newCopy(mv, file("a.txt"), file("b.txt"), folder("docs"))
	c.Move = true

	var moved []Transfer
	c.OnMoved = func(m []Transfer) { moved = m }

	res := c.Run(context.Background(), &recorder{})

	if res.Failed() != 1 {
		t.Fatalf("failures = %v, want 1", res.Failures)
	}
	if len(moved) != 2 {
		t.Fatalf("recorded %d moves, want 2", len(moved))
	}
	for _, m := range moved {
		if strings.Contains(m.SrcKey, "b.txt") {
			t.Errorf("a failed item was recorded as moved: %+v", m)
		}
	}
	if moved[1].DstKey != "to/docs/" || !moved[1].IsFolder {
		t.Errorf("folder move recorded as %+v", moved[1])
	}
}

func TestCopyDoesNotRecordMovesForACopy(t *testing.T) {
	called := false
	c := newCopy(&fakeMover{}, file("a.txt"))
	c.OnMoved = func([]Transfer) { called = true }

	c.Run(context.Background(), &recorder{})

	if called {
		t.Error("a copy recorded an undo entry; nothing was moved")
	}
}

// Nothing moved means nothing to undo: a fully failed move must not leave an
// undo entry pointing at a destination that was never written.
func TestCopyNoUndoWhenEverythingFailed(t *testing.T) {
	mv := &fakeMover{failFor: map[string]error{"from/a.txt": errors.New("boom")}}
	c := newCopy(mv, file("a.txt"))
	c.Move = true
	called := false
	c.OnMoved = func([]Transfer) { called = true }

	res := c.Run(context.Background(), &recorder{})

	if called {
		t.Error("an undo entry was recorded for a move that wrote nothing")
	}
	if res.Done != 0 || res.Failed() != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestCopyCancelStopsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mv := &fakeMover{}
	c := newCopy(mv, file("a"), file("b"), file("c"))

	res := c.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	mv.mu.Lock()
	defer mv.mu.Unlock()
	if len(mv.calls) != 0 {
		t.Errorf("made %d calls with a dead context", len(mv.calls))
	}
}

// The skip set is decided by the overwrite scan before the run; the server is
// never asked about those keys, so the report states the scan's decision.
func TestCopyReportsTheSkipSet(t *testing.T) {
	mv := &fakeMover{}
	c := newCopy(mv, file("a.txt"))
	c.Skip = map[string]bool{"to/x": true, "to/y": true}

	res := c.Run(context.Background(), &recorder{})

	if res.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", res.Skipped)
	}
	mv.mu.Lock()
	defer mv.mu.Unlock()
	if len(mv.calls[0].skip) != 2 {
		t.Errorf("the skip set did not reach the mover: %v", mv.calls[0].skip)
	}
}

func TestCopyRetryCoversOnlyTheFailures(t *testing.T) {
	mv := &fakeMover{failFor: map[string]error{"from/b.txt": errors.New("boom")}}
	c := newCopy(mv, file("a.txt"), file("b.txt"), file("c.txt"))

	res := c.Run(context.Background(), &recorder{})

	rc, ok := res.Retry.(*Copy)
	if !ok {
		t.Fatalf("retry is %T, want *Copy", res.Retry)
	}
	if len(rc.Items) != 1 || rc.Items[0].ShortName != "b.txt" {
		t.Errorf("retry items = %v, want just b.txt", rc.Items)
	}
	if rc.DstPrefix != c.DstPrefix || rc.Move != c.Move {
		t.Error("the retry changed where the items go or how")
	}
}

// Progress for a server-side operation is measured in objects: no bytes pass
// through this process, so a byte rate would be a fiction.
func TestCopyReportsObjectProgress(t *testing.T) {
	mv := &fakeMover{emit: true, objectsFor: map[string]int{"from/docs/": 3}}
	c := newCopy(mv, folder("docs"))
	rep := &recorder{}

	c.Run(context.Background(), rep)

	last, ok := rep.lastProgress()
	if !ok {
		t.Fatal("no progress reported")
	}
	if !last.ByObject {
		t.Error("progress should be flagged as object-counted")
	}
	if last.Items != 3 || last.TotalItems != 3 {
		t.Errorf("progress = %d/%d, want 3/3", last.Items, last.TotalItems)
	}
	if last.Bytes != 0 || last.TotalBytes != 0 {
		t.Errorf("a server-side copy claimed byte progress: %+v", last)
	}
}
