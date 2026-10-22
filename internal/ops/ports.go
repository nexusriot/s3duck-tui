package ops

import (
	"context"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// The ports below are the operations' entire view of S3 and of the local
// filesystem. They are declared here, by the consumer, and kept as narrow as
// each operation actually needs — so *model.Model satisfies them without
// changing, and a test fake is a dozen lines rather than a mock of sixty
// methods.

// Fetcher downloads one object to its place under destPath and reports the bytes
// written. It is model.Model.DownloadTarget.
type Fetcher interface {
	DownloadTarget(
		ctx context.Context,
		t model.DownloadTarget,
		currentPath, destPath string,
		bucket *string,
		overwrite bool,
		progress func(written, total int64, key string),
	) (int64, error)
}

// Pusher uploads exactly the given targets — local path to remote key, already
// resolved — and reports progress as it goes. It is model.Model.UploadTargets.
//
// It deliberately takes no directory to walk: what the user confirmed is the
// target list, so that is what is sent. A port that walked a root of its own
// is how the local pane's upload of one marked file sent the whole directory
// it sat in.
type Pusher interface {
	UploadTargets(
		ctx context.Context,
		bucket *model.Object,
		targets []model.UploadTarget,
		progress func(current, total int64, done, count int, local, remote string),
	) error
}

// Checker compares a local file against the object it came from. It is
// model.Model.VerifyLocalFile.
type Checker interface {
	VerifyLocalFile(ctx context.Context, bucket *model.Object, key, localPath string) (model.VerifyResult, error)
}

// FS is the sliver of the filesystem the operations touch directly: testing
// whether a destination already exists, and creating directory markers. Behind
// an interface so the overwrite logic — the part with the interesting
// branches — can be tested without a temp dir.
type FS interface {
	Exists(path string) bool
	MkdirAll(path string) error
}

// Gate caps how many byte-transfer phases run at once. It is jobs.Queue.
// Acquire reports false when ctx ends first, in which case nothing was taken
// and Release must not be called.
type Gate interface {
	Acquire(ctx context.Context) bool
	Release()
}

// QueueReporter is implemented by a Reporter that shows when an operation is
// waiting for a transfer slot. Optional: Reserve checks for it.
type QueueReporter interface {
	// Queued is called with true before waiting for a slot and with false
	// once one is held (or the wait ended).
	Queued(waiting bool)
}

// Reserve runs f while holding a slot from g, or reports false if ctx ended
// while waiting. A nil Gate means "no limit", which is what tests want. rep,
// when it is a QueueReporter, is told while the wait lasts — without that a
// job behind two others read "running" in the transfers panel and the badge.
func Reserve(ctx context.Context, g Gate, rep Reporter, f func()) bool {
	if g == nil {
		f()
		return true
	}
	q, _ := rep.(QueueReporter)
	if q != nil {
		q.Queued(true)
	}
	ok := g.Acquire(ctx)
	if q != nil {
		q.Queued(false)
	}
	if !ok {
		return false
	}
	defer g.Release()
	f()
	return true
}
