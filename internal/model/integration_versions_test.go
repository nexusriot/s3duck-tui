//go:build integration

package model_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// enableVersioning turns versioning on for a bucket, skipping the test when
// the endpoint does not support it.
func enableVersioning(t *testing.T, m *model.Model, bucket *model.Object) {
	t.Helper()
	_, err := m.Client.PutBucketVersioning(context.Background(), &s3.PutBucketVersioningInput{
		Bucket:                  bucket.Key,
		VersioningConfiguration: &s3t.VersioningConfiguration{Status: s3t.BucketVersioningStatusEnabled},
	})
	if err != nil {
		t.Skipf("cannot enable versioning on this endpoint: %v", err)
	}
}

func TestIntegrationVersionContentForDiff(t *testing.T) {
	// The version diff needs the *old* body, which is only reachable with a
	// VersionId — and only on a versioned bucket.
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-vdiff")
	enableVersioning(t, m, bucket)

	putObject(t, m, bucket, "conf.yaml", "a: 1\nb: 2\n")
	putObject(t, m, bucket, "conf.yaml", "a: 1\nb: 3\n")

	versions, err := m.ListVersions(ctx, bucket, "conf.yaml")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) < 2 {
		t.Skipf("endpoint reported %d versions; versioning may be unsupported", len(versions))
	}

	current, err := m.GetVersionContent(ctx, bucket, "conf.yaml", "", 1<<20)
	if err != nil {
		t.Fatalf("GetVersionContent (current): %v", err)
	}
	if string(current) != "a: 1\nb: 3\n" {
		t.Errorf("current body = %q", current)
	}

	// The older version is the one that is not latest.
	var older string
	for _, v := range versions {
		if !v.IsLatest && !v.IsDeleteMark {
			older = v.VersionID
			break
		}
	}
	if older == "" {
		t.Skip("no non-latest version to read")
	}
	old, err := m.GetVersionContent(ctx, bucket, "conf.yaml", older, 1<<20)
	if err != nil {
		t.Fatalf("GetVersionContent (old): %v", err)
	}
	if string(old) != "a: 1\nb: 2\n" {
		t.Errorf("old body = %q, want the first write", old)
	}
}
