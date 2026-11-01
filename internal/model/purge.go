package model

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Versions, recursively. An ordinary delete on a versioned bucket removes
// nothing: it writes a delete marker on top of the history, so a folder delete
// left every byte in place and a bucket could never be emptied — DeleteBucket
// failed with BucketNotEmpty however often it was asked. Purging deletes the
// stored versions themselves, by version ID, which is the only removal S3
// treats as final.

// VersionRef is one stored version, or one delete marker, of some key.
type VersionRef struct {
	Key          string
	VersionID    string
	IsDeleteMark bool
	IsLatest     bool
	Size         int64
	LastModified *time.Time
}

// ListVersionsUnder streams every version and delete marker whose key starts
// with prefix — no delimiter, so the whole subtree — handing each page to
// onPage, which returns false to stop. A page whose continuation markers do
// not move the listing forward is an error, not the end: a purge planned from
// a listing cut short would report success and leave versions behind.
func (m *Model) ListVersionsUnder(ctx context.Context, bucket *Object, prefix string, onPage func([]VersionRef) bool) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var keyMarker, versionMarker *string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := m.Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket:          aws.String(*bucket.Key),
			Prefix:          aws.String(prefix),
			KeyMarker:       keyMarker,
			VersionIdMarker: versionMarker,
		})
		if err != nil {
			return err
		}
		refs := make([]VersionRef, 0, len(page.Versions)+len(page.DeleteMarkers))
		for _, v := range page.Versions {
			refs = append(refs, VersionRef{
				Key:          aws.ToString(v.Key),
				VersionID:    aws.ToString(v.VersionId),
				IsLatest:     v.IsLatest,
				Size:         v.Size,
				LastModified: v.LastModified,
			})
		}
		for _, d := range page.DeleteMarkers {
			refs = append(refs, VersionRef{
				Key:          aws.ToString(d.Key),
				VersionID:    aws.ToString(d.VersionId),
				IsDeleteMark: true,
				IsLatest:     d.IsLatest,
				LastModified: d.LastModified,
			})
		}
		if !onPage(refs) {
			return nil
		}
		if !page.IsTruncated {
			return nil
		}
		if !versionMarkersAdvance(keyMarker, versionMarker, page.NextKeyMarker, page.NextVersionIdMarker) {
			return fmt.Errorf("listing versions under %q: the server reported more results but no new continuation marker", prefix)
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
}

// PurgeMode says which versions a purge removes.
type PurgeMode int

const (
	// PurgeAll removes every version and delete marker: the key is gone, with
	// no history to restore from.
	PurgeAll PurgeMode = iota
	// PurgeNoncurrent removes everything but the latest entry of each key —
	// the old versions and the old delete markers. What the key reads as now
	// (an object, or "deleted" when its latest entry is a marker) is kept.
	PurgeNoncurrent
)

