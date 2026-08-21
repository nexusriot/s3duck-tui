package model

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// SizeUnknown marks a copy whose source size the caller hasn't got. Resolving
// it costs one HeadObject, which is why every bulk path (a folder listing, a
// sync plan) passes the size it already knows instead.
const SizeUnknown int64 = -1

// A single-request CopyObject is capped at 5 GiB by S3; larger sources must be
// copied part by part with UploadPartCopy. Both knobs are variables rather
// than constants so the integration suite can exercise the multipart path
// without moving 5 GiB around — nothing else should write to them.
var (
	// MultipartCopyThreshold is the source size above which a copy is done as
	// a multipart part-copy.
	MultipartCopyThreshold int64 = 5 * 1024 * 1024 * 1024

	// MultipartCopyPartSize is the target part size. S3 requires every part
	// except the last to be at least 5 MiB and allows at most 10,000 parts;
	// 512 MiB × 10,000 covers the 5 TiB maximum object size.
	MultipartCopyPartSize int64 = 512 * 1024 * 1024
)

const (
	// copyPartMinSize is the smallest part S3 accepts (except the last one).
	copyPartMinSize = 5 * 1024 * 1024
	// copyMaxParts is the S3 limit on parts in one multipart upload.
	copyMaxParts = 10000
	// copyPartWorkers bounds the concurrent part-copies. The bytes move inside
	// S3, so this costs no local bandwidth — it only stops a 100 GiB object
	// from taking one round trip per 512 MiB in series.
	copyPartWorkers = 4
	// abortTimeout bounds the cleanup call after a failed multipart copy, so a
	// wedged endpoint can't hang the flow on its way out.
	abortTimeout = 30 * time.Second
)

// copyPart is one part of a multipart copy: its 1-based number and the
// inclusive byte range of the source it covers.
type copyPart struct {
	Number int32
	Start  int64
	End    int64
}

// copyPartSize picks the part size for a source of the given size: the
// configured size, grown when the source would otherwise need more than
// copyMaxParts parts.
func copyPartSize(size int64) int64 {
	part := MultipartCopyPartSize
	if part < copyPartMinSize {
		part = copyPartMinSize
	}
	// Round the minimum viable part size up to a whole MiB so the numbers stay
	// legible in logs and errors.
	if need := (size + copyMaxParts - 1) / copyMaxParts; need > part {
		const mib = 1 << 20
		part = ((need + mib - 1) / mib) * mib
	}
	return part
}

// planCopyParts splits a source of size bytes into part ranges. The last part
// absorbs the remainder, so it is the only one that may be under the 5 MiB
// minimum — which S3 allows precisely because it is last.
func planCopyParts(size int64) []copyPart {
	if size <= 0 {
		return nil
	}
	part := copyPartSize(size)

	var parts []copyPart
	for start, n := int64(0), int32(1); start < size; n++ {
		end := start + part - 1
		if end > size-1 {
			end = size - 1
		}
		parts = append(parts, copyPart{Number: n, Start: start, End: end})
		start = end + 1
	}
	return parts
}

// copySpec describes one server-side copy in enough detail to run it either
// way. The paths differ in more than mechanics: CopyObject inherits the
// source's attributes for free, while CreateMultipartUpload starts a fresh
// object and must be handed everything explicitly.
type copySpec struct {
	srcBucket  string
	srcKey     string
	srcVersion string // "" = current version
	dstBucket  string
	dstKey     string

	// srcSize is the source's size, or SizeUnknown.
	srcSize int64

	// meta, when non-nil, replaces the destination's attributes
	// (MetadataDirective=REPLACE). nil copies the source's through.
	meta *ObjectMeta
	// storageClass overrides the destination's storage class. Empty leaves it
	// to the copy — which means STANDARD, since that is what a CopyObject
	// without an explicit class produces.
	storageClass string
}

// copySourceValue is the x-amz-copy-source header for this spec.
func (sp copySpec) copySourceValue() string {
	if sp.srcVersion != "" {
		return versionCopySource(sp.srcBucket, sp.srcKey, sp.srcVersion)
	}
	return copySource(sp.srcBucket, sp.srcKey)
}

