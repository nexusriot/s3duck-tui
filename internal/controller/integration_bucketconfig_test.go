//go:build integration

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// The dashboard against a bucket that actually has things configured. Every
// summary here is a pure function over what the SDK decoded, and this is the
// only place that proves the SDK decoded what MinIO really sent.
func TestIntegrationBucketDashboardWithRealConfiguration(t *testing.T) {
	m := itModel(t)
	ctx := context.Background()
	b := itBucket(t, m, "s3duck-it-dash-real")
	itPut(t, m, b, "logs/a.txt", "x")

	if _, err := m.Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  b.Key,
		VersioningConfiguration: &s3t.VersioningConfiguration{Status: s3t.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Skipf("endpoint does not support versioning: %v", err)
	}
	if err := m.PutBucketTags(ctx, b, map[string]string{"env": "corner", "owner": "duck"}); err != nil {
		t.Logf("bucket tagging unsupported: %v", err)
	}
	// MakeBucketPublic is the app's own policy writer, so this also checks
	// that what it writes can be read back and summarised.
	if err := m.MakeBucketPublic(*b.Key); err != nil {
		t.Logf("bucket policy unsupported: %v", err)
	}
	lifecycleSet := true
	if _, err := m.Client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: b.Key,
		LifecycleConfiguration: &s3t.BucketLifecycleConfiguration{Rules: []s3t.LifecycleRule{
			{
				ID:         aws.String("expire-logs"),
				Status:     s3t.ExpirationStatusEnabled,
				Filter:     &s3t.LifecycleRuleFilterMemberPrefix{Value: "logs/"},
				Expiration: &s3t.LifecycleExpiration{Days: 30},
			},
			{
				ID:                          aws.String("tidy-versions"),
				Status:                      s3t.ExpirationStatusDisabled,
				Filter:                      &s3t.LifecycleRuleFilterMemberPrefix{Value: ""},
				NoncurrentVersionExpiration: &s3t.NoncurrentVersionExpiration{NoncurrentDays: 7},
			},
		}},
	}); err != nil {
		lifecycleSet = false
		t.Logf("lifecycle unsupported: %v", err)
	}

	info, err := m.BucketConfig(ctx, b)
	if err != nil {
		t.Fatalf("BucketConfig: %v", err)
	}
	text := rendered(bucketDashboardText(*b.Key, info))
	t.Logf("dashboard:\n%s", text)

	if info.Versioning.State != model.ConfigSet || info.Versioning.Value != "Enabled" {
		t.Errorf("versioning = %+v", info.Versioning)
	}
	if info.Tags.State == model.ConfigSet && info.TagSet["env"] != "corner" {
		t.Errorf("tags = %+v / %v", info.Tags, info.TagSet)
	}
	if info.Policy.State == model.ConfigSet {
		if !strings.Contains(info.Policy.Value, "statement") {
			t.Errorf("policy value = %q, want a statement count", info.Policy.Value)
		}
		if len(info.Policy.Detail) < 3 {
			t.Errorf("a policy should be shown in full, got %d lines", len(info.Policy.Detail))
		}
	}
	if lifecycleSet {
		if info.Lifecycle.State != model.ConfigSet {
			t.Fatalf("lifecycle = %+v", info.Lifecycle)
		}
		if info.Lifecycle.Value != "2 rule(s), 1 enabled" {
			t.Errorf("lifecycle value = %q", info.Lifecycle.Value)
		}
		joined := strings.Join(info.Lifecycle.Detail, "\n")
		for _, want := range []string{"✓ expire-logs", "prefix logs/", "expire after 30d", "✗ tidy-versions", "noncurrent expire after 7d"} {
			if !strings.Contains(joined, want) {
				t.Errorf("lifecycle detail lacks %q:\n%s", want, joined)
			}
		}
		// A rule whose scope the summary could not read would say "whole
		// bucket" for a rule that has a prefix — the quiet way to mislead.
		if strings.Contains(joined, "expire-logs") && !strings.Contains(joined, "expire-logs — prefix logs/") {
			t.Errorf("the prefixed rule lost its scope:\n%s", joined)
		}
	}
	// No section may claim a value it could not read.
	for name, item := range map[string]model.ConfigItem{
		"versioning": info.Versioning, "encryption": info.Encryption, "object lock": info.ObjectLock,
		"public access block": info.PublicAccessBlock, "tags": info.Tags, "policy": info.Policy,
		"lifecycle": info.Lifecycle, "cors": info.CORS, "website": info.Website,
		"replication": info.Replication, "notifications": info.Notifications, "logging": info.Logging,
	} {
		if item.State == model.ConfigError {
			t.Errorf("%s: unclassified error: %v", name, item.Err)
		}
	}
}