// SelectPurge picks the refs a purge in mode removes, from a listing of
// prefix. exact restricts it to one key: ListObjectVersions is prefix-based,
// so a purge of "x" would otherwise also take "x.bak".
func SelectPurge(refs []VersionRef, mode PurgeMode, exact string) []VersionRef {
	out := make([]VersionRef, 0, len(refs))
	for _, r := range refs {
		if exact != "" && r.Key != exact {
			continue
		}
		if mode == PurgeNoncurrent && r.IsLatest {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PurgeSummary totals what a purge would remove, for its confirmation.
type PurgeSummary struct {
	Keys, Versions, Markers int
	Bytes                   int64
}

// SummarizePurge counts refs by kind. Keys is the number of distinct keys.
func SummarizePurge(refs []VersionRef) PurgeSummary {
	var s PurgeSummary
	keys := map[string]bool{}
	for _, r := range refs {
		keys[r.Key] = true
		if r.IsDeleteMark {
			s.Markers++
			continue
		}
		s.Versions++
		s.Bytes += r.Size
	}
	s.Keys = len(keys)
	return s
}

// VersionFailure is one version a purge could not remove.
type VersionFailure struct {
	Ref VersionRef
	Err error
}

// DeleteVersions permanently removes refs, by version ID, in DeleteObjects
// batches of 1000. It keeps going past per-version failures — a purge stopped
// at the first refusal would leave the rest for no reason — and returns them,
// with how many were removed. err is reserved for a batch that failed as a
// whole (and for cancellation).
func (m *Model) DeleteVersions(ctx context.Context, bucket *Object, refs []VersionRef) (deleted int, failures []VersionFailure, err error) {
	if bucket == nil || bucket.Key == nil {
		return 0, nil, fmt.Errorf("bucket is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	const maxDelete = 1000
	for i := 0; i < len(refs); i += maxDelete {
		if err := ctx.Err(); err != nil {
			return deleted, failures, err
		}
		end := i + maxDelete
		if end > len(refs) {
			end = len(refs)
		}
		batch := refs[i:end]
		ids := make([]s3t.ObjectIdentifier, 0, len(batch))
		for _, r := range batch {
			id := s3t.ObjectIdentifier{Key: aws.String(r.Key)}
			// "null" is a real version ID — the one an object written while
			// versioning was off or suspended carries — and must be sent:
			// without one the request writes a delete marker instead.
			if r.VersionID != "" {
				id.VersionId = aws.String(r.VersionID)
			}
			ids = append(ids, id)
		}
		out, err := m.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(*bucket.Key),
			Delete: &s3t.Delete{Objects: ids, Quiet: true},
		})
		if err != nil {
			return deleted, failures, err
		}
		failed := map[string]error{}
		for _, e := range out.Errors {
			failed[aws.ToString(e.Key)+"\x00"+aws.ToString(e.VersionId)] =
				fmt.Errorf("%s: %s", aws.ToString(e.Code), aws.ToString(e.Message))
		}
		for _, r := range batch {
			if ferr, ok := failed[r.Key+"\x00"+r.VersionID]; ok {
				failures = append(failures, VersionFailure{Ref: r, Err: ferr})
				continue
			}
			deleted++
		}
	}
	return deleted, failures, nil
}

// isNotImplemented reports an endpoint that does not offer a call at all —
// some S3-compatible backends have no ListObjectVersions.
func isNotImplemented(err error) bool { return classifyConfigErr(err) == ConfigUnsupported }

// IsAccessDenied reports a refusal for want of permission — the answer a key
// scoped to some buckets gets from ListBuckets.
func IsAccessDenied(err error) bool { return classifyConfigErr(err) == ConfigDenied }

// DeletedObject is a key whose latest entry is a delete marker: it no longer
// lists, but its history is still there to restore from.
type DeletedObject struct {
	Key string
	// MarkerID is the delete marker on top; removing it brings back the
	// version beneath.
	MarkerID  string
	DeletedAt *time.Time
	// Size and Versions describe what would come back: the newest real
	// version beneath the marker, and how many versions the key keeps.
	Size     int64
	Versions int
}

// FindDeleted lists the deleted keys under prefix, at most max of them (0 for
// no cap; truncated reports a cap that was hit). A listing returns each key's
// entries together, newest first, so one pass is enough: a key is deleted when
// its latest entry is a marker.
func (m *Model) FindDeleted(ctx context.Context, bucket *Object, prefix string, max int, onProgress func(scanned, found int)) (out []DeletedObject, truncated bool, err error) {
	byKey := map[string]*DeletedObject{}
	newest := map[string]*time.Time{}
	sizeOf := map[string]int64{}
	versionsOf := map[string]int{}
	scanned := 0
	err = m.ListVersionsUnder(ctx, bucket, prefix, func(page []VersionRef) bool {
		for _, r := range page {
			scanned++
			if r.IsDeleteMark && r.IsLatest {
				if _, ok := byKey[r.Key]; !ok {
					if max > 0 && len(byKey) >= max {
						truncated = true
						return false
					}
					byKey[r.Key] = &DeletedObject{Key: r.Key, MarkerID: r.VersionID, DeletedAt: r.LastModified}
				}
				continue
			}
			if r.IsDeleteMark {
				continue
			}
			// A real version: remember the newest, for the size that would
			// come back. The marker may arrive before or after it.
			if t, ok := newest[r.Key]; !ok || laterThan(r.LastModified, t) {
				newest[r.Key] = r.LastModified
				sizeOf[r.Key] = r.Size
			}
			versionsOf[r.Key]++
		}
		if onProgress != nil {
			onProgress(scanned, len(byKey))
		}
		return true
	})
	if err != nil {
		return nil, false, err
	}
	for k, d := range byKey {
		d.Size = sizeOf[k]
		d.Versions = versionsOf[k]
		out = append(out, *d)
	}
	sortDeleted(out)
	return out, truncated, nil
}

// laterThan reports a after b, a missing time being earliest.
func laterThan(a, b *time.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return a.After(*b)
}

// sortDeleted orders deleted objects most recently deleted first, by key
// after that, so the list reads as "what went missing lately".
func sortDeleted(ds []DeletedObject) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if laterThan(a.DeletedAt, b.DeletedAt) {
			return true
		}
		if laterThan(b.DeletedAt, a.DeletedAt) {
			return false
		}
		return a.Key < b.Key
	})
}

// Undelete removes the delete marker on top of a deleted key, so the version
// beneath it becomes current again. Only that marker goes: nothing else in the
// history changes, so it is as reversible as the delete was.
func (m *Model) Undelete(ctx context.Context, bucket *Object, d DeletedObject) error {
	if d.MarkerID == "" {
		return fmt.Errorf("%s has no delete marker to remove", d.Key)
	}
	return m.DeleteVersion(ctx, bucket, d.Key, d.MarkerID)
}
