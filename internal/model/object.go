package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectMeta is the editable metadata of a single object, plus the read-only
// facts (size, etag, restore state) the properties view shows alongside it.
type ObjectMeta struct {
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	StorageClass       string
	// UserMetadata holds the x-amz-meta-* pairs with the prefix stripped; the
	// SDK adds and removes it, so keys here are the bare names.
	UserMetadata map[string]string

	Size         int64
	ETag         string
	LastModified *time.Time
	// Restore is the raw x-amz-restore header for an archived object; empty
	// when the object is not archived. Parse it with ParseRestoreStatus.
	Restore string
}

// StorageClasses lists the classes offered in the change-class dialog. It is
// deliberately a curated list rather than every value the SDK knows: these are
// the ones a copy-in-place can legitimately move an object to.
func StorageClasses() []string {
	return []string{
		"STANDARD",
		"STANDARD_IA",
		"ONEZONE_IA",
		"INTELLIGENT_TIERING",
		"GLACIER_IR",
		"GLACIER",
		"DEEP_ARCHIVE",
		"REDUCED_REDUNDANCY",
	}
}

// ArchivedStorageClasses are the classes whose objects must be restored before
// they can be read.
func ArchivedStorageClasses() []string {
	return []string{"GLACIER", "DEEP_ARCHIVE"}
}

// IsArchived reports whether objects in this storage class need a restore
// before they can be downloaded.
func IsArchived(class string) bool {
	for _, c := range ArchivedStorageClasses() {
		if strings.EqualFold(class, c) {
			return true
		}
	}
	return false
}

// ParseRestoreStatus interprets the x-amz-restore header, which looks like
//
//	ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"
//
// ongoing reports a restore still in progress; expiry is when a completed
// restore's temporary copy disappears (empty while ongoing). ok is false when
// the header is absent or unparseable, meaning "not archived / unknown".
func ParseRestoreStatus(header string) (ongoing bool, expiry string, ok bool) {
	h := strings.TrimSpace(header)
	if h == "" {
		return false, "", false
	}

	if v, found := restoreHeaderValue(h, "ongoing-request"); found {
		if b, err := strconv.ParseBool(v); err == nil {
			ongoing, ok = b, true
		}
	}
	expiry, _ = restoreHeaderValue(h, "expiry-date")
	return ongoing, expiry, ok
}

// restoreHeaderValue pulls one directive out of an x-amz-restore header.
// Values are quoted and may themselves contain commas — an RFC 1123 date does,
// e.g. expiry-date="Fri, 21 Dec 2012 00:00:00 GMT" — so the header cannot be
// split on commas; the value runs to its closing quote.
func restoreHeaderValue(header, key string) (string, bool) {
	i := strings.Index(header, key+"=")
	if i < 0 {
		return "", false
	}
	rest := strings.TrimSpace(header[i+len(key)+1:])

	if !strings.HasPrefix(rest, `"`) {
		end := strings.IndexByte(rest, ',')
		if end < 0 {
			end = len(rest)
		}
		return strings.TrimSpace(rest[:end]), true
	}

	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// RestoreSummary renders the restore state for display.
func RestoreSummary(storageClass, restoreHeader string) string {
	ongoing, expiry, ok := ParseRestoreStatus(restoreHeader)
	if !ok {
		if IsArchived(storageClass) {
			return "archived (not restored)"
		}
		return ""
	}
	if ongoing {
		return "restore in progress"
	}
	if expiry != "" {
		return "restored until " + expiry
	}
	return "restored"
}

// HeadObject fetches an object's metadata without downloading its body.
func (m *Model) HeadObject(ctx context.Context, bucket *Object, key string) (ObjectMeta, error) {
	if bucket == nil || bucket.Key == nil {
		return ObjectMeta{}, fmt.Errorf("bucket is nil")
	}
	if key == "" {
		return ObjectMeta{}, fmt.Errorf("key is empty")
	}

	out, err := m.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	})
	if err != nil {
		return ObjectMeta{}, err
	}
	return metaFromHead(out), nil
}

