package model

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectVersion is one entry in an object's version history. A delete marker is
// also a version — it is what makes the object appear deleted — so it is listed
// alongside the real ones rather than filtered out.
type ObjectVersion struct {
	VersionID    string
	Size         int64
	LastModified *time.Time
	ETag         string
	IsLatest     bool
	IsDeleteMark bool
	StorageClass string
}

// SortVersions orders a version history newest first, which is how the browser
// presents it. Versions without a timestamp sort last; the version ID breaks
// ties so the order is stable.
func SortVersions(vs []ObjectVersion) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		switch {
		case a.LastModified == nil && b.LastModified == nil:
			return a.VersionID < b.VersionID
		case a.LastModified == nil:
			return false
		case b.LastModified == nil:
			return true
		case a.LastModified.Equal(*b.LastModified):
			return a.VersionID < b.VersionID
		default:
			return a.LastModified.After(*b.LastModified)
		}
	})
}

// ListVersions returns the version history of one exact key, newest first.
//
// ListObjectVersions is prefix-based, so it also returns every key that merely
// starts with this one; the exact-match filter is what makes it a per-object
// history. Pagination is followed to the end because a heavily-rewritten object
// can easily exceed one page.
func (m *Model) ListVersions(ctx context.Context, bucket *Object, key string) ([]ObjectVersion, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}
	if key == "" {
		return nil, fmt.Errorf("key is empty")
	}

	var out []ObjectVersion
	var keyMarker, versionMarker *string

	for {
		page, err := m.Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket:          aws.String(*bucket.Key),
			Prefix:          aws.String(key),
			KeyMarker:       keyMarker,
			VersionIdMarker: versionMarker,
		})
		if err != nil {
			return nil, err
		}

		for _, v := range page.Versions {
			if aws.ToString(v.Key) != key {
				continue
			}
			out = append(out, ObjectVersion{
				VersionID:    aws.ToString(v.VersionId),
				Size:         v.Size,
				LastModified: v.LastModified,
				ETag:         strings.Trim(aws.ToString(v.ETag), `"`),
				IsLatest:     v.IsLatest,
				StorageClass: string(v.StorageClass),
			})
		}
		for _, d := range page.DeleteMarkers {
			if aws.ToString(d.Key) != key {
				continue
			}
			out = append(out, ObjectVersion{
				VersionID:    aws.ToString(d.VersionId),
				LastModified: d.LastModified,
				IsLatest:     d.IsLatest,
				IsDeleteMark: true,
			})
		}

		if !page.IsTruncated {
			break
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}

	SortVersions(out)
	return out, nil
}

// versionCopySource builds the CopySource for a specific version: the usual
// bucket/key form with the version pinned in the query string.
func versionCopySource(bucket, key, versionID string) string {
	return copySource(bucket, key) + "?versionId=" + url.QueryEscape(versionID)
}

// RestoreVersion makes an old version current by copying it over the key. The
// history is preserved — this adds a new latest version rather than rewinding,
// which is the only non-destructive way to "go back" in a versioned bucket.
// storageClass is the restored version's class and is passed through because a
// copy that omits it silently demotes the object to STANDARD (the same trap
// PutObjectMeta guards against); empty means the endpoint reported none.
func (m *Model) RestoreVersion(ctx context.Context, bucket *Object, key, versionID, storageClass string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" || versionID == "" {
		return fmt.Errorf("key and version id are required")
	}

	in := &s3.CopyObjectInput{
		Bucket:     aws.String(*bucket.Key),
		Key:        aws.String(key),
		CopySource: aws.String(versionCopySource(*bucket.Key, key, versionID)),
	}
	if storageClass != "" {
		in.StorageClass = s3t.StorageClass(storageClass)
	}
	_, err := m.Client.CopyObject(ctx, in)
	return err
}

// DeleteVersion permanently removes one version. Unlike an ordinary delete this
// does not leave a delete marker — the data is gone.
func (m *Model) DeleteVersion(ctx context.Context, bucket *Object, key, versionID string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" || versionID == "" {
		return fmt.Errorf("key and version id are required")
	}

	_, err := m.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    aws.String(*bucket.Key),
		Key:       aws.String(key),
		VersionId: aws.String(versionID),
	})
	return err
}

// DownloadVersion writes one specific version to destPath under the given file
// name. Unlike DownloadTarget it takes an explicit destination file, because a
// version download is a single deliberate file rather than part of a tree.
func (m *Model) DownloadVersion(ctx context.Context, bucket *Object, key, versionID, destPath, fileName string) (int64, error) {
	if bucket == nil || bucket.Key == nil {
		return 0, fmt.Errorf("bucket is nil")
	}
	if err := os.MkdirAll(destPath, 0760); err != nil {
		return 0, err
	}

	target := filepath.Join(destPath, fileName)
	if _, err := os.Stat(target); err == nil {
		return 0, fmt.Errorf("%w: %s", ErrFileExists, target)
	}

	fp, err := os.Create(target)
	if err != nil {
		return 0, err
	}
	defer fp.Close()

	n, err := m.Downloader.Download(ctx, fp, &s3.GetObjectInput{
		Bucket:    aws.String(*bucket.Key),
		Key:       aws.String(key),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		_ = os.Remove(target)
		return n, err
	}
	return n, nil
}

// VersionFileName is the local name a downloaded version is saved under: the
// object's base name with a short version suffix, so several versions of the
// same object can sit in one directory without colliding.
func VersionFileName(key, versionID string) string {
	base := path.Base(key)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	short := versionID
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" || short == "null" {
		short = "null"
	}
	return fmt.Sprintf("%s.%s%s", stem, short, ext)
}
