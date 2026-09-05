package model

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectContent is a whole small object in memory, together with every
// replicable attribute a rewrite must carry back — Content-Type alone is not
// enough: Cache-Control, Content-Disposition/Encoding/Language, the storage
// class, user metadata and tags are all silently stripped by a plain
// re-upload, which is exactly what saving an edited object must not do.
type ObjectContent struct {
	Data []byte
	// ETag as returned by the GET (quotes trimmed). A rewrite compares it
	// against the object's current ETag to detect a concurrent modification
	// before overwriting it (lost-update guard).
	ETag string

	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	// StorageClass as reported by the GET ("" means the backend default).
	// Forwarded on same-endpoint rewrites only — class names are not portable
	// across providers, so CrossCopy deliberately leaves it to the destination.
	StorageClass string
	UserMetadata map[string]string
	// Tagging is the object's tag set in URL-encoded form ("k=v&k2=v2"),
	// ready for PutObjectInput.Tagging. Empty when the object has no tags.
	Tagging string
}

// trimETag strips the RFC 7232 quotes S3 wraps ETags in.
func trimETag(s *string) string {
	return strings.Trim(aws.ToString(s), `"`)
}

// getObjectTagging fetches an object's tags as a URL-encoded string. version
// selects a specific version; empty means the current one — a copy from an old
// version must carry that version's tags, not whatever the key holds now.
func getObjectTagging(ctx context.Context, client *s3.Client, bucket, key, version string) (string, error) {
	in := &s3.GetObjectTaggingInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	out, err := client.GetObjectTagging(ctx, in)
	if err != nil {
		return "", err
	}
	vals := url.Values{}
	for _, t := range out.TagSet {
		vals.Set(aws.ToString(t.Key), aws.ToString(t.Value))
	}
	return vals.Encode(), nil
}

// attrsFromGet lifts the replicable attributes off a GET response.
func attrsFromGet(out *s3.GetObjectOutput) ObjectContent {
	c := ObjectContent{
		ETag:               trimETag(out.ETag),
		ContentType:        aws.ToString(out.ContentType),
		CacheControl:       aws.ToString(out.CacheControl),
		ContentDisposition: aws.ToString(out.ContentDisposition),
		ContentEncoding:    aws.ToString(out.ContentEncoding),
		ContentLanguage:    aws.ToString(out.ContentLanguage),
		StorageClass:       string(out.StorageClass),
		UserMetadata:       map[string]string{},
	}
	for k, v := range out.Metadata {
		c.UserMetadata[k] = v
	}
	return c
}

// applyAttrs copies the carried attributes onto a PutObjectInput. StorageClass
// is applied only when withClass is true (same-endpoint rewrites).
func applyAttrs(in *s3.PutObjectInput, attrs ObjectContent, withClass bool) {
	if attrs.ContentType != "" {
		in.ContentType = aws.String(attrs.ContentType)
	}
	if attrs.CacheControl != "" {
		in.CacheControl = aws.String(attrs.CacheControl)
	}
	if attrs.ContentDisposition != "" {
		in.ContentDisposition = aws.String(attrs.ContentDisposition)
	}
	if attrs.ContentEncoding != "" {
		in.ContentEncoding = aws.String(attrs.ContentEncoding)
	}
	if attrs.ContentLanguage != "" {
		in.ContentLanguage = aws.String(attrs.ContentLanguage)
	}
	if len(attrs.UserMetadata) > 0 {
		in.Metadata = attrs.UserMetadata
	}
	if attrs.Tagging != "" {
		in.Tagging = aws.String(attrs.Tagging)
	}
	if withClass && attrs.StorageClass != "" {
		in.StorageClass = s3t.StorageClass(attrs.StorageClass)
	}
}

// GetObjectContent fetches an entire object body, refusing anything larger
// than maxSize — this exists for the in-editor flow, which loads the object
// into memory. The reported ContentLength is checked first, but the body read
// is capped independently: the object may have grown since it was listed, and
// a lying backend must not be able to balloon the process.
func (m *Model) GetObjectContent(ctx context.Context, bucket *Object, key string, maxSize int64) (ObjectContent, error) {
	if bucket == nil || bucket.Key == nil {
		return ObjectContent{}, fmt.Errorf("bucket is nil")
	}
	if key == "" {
		return ObjectContent{}, fmt.Errorf("key is empty")
	}
	if maxSize <= 0 {
		return ObjectContent{}, fmt.Errorf("maxSize must be positive")
	}

	out, err := m.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	})
	if err != nil {
		return ObjectContent{}, err
	}
	defer out.Body.Close()

	if out.ContentLength > maxSize {
		return ObjectContent{}, fmt.Errorf("object is %d bytes, over the %d-byte editing cap", out.ContentLength, maxSize)
	}

	data, err := io.ReadAll(io.LimitReader(out.Body, maxSize+1))
	if err != nil {
		return ObjectContent{}, err
	}
	if int64(len(data)) > maxSize {
		return ObjectContent{}, fmt.Errorf("object body exceeds the %d-byte editing cap", maxSize)
	}

	content := attrsFromGet(out)
	content.Data = data
	if out.TagCount > 0 {
		tagging, err := getObjectTagging(ctx, m.Client, *bucket.Key, key, "")
		if err != nil {
			return ObjectContent{}, fmt.Errorf("reading tags of %s: %w", key, err)
		}
		content.Tagging = tagging
	}
	return content, nil
}