// metaFromHead lifts a HeadObject response into an ObjectMeta.
func metaFromHead(out *s3.HeadObjectOutput) ObjectMeta {
	meta := ObjectMeta{
		ContentType:        aws.ToString(out.ContentType),
		CacheControl:       aws.ToString(out.CacheControl),
		ContentDisposition: aws.ToString(out.ContentDisposition),
		ContentEncoding:    aws.ToString(out.ContentEncoding),
		StorageClass:       string(out.StorageClass),
		UserMetadata:       map[string]string{},
		Size:               out.ContentLength,
		ETag:               strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified:       out.LastModified,
		Restore:            aws.ToString(out.Restore),
	}
	for k, v := range out.Metadata {
		meta.UserMetadata[k] = v
	}
	// HeadObject omits the storage class for STANDARD objects.
	if meta.StorageClass == "" {
		meta.StorageClass = "STANDARD"
	}
	return meta
}

// PutObjectMeta rewrites an object's metadata by copying it onto itself with
// MetadataDirective=REPLACE — S3 has no metadata-update API. The storage class
// is passed through explicitly because a replace-copy that omits it would
// silently demote the object to STANDARD. meta.Size lets the copy skip a
// HeadObject when deciding whether it must go multipart; it is filled in by
// the HeadObject the editor already did.
func (m *Model) PutObjectMeta(ctx context.Context, bucket *Object, key string, meta ObjectMeta) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" || strings.HasSuffix(key, "/") {
		return fmt.Errorf("not a file key: %q", key)
	}

	size := meta.Size
	if size <= 0 {
		size = SizeUnknown
	}
	return m.runCopy(ctx, copySpec{
		srcBucket: *bucket.Key,
		srcKey:    key,
		dstBucket: *bucket.Key,
		dstKey:    key,
		srcSize:   size,
		meta:      &meta,
	})
}

// SetStorageClass moves an object to another storage class with a copy in
// place. The existing metadata rides along untouched (MetadataDirective
// defaults to COPY on the single-request path, and the multipart path
// replicates the source's attributes explicitly).
func (m *Model) SetStorageClass(ctx context.Context, bucket *Object, key, class string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" || strings.HasSuffix(key, "/") {
		return fmt.Errorf("not a file key: %q", key)
	}
	if class == "" {
		return fmt.Errorf("storage class is empty")
	}

	return m.runCopy(ctx, copySpec{
		srcBucket:    *bucket.Key,
		srcKey:       key,
		dstBucket:    *bucket.Key,
		dstKey:       key,
		srcSize:      SizeUnknown,
		storageClass: class,
	})
}

// RestoreObject asks S3 to make an archived object temporarily readable for
// days days. tier is Expedited / Standard / Bulk; empty means Standard.
func (m *Model) RestoreObject(ctx context.Context, bucket *Object, key string, days int32, tier string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" || strings.HasSuffix(key, "/") {
		return fmt.Errorf("not a file key: %q", key)
	}
	if days < 1 {
		return fmt.Errorf("days must be at least 1, got %d", days)
	}
	if tier == "" {
		tier = string(s3t.TierStandard)
	}

	_, err := m.Client.RestoreObject(ctx, &s3.RestoreObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
		RestoreRequest: &s3t.RestoreRequest{
			Days:                 days,
			GlacierJobParameters: &s3t.GlacierJobParameters{Tier: s3t.Tier(tier)},
		},
	})
	return err
}

// RestoreTiers lists the retrieval speeds RestoreObject accepts.
func RestoreTiers() []string {
	return []string{string(s3t.TierStandard), string(s3t.TierBulk), string(s3t.TierExpedited)}
}

// ObjectTags returns the object's tag set as a plain map.
func (m *Model) ObjectTags(ctx context.Context, bucket *Object, key string) (map[string]string, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}
	out, err := m.Client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	tags := make(map[string]string, len(out.TagSet))
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// PutObjectTags replaces the object's tag set. An empty map removes every tag
// (via DeleteObjectTagging, which is what S3 expects for "no tags").
func (m *Model) PutObjectTags(ctx context.Context, bucket *Object, key string, tags map[string]string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if len(tags) == 0 {
		_, err := m.Client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
			Bucket: aws.String(*bucket.Key),
			Key:    aws.String(key),
		})
		return err
	}

	set := make([]s3t.Tag, 0, len(tags))
	for k, v := range tags {
		set = append(set, s3t.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := m.Client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket:  aws.String(*bucket.Key),
		Key:     aws.String(key),
		Tagging: &s3t.Tagging{TagSet: set},
	})
	return err
}
