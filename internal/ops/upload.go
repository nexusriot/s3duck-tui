package ops

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// Upload sends a local file or directory tree to a prefix.
//
// Unlike Download it has one phase: the destination conflicts were already
// settled before the operation was built (the overwrite scan runs against the
// bucket, not the filesystem, so it is a listing rather than a per-file
// question), and Skip carries the keys the user chose to leave alone.
type Upload struct {
	LocalPath string
	DstPrefix string
	Bucket    *model.Object
	// Files is the resolved file set, used for the counts and the confirmation.
	Files      []model.UploadTarget
	TotalBytes int64
	// Skip holds destination keys to leave alone, as decided by the overwrite
	// scan. The model drops them before it totals the transfer, so the gauge
	// never counts bytes that were never going to be sent.
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

	started := Reserve(ctx, u.Gate, func() {
		err := u.Pusher.Upload(ctx, u.LocalPath, u.DstPrefix, u.Bucket, u.Skip,
			func(sent, total int64, i, count int, local, remote string) {
				if ctx.Err() != nil {
					return
				}
				rep.Progress(Progress{
					Bytes:      sent,
					TotalBytes: total,
					Items:      i,
					TotalItems: count,
					Active:     []string{filepath.Base(local)},
				})
				_ = remote
			})

		switch {
		case ctx.Err() != nil:
			led.cancel()
		case err != nil:
			// The model's Upload aborts the whole tree on the first error, so
			// there is one failure to report rather than a per-file ledger.
			led.fail(u.LocalPath, err)
		default:
			led.ok(u.TotalBytes, false)
			rep.Log("uploaded %s (%s) to %s/%s",
				u.LocalPath, humanize.IBytes(uint64(u.TotalBytes)), *u.Bucket.Key, u.DstPrefix)
		}
	})
	if !started {
		led.cancel()
	}
	return led.result()
}