// GetObjectHead reads the first n bytes of an object with a ranged GET, for
// the preview. A range is the whole point: previewing a 40 GiB log has to cost
// one small request, not a download, and S3 answers a Range on any object
// whatever its size.
//
// A backend that ignores the Range header (some S3-compatible ones do) would
// otherwise stream the whole body into memory, so the read is capped
// independently of what was asked for — the same belt-and-braces the editor's
// loader uses.
func (m *Model) GetObjectHead(ctx context.Context, bucket *Object, key string, n int64) (data []byte, contentType string, err error) {
	if bucket == nil || bucket.Key == nil {
		return nil, "", fmt.Errorf("bucket is nil")
	}
	if key == "" {
		return nil, "", fmt.Errorf("key is empty")
	}
	if n <= 0 {
		return nil, "", fmt.Errorf("n must be positive")
	}
	out, err := m.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=0-%d", n-1)),
	})
	if err != nil {
		return nil, "", err
	}
	defer out.Body.Close()

	data, err = io.ReadAll(io.LimitReader(out.Body, n))
	if err != nil {
		return nil, "", err
	}
	return data, aws.ToString(out.ContentType), nil
}

// GetVersionContent is GetObjectContent for one specific version, which is
// what a version diff needs: the current body is one GET, the old body is the
// same GET with a VersionId.
func (m *Model) GetVersionContent(ctx context.Context, bucket *Object, key, versionID string, maxSize int64) ([]byte, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}
	in := &s3.GetObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := m.Client.GetObject(ctx, in)
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	if out.ContentLength > maxSize {
		return nil, fmt.Errorf("version is %d bytes, over the %d-byte cap", out.ContentLength, maxSize)
	}
	data, err := io.ReadAll(io.LimitReader(out.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("version body exceeds the %d-byte cap", maxSize)
	}
	return data, nil
}

// CurrentETag returns the object's ETag right now (quotes trimmed), via a
// HEAD. The editor flow compares it against the ETag captured at download
// time so a save can refuse to overwrite a concurrent modification.
func (m *Model) CurrentETag(ctx context.Context, bucket *Object, key string) (string, error) {
	if bucket == nil || bucket.Key == nil {
		return "", fmt.Errorf("bucket is nil")
	}
	out, err := m.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", err
	}
	return trimETag(out.ETag), nil
}

// PutBytes writes a small in-memory body to a key, carrying attrs' replicable
// attributes (Content-Type and friends, storage class, metadata, tags) along.
// attrs.Data and attrs.ETag are ignored — data is the body being written.
func (m *Model) PutBytes(ctx context.Context, bucket *Object, key string, data []byte, attrs ObjectContent) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" {
		return fmt.Errorf("key is empty")
	}

	in := &s3.PutObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	}
	applyAttrs(in, attrs, true)
	// applyAttrs already carried the object's own Content-Type across, so
	// applyPut only fills one in when the object never had one, and always
	// applies the profile's encryption/checksum choice.
	m.writeOpts().applyPut(in, key)
	_, err := m.Client.PutObject(ctx, in)
	return err
}

// CrossCopy streams one object between two independent clients: a GET from the
// source feeds a multipart PUT at the destination, so the two sides may be
// different endpoints entirely (MinIO → AWS) — the case server-side CopyObject
// can never handle. Content headers, user metadata and tags ride along from
// the source; the storage class deliberately does not (class names differ
// across providers — STANDARD_IA would fail the whole PUT on a backend that
// doesn't know it — so the destination's default applies). The source model's
// bandwidth limiter throttles the read side, which bounds the whole pipe.
func CrossCopy(ctx context.Context, src *Model, srcBucket *Object, srcKey string, dst *Model, dstBucket *Object, dstKey string, progress func(written, total int64)) error {
	if src == nil || dst == nil {
		return fmt.Errorf("source and destination models are required")
	}
	if srcBucket == nil || srcBucket.Key == nil || dstBucket == nil || dstBucket.Key == nil {
		return fmt.Errorf("source and destination buckets are required")
	}
	if srcKey == "" || dstKey == "" || strings.HasSuffix(dstKey, "/") {
		return fmt.Errorf("bad keys: %q → %q", srcKey, dstKey)
	}

	out, err := src.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(*srcBucket.Key),
		Key:    aws.String(srcKey),
	})
	if err != nil {
		return fmt.Errorf("reading %s: %w", srcKey, err)
	}
	defer out.Body.Close()

	attrs := attrsFromGet(out)
	if out.TagCount > 0 {
		tagging, err := getObjectTagging(ctx, src.Client, *srcBucket.Key, srcKey, "")
		if err != nil {
			return fmt.Errorf("reading tags of %s: %w", srcKey, err)
		}
		attrs.Tagging = tagging
	}

	// progressReader calls update unconditionally, so it must never be nil.
	update := func(int64, int64) {}
	if progress != nil {
		update = progress
	}
	reader := &progressReader{
		r:       out.Body,
		total:   out.ContentLength,
		update:  update,
		limiter: src.Limiter,
	}

	in := &s3.PutObjectInput{
		Bucket: aws.String(*dstBucket.Key),
		Key:    aws.String(dstKey),
		Body:   reader,
	}
	applyAttrs(in, attrs, false)
	// The destination profile's rules apply, not the source's: encryption is
	// a property of where the bytes land. The source's Content-Type rides
	// along in attrs, so the derivation here only fills a gap.
	dst.writeOpts().applyPut(in, dstKey)

	if _, err := newUploader(dst.Client).Upload(ctx, in); err != nil {
		return fmt.Errorf("writing %s: %w", dstKey, err)
	}
	return nil
}
