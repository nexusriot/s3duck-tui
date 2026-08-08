package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// SyncEntry is one file on either side of a sync, addressed by its path
// relative to the sync root (slash-separated on both sides so local and remote
// entries compare directly).
type SyncEntry struct {
	Rel  string
	Size int64
	Mod  time.Time
}

// WalkLocal lists every regular file under root as a SyncEntry. Directories
// contribute nothing: S3 has no real directories, and empty ones would have no
// counterpart to compare against. A missing root is an error; unreadable
// entries below it abort the walk so a partial listing can never be mistaken
// for "the destination has fewer files" and trigger deletes.
func WalkLocal(root string) ([]SyncEntry, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	var out []SyncEntry
	err = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, SyncEntry{
			Rel:  filepath.ToSlash(rel),
			Size: fi.Size(),
			Mod:  fi.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRemoteEntries lists the objects under prefix as SyncEntries keyed by the
// path relative to that prefix. Folder-marker keys (ending in "/") are skipped:
// they are an encoding of a directory, not a file to transfer.
func (m *Model) ListRemoteEntries(prefix string, bucket *Object) ([]SyncEntry, error) {
	prefix = NormalizePrefix(prefix)
	objs, err := m.ListObjects(prefix, bucket)
	if err != nil {
		return nil, err
	}

	out := make([]SyncEntry, 0, len(objs))
	for _, o := range objs {
		if o.Key == nil || strings.HasSuffix(*o.Key, "/") {
			continue
		}
		rel := strings.TrimPrefix(*o.Key, prefix)
		if rel == "" {
			continue
		}
		e := SyncEntry{Rel: rel, Size: o.Size}
		if o.LastModified != nil {
			e.Mod = *o.LastModified
		}
		out = append(out, e)
	}
	return out, nil
}

// NormalizePrefix returns key in S3 prefix form: slash-separated and, unless
// empty (the bucket root), terminated with "/".
func NormalizePrefix(key string) string {
	p := filepath.ToSlash(strings.TrimSpace(key))
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// UploadFile uploads one local file to an explicit key. Model.Upload derives
// keys from a directory walk; sync already knows the exact key each file must
// land on, so it needs this narrower entry point. progressCb reports bytes
// written for this file only.
func (m *Model) UploadFile(ctx context.Context, localPath, key string, bucket *Object, progressCb func(written, total int64)) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}

	stat, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	fp, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer fp.Close()

	uploader := newUploader(m.Client)

	reader := &progressReader{
		r:     fp,
		total: stat.Size(),
		update: func(written, total int64) {
			if progressCb != nil {
				progressCb(written, total)
			}
		},
		limiter: m.Limiter,
	}

	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
		Body:   reader,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("upload canceled for %s", localPath)
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			return fmt.Errorf("upload failed for %s: %s - %s", localPath, apiErr.ErrorCode(), apiErr.ErrorMessage())
		}
		return fmt.Errorf("upload failed for %s: %w", localPath, err)
	}
	return nil
}

// DeleteKey removes a single object. Unlike Delete it never treats a trailing
// slash as a recursive prefix, so a sync delete can only ever remove the one
// file the plan named.
func (m *Model) DeleteKey(ctx context.Context, key string, bucket *Object) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == "" || strings.HasSuffix(key, "/") {
		return fmt.Errorf("refusing to delete prefix-like key %q", key)
	}
	_, err := m.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	})
	return err
}
