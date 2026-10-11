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

// Fetcher downloads one object to its place under destDir and reports the bytes
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

// Pusher uploads a local tree and reports progress per file. It is
// model.Model.Upload.
type Pusher interface {
	Upload(
		ctx context.Context,
		localPath, s3Prefix string,
		bucket *model.Object,
		skip map[string]bool,
		progress func(current, total int64, i, count int, local, remote string),
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

// Reserve runs f while holding a slot from g, or reports false if ctx ended
// while waiting. A nil Gate means "no limit", which is what tests want.
func Reserve(ctx context.Context, g Gate, f func()) bool {
	if g == nil {
		f()
		return true
	}
	if !g.Acquire(ctx) {
		return false
	}
	defer g.Release()
	f()
	return true
}
