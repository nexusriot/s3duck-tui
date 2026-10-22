package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

type fakePusher struct {
	err error
	// seen records the targets the operation handed down; nil until called.
	seen []model.UploadTarget
	// emit drives the progress callback, standing in for the model's walk.
	emit []progressCall
}

type progressCall struct {
	sent, total   int64
	i, count      int
	local, remote string
}

func (p *fakePusher) UploadTargets(
	ctx context.Context,
	_ *model.Object,
	targets []model.UploadTarget,
	progress func(current, total int64, done, count int, local, remote string),
) error {
	p.seen = append([]model.UploadTarget{}, targets...)
	for _, c := range p.emit {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress(c.sent, c.total, c.i, c.count, c.local, c.remote)
	}
	return p.err
}

func newUpload(p *fakePusher) *Upload {
	return &Upload{
		LocalPath:  "/home/u/photos",
		DstPrefix:  "trip/",
		Bucket:     bucketObj("b"),
		Files:      []model.UploadTarget{{LocalPath: "/home/u/photos/a.jpg", RemotePath: "trip/a.jpg", Size: 100}},
		TotalBytes: 100,
		Pusher:     p,
	}
}

func TestUploadDescribe(t *testing.T) {
	got := newUpload(&fakePusher{}).Describe()
	if got.Kind != KindUpload || got.TotalBytes != 100 || got.TotalItems != 1 {
		t.Errorf("descriptor = %+v", got)
	}
	// The title names the source by base name and the full destination.
	for _, want := range []string{"photos", "b/trip/"} {
		if !strings.Contains(got.Title, want) {
			t.Errorf("title %q is missing %q", got.Title, want)
		}
	}
}

func TestUploadHappyPath(t *testing.T) {
	p := &fakePusher{emit: []progressCall{
		{sent: 50, total: 100, i: 1, count: 1, local: "/home/u/photos/a.jpg", remote: "trip/a.jpg"},
		{sent: 100, total: 100, i: 1, count: 1, local: "/home/u/photos/a.jpg", remote: "trip/a.jpg"},
	}}
	u := newUpload(p)
	rep := &recorder{}

	res := u.Run(context.Background(), rep)

	if res.Done != 1 || res.Failed() != 0 || res.Canceled {
		t.Fatalf("result = %+v", res)
	}
	if res.Bytes != 100 {
		t.Errorf("bytes = %d, want 100", res.Bytes)
	}
	last, ok := rep.lastProgress()
	if !ok || last.Bytes != 100 || last.TotalBytes != 100 {
		t.Errorf("last progress = %+v, want 100/100", last)
	}
	if len(rep.logs) != 1 {
		t.Errorf("logged %d lines, want 1 completion line", len(rep.logs))
	}
}

func TestUploadReportsAFailure(t *testing.T) {
	u := newUpload(&fakePusher{err: errors.New("endpoint said no")})

	res := u.Run(context.Background(), &recorder{})

	if res.Done != 0 || res.Failed() != 1 {
		t.Fatalf("result = %+v, want one failure", res)
	}
	if !strings.Contains(res.Failures[0].Err.Error(), "endpoint said no") {
		t.Errorf("failure = %v", res.Failures[0])
	}
	if res.Failures[0].Label != u.LocalPath {
		t.Errorf("label = %q, want the local path", res.Failures[0].Label)
	}
}

// A cancelled upload is reported as cancelled, not as a failure: the user's
// own decision is not an error.
func TestUploadCancelIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u := newUpload(&fakePusher{err: context.Canceled})

	res := u.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("result should be marked cancelled")
	}
	if res.Failed() != 0 {
		t.Errorf("failures = %v, want none for a cancel", res.Failures)
	}
}

// The skip set is applied before anything is sent, so a kept destination is
// never written and the gauge never counts its bytes.
func TestUploadDropsSkippedTargets(t *testing.T) {
	p := &fakePusher{}
	u := newUpload(p)
	u.Files = append(u.Files, model.UploadTarget{LocalPath: "/home/u/photos/b.jpg", RemotePath: "trip/b.jpg", Size: 7})
	u.Skip = map[string]bool{"trip/a.jpg": true}

	res := u.Run(context.Background(), &recorder{})

	if len(p.seen) != 1 || p.seen[0].RemotePath != "trip/b.jpg" {
		t.Errorf("sent %+v, want only trip/b.jpg", p.seen)
	}
	if res.Done != 1 || res.Bytes != 7 {
		t.Errorf("done = %d, bytes = %d; want 1 and 7", res.Done, res.Bytes)
	}
}

// The operation sends exactly its targets — it has no directory to walk. The
// local pane's upload of one marked file used to hand the model the directory
// the file sat in, and the model uploaded all of it.
func TestUploadSendsExactlyItsTargets(t *testing.T) {
	p := &fakePusher{}
	u := newUpload(p)
	u.LocalPath = "/home/u" // the pane's directory: a label, never a root

	u.Run(context.Background(), &recorder{})

	if len(p.seen) != 1 || p.seen[0].LocalPath != "/home/u/photos/a.jpg" || p.seen[0].RemotePath != "trip/a.jpg" {
		t.Errorf("sent %+v, want exactly the one confirmed target", p.seen)
	}
}

// Every file of a successful multi-file upload is done: a single ok made the
// report read "Done: 1 of N".
func TestUploadCountsEveryFile(t *testing.T) {
	u := newUpload(&fakePusher{})
	u.Files = append(u.Files,
		model.UploadTarget{LocalPath: "/x/b", RemotePath: "trip/b", Size: 1},
		model.UploadTarget{LocalPath: "/x/c", RemotePath: "trip/c", Size: 2})

	res := u.Run(context.Background(), &recorder{})

	if res.Done != 3 || res.Done != u.Describe().TotalItems || res.Bytes != 103 {
		t.Errorf("done = %d of %d, bytes = %d; want 3 of 3 and 103", res.Done, u.Describe().TotalItems, res.Bytes)
	}
}

func TestUploadCancelWhileQueuedBehindTheGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakePusher{}
	u := newUpload(p)
	u.Gate = blockingGate{}

	go cancel()
	res := u.Run(ctx, &recorder{})

	if !res.Canceled {
		t.Error("a run cancelled while queued should be marked cancelled")
	}
	if p.seen != nil {
		t.Error("the upload started without a transfer slot")
	}
}
