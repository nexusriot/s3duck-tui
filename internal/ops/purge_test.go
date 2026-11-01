package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

type fakeVersionDeleter struct {
	calls   [][]model.VersionRef
	refuse  map[string]bool // version IDs reported as failed
	failAll error           // a whole-request failure
	onCall  func()
}

func (f *fakeVersionDeleter) DeleteVersions(_ context.Context, _ *model.Object, refs []model.VersionRef) (int, []model.VersionFailure, error) {
	f.calls = append(f.calls, refs)
	if f.onCall != nil {
		f.onCall()
	}
	if f.failAll != nil {
		return 0, nil, f.failAll
	}
	var failures []model.VersionFailure
	for _, r := range refs {
		if f.refuse[r.VersionID] {
			failures = append(failures, model.VersionFailure{Ref: r, Err: errors.New("AccessDenied")})
		}
	}
	return len(refs) - len(failures), failures, nil
}

func versions(n int) []model.VersionRef {
	out := make([]model.VersionRef, n)
	for i := range out {
		out[i] = model.VersionRef{Key: fmt.Sprintf("k%d", i), VersionID: fmt.Sprintf("v%d", i), Size: 2}
	}
	return out
}

func TestPurgeDeletesInChunksAndRetriesOnlyTheRefused(t *testing.T) {
	refs := versions(600)
	refs[3].IsDeleteMark = true
	d := &fakeVersionDeleter{refuse: map[string]bool{"v10": true, "v400": true}}
	p := &Purge{Bucket: model.BucketObject("b"), Refs: refs, What: "logs/", Deleter: d}

	res := p.Run(context.Background(), &recorder{})
	if len(d.calls) != 3 || len(d.calls[0]) != purgeChunk {
		t.Errorf("%d requests, first of %d; want chunks of %d", len(d.calls), len(d.calls[0]), purgeChunk)
	}
	if res.Done != 598 || res.Failed() != 2 {
		t.Errorf("done %d, failed %d; want 598 and 2", res.Done, res.Failed())
	}
	retry, ok := res.Retry.(*Purge)
	if !ok || len(retry.Refs) != 2 || retry.Refs[0].VersionID != "v10" || retry.Refs[1].VersionID != "v400" {
		t.Fatalf("retry = %+v, want exactly the two refused versions", res.Retry)
	}
	if !strings.Contains(res.Failures[0].Label, "k10 (version v10)") {
		t.Errorf("failure label = %q", res.Failures[0].Label)
	}
}

// A request that fails as a whole leaves every version in it retryable; a
// cancel stops before the next request and is not a failure.
func TestPurgeRequestFailureAndCancel(t *testing.T) {
	d := &fakeVersionDeleter{failAll: errors.New("connection reset")}
	res := (&Purge{Bucket: model.BucketObject("b"), Refs: versions(10), Deleter: d}).Run(context.Background(), &recorder{})
	if res.Failed() != 10 || res.Retry == nil {
		t.Errorf("failed %d, retry %v; want all 10 retryable", res.Failed(), res.Retry)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d2 := &fakeVersionDeleter{onCall: cancel}
	res = (&Purge{Bucket: model.BucketObject("b"), Refs: versions(600), Deleter: d2}).Run(ctx, &recorder{})
	if !res.Canceled || len(d2.calls) != 1 || res.Failed() != 0 {
		t.Errorf("canceled=%v requests=%d failed=%d; want one request, then a clean stop", res.Canceled, len(d2.calls), res.Failed())
	}
}

// The confirmation is the only place the user sees what a purge removes, and
// it is the one delete nothing can reverse: it says what, how much, and that.
func TestPurgeConfirmText(t *testing.T) {
	refs := []model.VersionRef{
		{Key: "a", VersionID: "1", Size: 1 << 20},
		{Key: "a", VersionID: "2", IsDeleteMark: true},
		{Key: "b", VersionID: "3", Size: 1 << 20},
	}
	p := &Purge{Bucket: model.BucketObject("prod"), Refs: refs, What: "logs/", Mode: model.PurgeAll}
	d := p.Describe()
	for _, want := range []string{"EVERY version of logs/ in prod", "2 key(s): 2 version(s), 2.0 MiB, and 1 delete marker(s)", "permanent"} {
		if !strings.Contains(d.ConfirmText, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, d.ConfirmText)
		}
	}
	p.Mode = model.PurgeNoncurrent
	if !strings.Contains(p.Describe().ConfirmText, "OLD versions") {
		t.Errorf("noncurrent wording:\n%s", p.Describe().ConfirmText)
	}
	if d.Kind != KindPurge || d.TotalItems != 3 || !d.RefreshAfter {
		t.Errorf("descriptor = %+v", d)
	}
}

func TestBatchCountsChangedSkippedFailedAndRetries(t *testing.T) {
	var seen []string
	b := &Batch{
		Verb: "tags", Confirm: "sure?", Keys: []string{"a", "b", "c", "d"},
		Apply: func(_ context.Context, k string) error {
			seen = append(seen, k)
			switch k {
			case "b":
				return ErrUnchanged
			case "c":
				return errors.New("AccessDenied")
			}
			return nil
		},
	}
	res := b.Run(context.Background(), &recorder{})
	if res.Done != 2 || res.Skipped != 1 || res.Failed() != 1 {
		t.Errorf("done=%d skipped=%d failed=%d; want 2, 1, 1", res.Done, res.Skipped, res.Failed())
	}
	retry, ok := res.Retry.(*Batch)
	if !ok || len(retry.Keys) != 1 || retry.Keys[0] != "c" || retry.Confirm != "" {
		t.Errorf("retry = %+v; want key c, asked nothing again", res.Retry)
	}
	if d := b.Describe(); d.ConfirmText != "sure?" || d.TotalItems != 4 || d.Kind != KindBatch {
		t.Errorf("descriptor = %+v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	res = (&Batch{Keys: []string{"a", "b", "c"}, Apply: func(context.Context, string) error {
		n++
		cancel()
		return nil
	}}).Run(ctx, &recorder{})
	if !res.Canceled || n != 1 || res.Done != 1 {
		t.Errorf("after cancel: applied %d, done %d, canceled %v", n, res.Done, res.Canceled)
	}
}
