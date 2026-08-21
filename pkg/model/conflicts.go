package model

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	// conflictHeadLimit is how many candidates are probed individually with
	// HeadObject before it becomes cheaper to list the destination prefix
	// once. A rename checks one key and must not pay for a recursive listing
	// of the whole folder; a folder copy checks thousands and must not pay for
	// thousands of round trips. The probes run concurrently, so this many is
	// a few round trips rather than a few dozen.
	conflictHeadLimit = 32
	// conflictHeadWorkers bounds those concurrent probes.
	conflictHeadWorkers = 8
)

// CommonPrefix is the longest S3 prefix shared by every key, cut at a "/" so
// it is a real prefix and not half a name. Listing it is what lets one request
// answer "which of these keys exist" for a whole batch.
func CommonPrefix(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	prefix := keys[0]
	for _, k := range keys[1:] {
		for !strings.HasPrefix(k, prefix) {
			cut := strings.LastIndex(strings.TrimSuffix(prefix, "/"), "/")
			if cut < 0 {
				return ""
			}
			prefix = prefix[:cut+1]
		}
	}
	// Trim a partial final segment: "a/b.txt" as the only key would otherwise
	// be its own prefix, which is fine for listing, but "a/report" shared by
	// "a/report1" and "a/report2" is not a prefix boundary.
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		return prefix[:i+1]
	}
	return ""
}

// PlannedCopyKeys returns the destination keys a copy or move would write,
// expanding a folder into the concrete objects underneath it. It runs the
// same planFolderCopy/remapKey mapping CopyKeys does, so what the overwrite
// confirmation lists is exactly what the transfer would write.
func (m *Model) PlannedCopyKeys(srcBucket, dstBucket *Object, srcKey, dstKey string, isFolder bool) ([]string, error) {
	if srcBucket == nil || srcBucket.Key == nil || dstBucket == nil || dstBucket.Key == nil {
		return nil, errors.New("bucket is nil")
	}
	if !isFolder {
		return []string{dstKey}, nil
	}

	src, dst, err := planFolderCopy(*srcBucket.Key == *dstBucket.Key, srcKey, dstKey)
	if err != nil {
		return nil, err
	}
	objs, err := m.ListObjects(src, srcBucket)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		keys = append(keys, remapKey(src, dst, *o.Key))
	}
	return keys, nil
}

// Conflicts reports which of the candidate destination keys already hold
// objects, so a write can say what it is about to overwrite instead of doing
// it silently. A candidate ending in "/" is a folder: it conflicts when
// anything at all exists beneath it.
//
// The result is sorted, so the confirmation dialog reads the same way twice.
func (m *Model) Conflicts(ctx context.Context, bucket *Object, keys []string) ([]string, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, errors.New("bucket is nil")
	}
	if len(keys) == 0 {
		return nil, nil
	}

	var files, folders []string
	for _, k := range keys {
		switch {
		case k == "":
		case strings.HasSuffix(k, "/"):
			folders = append(folders, k)
		default:
			files = append(files, k)
		}
	}

	found, err := m.conflictingFolders(ctx, bucket, folders)
	if err != nil {
		return nil, err
	}

	// Probe a handful of keys directly; fall back to one listing only for a
	// batch large enough to be worth it. The listing is scoped to the keys'
	// CommonPrefix — which is empty when they sit at the bucket root, and
	// listing a whole bucket to check a few names would be far worse than the
	// round trips it saves.
	var fileHits []string
	switch {
	case len(files) == 0:
	case len(files) <= conflictHeadLimit:
		fileHits, err = m.conflictsByHead(ctx, bucket, files)
	default:
		fileHits, err = m.conflictsByListing(ctx, bucket, files)
	}
	if err != nil {
		return nil, err
	}

	found = append(found, fileHits...)
	sort.Strings(found)
	return found, nil
}

// conflictingFolders reports which destination prefixes already hold anything.
// One MaxKeys=1 listing answers that per folder — enumerating the prefix in
// full would be the same answer at arbitrary cost.
func (m *Model) conflictingFolders(ctx context.Context, bucket *Object, folders []string) ([]string, error) {
	var found []string
	for _, prefix := range folders {
		out, err := m.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:  aws.String(*bucket.Key),
			Prefix:  aws.String(prefix),
			MaxKeys: 1,
		})
		if err != nil {
			return nil, err
		}
		if out.KeyCount > 0 || len(out.Contents) > 0 {
			found = append(found, prefix)
		}
	}
	return found, nil
}

// conflictsByHead probes each key directly. Cheap and exact for a handful of
// keys, and it never lists a prefix that could hold a million objects. The
// probes run concurrently because they are pure latency: the user is waiting
// behind a modal for the answer.
func (m *Model) conflictsByHead(ctx context.Context, bucket *Object, keys []string) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	exists := make([]bool, len(keys))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, conflictHeadWorkers)

	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, k string) {
			defer wg.Done()
			defer func() { <-sem }()

			_, err := m.Client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(*bucket.Key),
				Key:    aws.String(k),
			})
			switch {
			case err == nil:
				exists[i] = true
			case isNotFound(err):
			default:
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}(i, k)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	var found []string
	for i, ok := range exists {
		if ok {
			found = append(found, keys[i])
		}
	}
	sort.Strings(found)
	return found, nil
}

// conflictsByListing lists the candidates' common prefix once and intersects.
// It is given plain object keys only; folder candidates are answered by
// conflictingFolders.
func (m *Model) conflictsByListing(ctx context.Context, bucket *Object, keys []string) ([]string, error) {
	objs, err := m.ListObjects(CommonPrefix(keys), bucket)
	if err != nil {
		return nil, err
	}

	existing := make(map[string]bool, len(objs))
	for _, o := range objs {
		if o.Key != nil {
			existing[*o.Key] = true
		}
	}

	var found []string
	for _, k := range keys {
		if existing[k] {
			found = append(found, k)
		}
	}
	sort.Strings(found)
	return found, nil
}

// isNotFound reports whether an error is S3's "this key does not exist".
//
// It has to be generous. A HEAD response carries no body, so there is nothing
// for the SDK to parse an error shape out of: against MinIO a missing key
// arrives as an untyped API error with code "NotFound", not as *s3t.NotFound.
// Since a failed conflict scan deliberately blocks the write, missing one of
// these spellings would turn "the destination is free" into "the operation
// cannot proceed" for every rename and copy on that backend.
func isNotFound(err error) bool {
	var nf *s3t.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *s3t.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}
