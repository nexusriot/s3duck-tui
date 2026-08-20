package model

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// conflictHeadLimit is how many candidates are probed individually with
// HeadObject before it becomes cheaper to list the destination prefix once.
// A rename checks one key and must not pay for a recursive listing of the
// whole folder; a folder copy checks thousands and must not pay for thousands
// of round trips.
const conflictHeadLimit = 8

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

	folders := false
	for _, k := range keys {
		if strings.HasSuffix(k, "/") {
			folders = true
			break
		}
	}

	if len(keys) <= conflictHeadLimit && !folders {
		return m.conflictsByHead(ctx, bucket, keys)
	}
	return m.conflictsByListing(ctx, bucket, keys)
}

// conflictsByHead probes each key directly. Cheap and exact for a handful of
// keys, and it never lists a prefix that could hold a million objects.
func (m *Model) conflictsByHead(ctx context.Context, bucket *Object, keys []string) ([]string, error) {
	var found []string
	for _, k := range keys {
		if k == "" {
			continue
		}
		_, err := m.Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(*bucket.Key),
			Key:    aws.String(k),
		})
		if err == nil {
			found = append(found, k)
			continue
		}
		if isNotFound(err) {
			continue
		}
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// conflictsByListing lists the candidates' common prefix once and intersects.
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
		if k == "" {
			continue
		}
		if !strings.HasSuffix(k, "/") {
			if existing[k] {
				found = append(found, k)
			}
			continue
		}
		// A folder candidate conflicts when the destination prefix already
		// holds anything: those objects are what a recursive copy would
		// overwrite name by name.
		for e := range existing {
			if strings.HasPrefix(e, k) {
				found = append(found, k)
				break
			}
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