// runCopy executes a copy, choosing the single-request or multipart path by
// the source's size.
func (m *Model) runCopy(ctx context.Context, sp copySpec) error {
	size := sp.srcSize
	var head *ObjectMeta

	if size == SizeUnknown {
		h, err := m.headForCopy(ctx, sp)
		if err != nil {
			return fmt.Errorf("reading %s: %w", sp.srcKey, err)
		}
		head, size = &h, h.Size
	}

	if size <= MultipartCopyThreshold {
		return m.copySingle(ctx, sp)
	}

	// The multipart path builds a brand-new object, so whatever the single
	// path would have inherited has to be fetched and re-applied.
	attrs := sp.meta
	if attrs == nil {
		if head == nil {
			h, err := m.headForCopy(ctx, sp)
			if err != nil {
				return fmt.Errorf("reading %s: %w", sp.srcKey, err)
			}
			head = &h
		}
		attrs = head
	}
	return m.copyMultipart(ctx, sp, size, *attrs)
}

// headForCopy reads the source's attributes, honoring the source version.
func (m *Model) headForCopy(ctx context.Context, sp copySpec) (ObjectMeta, error) {
	in := &s3.HeadObjectInput{
		Bucket: aws.String(sp.srcBucket),
		Key:    aws.String(sp.srcKey),
	}
	if sp.srcVersion != "" {
		in.VersionId = aws.String(sp.srcVersion)
	}
	out, err := m.Client.HeadObject(ctx, in)
	if err != nil {
		return ObjectMeta{}, err
	}
	return metaFromHead(out), nil
}

// copySingle is the ordinary one-request copy.
func (m *Model) copySingle(ctx context.Context, sp copySpec) error {
	in := &s3.CopyObjectInput{
		Bucket:     aws.String(sp.dstBucket),
		Key:        aws.String(sp.dstKey),
		CopySource: aws.String(sp.copySourceValue()),
	}
	if sp.meta != nil {
		in.MetadataDirective = s3t.MetadataDirectiveReplace
		in.Metadata = sp.meta.UserMetadata
		if sp.meta.ContentType != "" {
			in.ContentType = aws.String(sp.meta.ContentType)
		}
		if sp.meta.CacheControl != "" {
			in.CacheControl = aws.String(sp.meta.CacheControl)
		}
		if sp.meta.ContentDisposition != "" {
			in.ContentDisposition = aws.String(sp.meta.ContentDisposition)
		}
		if sp.meta.ContentEncoding != "" {
			in.ContentEncoding = aws.String(sp.meta.ContentEncoding)
		}
	}
	if class := sp.destinationClass(); class != "" {
		in.StorageClass = s3t.StorageClass(class)
	}

	_, err := m.Client.CopyObject(ctx, in)
	return err
}

// destinationClass is the storage class to write, if any was asked for.
func (sp copySpec) destinationClass() string {
	if sp.storageClass != "" {
		return sp.storageClass
	}
	if sp.meta != nil {
		return sp.meta.StorageClass
	}
	return ""
}

