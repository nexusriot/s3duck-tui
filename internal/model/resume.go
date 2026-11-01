package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// Download resume. An interrupted download used to restart from byte 0: its
// temp file was removed on cancel and on failure, so a 20 GiB object that
// failed at 19 GiB — or was cancelled by quitting — cost 19 GiB again. A
// download big enough to be worth resuming now keeps what it has:
//
//   - its partial file has a stable name derived from bucket and key, beside
//     the target (so the final rename stays atomic);
//   - a sidecar records which object it is (bucket, key, ETag, size) and how
//     many bytes from the start are on disk;
//   - the next download of the same object finds both, checks that the object
//     is still that version, and fetches only the rest — a ranged GET pinned
//     to the ETag with If-Match, so bytes of two different versions can never
//     be stitched together; an object that changed starts over.
//
// The download itself is parallel (5 MiB ranges, several in flight), so the
// file fills out of order; only the contiguous prefix from byte 0 counts as
// "had". That is computed from the writes actually made, not from the file's
// size, which a sparse out-of-order write makes meaningless.

// ResumeMinSize is the smallest object a download keeps a partial file for.
// Below it, restarting costs seconds, and a sidecar per small file would only
// clutter the directory.
const ResumeMinSize int64 = 8 << 20

// partialSuffix and sidecarSuffix mark the files a resumable download leaves.
const (
	partialSuffix = ".s3duck-part"
	sidecarSuffix = ".s3duck-resume"
)

// IsPartialFile reports a download's partial or sidecar file — never an
// object to upload, sync or list as the user's own.
func IsPartialFile(name string) bool {
	return strings.HasSuffix(name, partialSuffix) || strings.HasSuffix(name, sidecarSuffix)
}

// resumeState is a partial download's sidecar.
type resumeState struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
	// Have is how many bytes from the start of the object are on disk.
	Have int64 `json:"have"`
}

// partialPaths names the partial file and its sidecar for an object landing
// at target. The hash keeps two keys that map to one local path ("a//b" and
// "a/b") from sharing a partial; the sidecar also records the key, and is
// checked, so a hash collision cannot mix them either.
func partialPaths(target, bucket, key string) (partial, sidecar string) {
	h := fnv.New64a()
	_, _ = io.WriteString(h, bucket+"\x00"+key)
	base := "." + tempStem(filepath.Base(target)) + fmt.Sprintf(".%08x", h.Sum64()&0xffffffff)
	dir := filepath.Dir(target)
	return filepath.Join(dir, base+partialSuffix), filepath.Join(dir, base+sidecarSuffix)
}

// loadResume returns how many bytes of the partial can be kept for this
// object, or 0. Anything that does not match — another object, another
// version, a size change, a partial shorter than its sidecar claims — is
// discarded, and the download starts over.
func loadResume(partial, sidecar, bucket, key, etag string, size int64) int64 {
	data, err := os.ReadFile(sidecar)
	if err != nil {
		return 0
	}
	var st resumeState
	if json.Unmarshal(data, &st) != nil ||
		st.Bucket != bucket || st.Key != key || st.ETag == "" || st.ETag != etag ||
		st.Size != size || st.Have <= 0 || st.Have >= size {
		discardPartial(partial, sidecar)
		return 0
	}
	fi, err := os.Stat(partial)
	if err != nil || fi.Size() < st.Have {
		discardPartial(partial, sidecar)
		return 0
	}
	return st.Have
}

// saveResume records have bytes of the object as kept, trimming the partial
// to exactly that so nothing past the prefix — holes or a half-written range
// — is ever read back. With nothing worth keeping, both files go.
func saveResume(partial, sidecar string, st resumeState) {
	if st.Have <= 0 || st.ETag == "" {
		discardPartial(partial, sidecar)
		return
	}
	if err := os.Truncate(partial, st.Have); err != nil {
		discardPartial(partial, sidecar)
		return
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(sidecar, data, 0600); err != nil {
		discardPartial(partial, sidecar)
	}
}

func discardPartial(partial, sidecar string) {
	_ = os.Remove(partial)
	_ = os.Remove(sidecar)
}

// coverage records which byte ranges a download has written, so a failed one
// knows how much of the file, from a given offset, is complete.
type coverage struct {
	mu    sync.Mutex
	spans [][2]int64 // [start, end)
}

func (c *coverage) add(off, n int64) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	c.spans = append(c.spans, [2]int64{off, off + n})
	c.mu.Unlock()
}

// prefix is the end of the contiguous run of written bytes starting at from
// (from itself when nothing at from was written).
func (c *coverage) prefix(from int64) int64 {
	c.mu.Lock()
	spans := append([][2]int64(nil), c.spans...)
	c.mu.Unlock()
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	end := from
	for _, s := range spans {
		if s[0] > end {
			break
		}
		if s[1] > end {
			end = s[1]
		}
	}
	return end
}

// offsetWriterAt writes at base+off and records what it wrote. A ranged GET
// writes its body from offset 0; the resumed part belongs at base.
type offsetWriterAt struct {
	w    io.WriterAt
	base int64
	cov  *coverage
}

func (o *offsetWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := o.w.WriteAt(p, o.base+off)
	o.cov.add(o.base+off, int64(n))
	return n, err
}

// isPreconditionFailed reports a 412: the object is no longer the version an
// If-Match named.
func isPreconditionFailed(err error) bool {
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
		return true
	}
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed
}

// ErrObjectChanged is a download whose object was replaced while it ran (or
// since a partial was kept): its bytes would mix two versions, so they are
// discarded.
var ErrObjectChanged = errors.New("the object changed during the download")
