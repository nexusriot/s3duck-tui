package ops

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// Upload sends a resolved set of local files to their remote keys.
//
// Unlike Download it has one phase: the destination conflicts were already
// settled before the operation was built (the overwrite scan runs against the
// bucket, not the filesystem, so it is a listing rather than a per-file
// question), and Skip carries the keys the user chose to leave alone.
type Upload struct {
	// LocalPath names the source in the job title; nothing is walked from it.
	LocalPath string
	DstPrefix string
	Bucket    *model.Object
	// Files is exactly what is sent: each target's local path and remote key,
	// as the preview and the overwrite scan saw them.
	Files      []model.UploadTarget
	TotalBytes int64
	// Skip holds destination keys to leave alone, as decided by the overwrite
	// scan. Files matching it are dropped before anything is sent.
	Skip map[string]bool

	Pusher Pusher
	Gate   Gate
}

func (u *Upload) Describe() Descriptor {
	return Descriptor{
		Kind:         KindUpload,
		Title:        fmt.Sprintf("%s → %s/%s", filepath.Base(u.LocalPath), *u.Bucket.Key, u.DstPrefix),
		TotalBytes:   u.TotalBytes,
		TotalItems:   len(u.Files),
		RefreshAfter: true,
	}
}

func (u *Upload) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}

	files := u.Files
	if len(u.Skip) > 0 {
		files = make([]model.UploadTarget, 0, len(u.Files))
		for _, f := range u.Files {
			if !u.Skip[f.RemotePath] {
				files = append(files, f)
			}
		}
	}
	var bytes int64
	for _, f := range files {
		bytes += f.Size
	}

	started := Reserve(ctx, u.Gate, func() {
		err := u.Pusher.UploadTargets(ctx, u.Bucket, files,
			func(sent, total int64, done, count int, local, _ string) {
				if ctx.Err() != nil {
					return
				}
				rep.Progress(Progress{
					Bytes:      sent,
					TotalBytes: total,
					Items:      done,
					TotalItems: count,
					Active:     []string{filepath.Base(local)},
				})
			})

		switch {
		case ctx.Err() != nil:
			led.cancel()
		case err != nil:
			// The model aborts the whole set on the first error, so there is
			// one failure to report rather than a per-file ledger.
			led.fail(u.LocalPath, err)
		default:
			// One ok per file: the report reads "Done: N of N", and a single
			// ok here made every successful multi-file upload say "1 of N".
			for range files {
				led.ok(0, false)
			}
			led.addBytes(bytes)
			rep.Log("uploaded %s (%s) to %s/%s",
				u.LocalPath, humanize.IBytes(uint64(bytes)), *u.Bucket.Key, u.DstPrefix)
		}
	})
	if !started {
		led.cancel()
	}
	return led.result()
}