// copyMultipart copies a source larger than the single-request limit by
// part-copying byte ranges server-side. attrs supplies the destination's
// attributes, which CreateMultipartUpload needs up front — after the parts are
// in flight there is no way to add them.
func (m *Model) copyMultipart(ctx context.Context, sp copySpec, size int64, attrs ObjectMeta) error {
	parts := planCopyParts(size)
	if len(parts) == 0 {
		return fmt.Errorf("nothing to copy: %s is %d bytes", sp.srcKey, size)
	}

	create := &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(sp.dstBucket),
		Key:      aws.String(sp.dstKey),
		Metadata: attrs.UserMetadata,
	}
	if attrs.ContentType != "" {
		create.ContentType = aws.String(attrs.ContentType)
	}
	if attrs.CacheControl != "" {
		create.CacheControl = aws.String(attrs.CacheControl)
	}
	if attrs.ContentDisposition != "" {
		create.ContentDisposition = aws.String(attrs.ContentDisposition)
	}
	if attrs.ContentEncoding != "" {
		create.ContentEncoding = aws.String(attrs.ContentEncoding)
	}
	if class := sp.destinationClass(); class != "" {
		create.StorageClass = s3t.StorageClass(class)
	}
	// A single-request copy carries the source's tags along whatever else it
	// does — TaggingDirective defaults to COPY even under
	// MetadataDirective=REPLACE — while a fresh multipart upload starts with
	// none. So the tags are re-applied on every multipart copy, including a
	// metadata save, and are read from the source *version* being copied.
	// Tagging is optional on S3-compatible backends, so a failure degrades to
	// "no tags" rather than failing a multi-gigabyte copy outright — the same
	// trade the metadata editor makes.
	if tagging, err := getObjectTagging(ctx, m.Client, sp.srcBucket, sp.srcKey, sp.srcVersion); err == nil && tagging != "" {
		create.Tagging = aws.String(tagging)
	}

	started, err := m.Client.CreateMultipartUpload(ctx, create)
	if err != nil {
		return fmt.Errorf("starting multipart copy of %s: %w", sp.srcKey, err)
	}
	uploadID := aws.ToString(started.UploadId)

	done, err := m.copyParts(ctx, sp, parts, uploadID)
	if err != nil {
		m.abortMultipart(ctx, sp, uploadID)
		return err
	}

	if _, err := m.Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(sp.dstBucket),
		Key:             aws.String(sp.dstKey),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &s3t.CompletedMultipartUpload{Parts: done},
	}); err != nil {
		// Completion can fail on its own (a rejected part list, a dead
		// connection) and leaves the upload just as open as a failed part
		// does, so it gets the same cleanup.
		m.abortMultipart(ctx, sp, uploadID)
		return fmt.Errorf("completing multipart copy of %s: %w", sp.dstKey, err)
	}
	return nil
}

// abortMultipart discards an upload whose parts will never be completed, so
// they stop accruing storage charges. It runs on a context detached from the
// caller's: a cancelled copy leaves ctx already dead, and the cleanup request
// would be dropped before it was ever sent.
func (m *Model) abortMultipart(ctx context.Context, sp copySpec, uploadID string) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_, _ = m.Client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(sp.dstBucket),
		Key:      aws.String(sp.dstKey),
		UploadId: aws.String(uploadID),
	})
}

// copyParts runs the part-copies through a bounded worker pool and returns
// them ordered by part number, as CompleteMultipartUpload requires.
func (m *Model) copyParts(ctx context.Context, sp copySpec, parts []copyPart, uploadID string) ([]s3t.CompletedPart, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	source := sp.copySourceValue()
	done := make([]s3t.CompletedPart, len(parts))

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, copyPartWorkers)

	for i, p := range parts {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			wg.Add(1)
			go func(i int, p copyPart) {
				defer wg.Done()
				defer func() { <-sem }()

				out, err := m.Client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
					Bucket:          aws.String(sp.dstBucket),
					Key:             aws.String(sp.dstKey),
					UploadId:        aws.String(uploadID),
					PartNumber:      p.Number,
					CopySource:      aws.String(source),
					CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", p.Start, p.End)),
				})

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("copying part %d of %s: %w", p.Number, sp.srcKey, err)
						// Stop the parts still in flight; the caller aborts
						// the upload so none of them are worth finishing.
						cancel()
					}
					return
				}
				var etag *string
				if out.CopyPartResult != nil {
					etag = out.CopyPartResult.ETag
				}
				done[i] = s3t.CompletedPart{ETag: etag, PartNumber: p.Number}
			}(i, p)
			continue
		}
		break
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, d := range done {
		if d.ETag == nil {
			return nil, fmt.Errorf("multipart copy of %s: part %d returned no ETag", sp.srcKey, parts[i].Number)
		}
	}
	return done, nil
}
