package model

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

func statusErr(code int) error {
	return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
		Err:      errors.New("status"),
	}}
}

func TestClassifyConfigErr(t *testing.T) {
	cases := []struct {
		err   error
		unset []string
		want  ConfigState
	}{
		{apiErr("NoSuchBucketPolicy"), []string{"NoSuchBucketPolicy"}, ConfigUnset},
		// The same code means nothing for a call that does not list it.
		{apiErr("NoSuchBucketPolicy"), nil, ConfigError},
		{apiErr("NotImplemented"), nil, ConfigUnsupported},
		{apiErr("XNotImplemented"), nil, ConfigUnsupported},
		{apiErr("AccessDenied"), []string{"NoSuchTagSet"}, ConfigDenied},
		{statusErr(http.StatusNotImplemented), nil, ConfigUnsupported},
		{statusErr(http.StatusForbidden), nil, ConfigDenied},
		{errors.New("dial tcp: timeout"), nil, ConfigError},
	}
	for _, c := range cases {
		if got := classifyConfigErr(c.err, c.unset...); got != c.want {
			t.Errorf("classifyConfigErr(%v, %v) = %d, want %d", c.err, c.unset, got, c.want)
		}
	}
}

func TestFailedKeepsTheReason(t *testing.T) {
	// The bug this replaces: AccessDenied read as "off".
	if item := failed(apiErr("AccessDenied"), "off"); item.State != ConfigDenied || item.Value != "" {
		t.Errorf("denied read = %+v, want ConfigDenied with no value", item)
	}
	if item := failed(apiErr("NoSuchCORSConfiguration"), "none", "NoSuchCORSConfiguration"); item.State != ConfigUnset || item.Value != "none" {
		t.Errorf("unset read = %+v", item)
	}
	if item := failed(errors.New("boom"), "off"); item.State != ConfigError || item.Err == nil {
		t.Errorf("error read = %+v, want the error kept", item)
	}
}

func TestSummarizeVersioningAndEncryption(t *testing.T) {
	if v := summarizeVersioning(&s3.GetBucketVersioningOutput{}); v.State != ConfigUnset || v.Value != "off" {
		t.Errorf("never enabled: %+v", v)
	}
	v := summarizeVersioning(&s3.GetBucketVersioningOutput{Status: s3t.BucketVersioningStatusSuspended, MFADelete: s3t.MFADeleteStatusEnabled})
	if v.Value != "Suspended, MFA delete on" {
		t.Errorf("suspended: %+v", v)
	}
	e := summarizeEncryption(&s3t.ServerSideEncryptionConfiguration{Rules: []s3t.ServerSideEncryptionRule{{
		ApplyServerSideEncryptionByDefault: &s3t.ServerSideEncryptionByDefault{SSEAlgorithm: s3t.ServerSideEncryptionAwsKms, KMSMasterKeyID: aws.String("k1")},
		BucketKeyEnabled:                   true,
	}}})
	if e.Value != "aws:kms, key k1, bucket key on" {
		t.Errorf("kms: %+v", e)
	}
}

func TestSummarizePublicAccessBlock(t *testing.T) {
	all := summarizePublicAccessBlock(&s3t.PublicAccessBlockConfiguration{BlockPublicAcls: true, IgnorePublicAcls: true, BlockPublicPolicy: true, RestrictPublicBuckets: true})
	if all.Value != "all public access blocked" || len(all.Detail) != 0 {
		t.Errorf("all on: %+v", all)
	}
	some := summarizePublicAccessBlock(&s3t.PublicAccessBlockConfiguration{BlockPublicAcls: true})
	if some.Value != "1 of 4 blocks on" || len(some.Detail) != 4 || !strings.Contains(strings.Join(some.Detail, " "), "BlockPublicPolicy: off") {
		t.Errorf("partial: %+v", some)
	}
}

func TestSummarizePolicyCountsStatements(t *testing.T) {
	p := summarizePolicy(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow"},{"Effect":"Deny"}]}`)
	if p.Value != "2 statement(s)" || len(p.Detail) < 5 {
		t.Errorf("policy: %+v", p)
	}
	single := summarizePolicy(`{"Statement":{"Effect":"Allow"}}`)
	if single.Value != "1 statement" {
		t.Errorf("a single statement object: %+v", single)
	}
	if bad := summarizePolicy("not json"); bad.Value != "set" || bad.Detail[0] != "not json" {
		t.Errorf("unparseable policy should still be shown verbatim: %+v", bad)
	}
}

func TestSummarizeLifecycle(t *testing.T) {
	rules := []s3t.LifecycleRule{
		{
			ID:          aws.String("logs"),
			Status:      s3t.ExpirationStatusEnabled,
			Filter:      &s3t.LifecycleRuleFilterMemberPrefix{Value: "logs/"},
			Transitions: []s3t.Transition{{Days: 30, StorageClass: s3t.TransitionStorageClassGlacier}},
			Expiration:  &s3t.LifecycleExpiration{Days: 365},
		},
		{
			Status:                         s3t.ExpirationStatusDisabled,
			Filter:                         &s3t.LifecycleRuleFilterMemberAnd{Value: s3t.LifecycleRuleAndOperator{Prefix: aws.String("tmp/"), Tags: []s3t.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}},
			AbortIncompleteMultipartUpload: &s3t.AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
		},
	}
	got := summarizeLifecycle(rules)
	if got.Value != "2 rule(s), 1 enabled" {
		t.Errorf("value = %q", got.Value)
	}
	if want := "✓ logs — prefix logs/: → GLACIER after 30d; expire after 365d"; got.Detail[0] != want {
		t.Errorf("rule 1 = %q, want %q", got.Detail[0], want)
	}
	if want := "✗ (unnamed) — prefix tmp/ and tag k=v: abort incomplete uploads after 7d"; got.Detail[1] != want {
		t.Errorf("rule 2 = %q, want %q", got.Detail[1], want)
	}
	if none := summarizeLifecycle(nil); none.State != ConfigUnset {
		t.Errorf("no rules: %+v", none)
	}
}

func TestSummarizeCORSWebsiteReplicationNotificationsLogging(t *testing.T) {
	cors := summarizeCORS([]s3t.CORSRule{{AllowedMethods: []string{"GET", "PUT"}, AllowedOrigins: []string{"https://a.example"}, MaxAgeSeconds: 600}})
	if cors.Detail[0] != "GET, PUT from https://a.example (max age 600s)" {
		t.Errorf("cors: %+v", cors)
	}

	site := summarizeWebsite(&s3.GetBucketWebsiteOutput{IndexDocument: &s3t.IndexDocument{Suffix: aws.String("index.html")}, ErrorDocument: &s3t.ErrorDocument{Key: aws.String("404.html")}})
	if site.Value != "on: index index.html, error page 404.html" {
		t.Errorf("website: %+v", site)
	}
	redirect := summarizeWebsite(&s3.GetBucketWebsiteOutput{RedirectAllRequestsTo: &s3t.RedirectAllRequestsTo{HostName: aws.String("example.com"), Protocol: s3t.ProtocolHttps}})
	if redirect.Value != "redirects every request to https://example.com" {
		t.Errorf("redirect: %+v", redirect)
	}

	rep := summarizeReplication(&s3t.ReplicationConfiguration{Role: aws.String("arn:role"), Rules: []s3t.ReplicationRule{{
		ID: aws.String("r1"), Status: s3t.ReplicationRuleStatusEnabled,
		Filter:      &s3t.ReplicationRuleFilterMemberPrefix{Value: "img/"},
		Destination: &s3t.Destination{Bucket: aws.String("arn:aws:s3:::dst"), StorageClass: s3t.StorageClassStandardIa},
	}}})
	if rep.Value != "1 rule(s), role arn:role" || rep.Detail[0] != "✓ r1 — prefix img/ → arn:aws:s3:::dst as STANDARD_IA" {
		t.Errorf("replication: %+v", rep)
	}

	n := summarizeNotifications(&s3.GetBucketNotificationConfigurationOutput{
		QueueConfigurations: []s3t.QueueConfiguration{{QueueArn: aws.String("arn:q"), Events: []s3t.Event{"s3:ObjectCreated:*"}}},
	})
	if n.Value != "1 target(s)" || n.Detail[0] != "SQS arn:q ← s3:ObjectCreated:*" {
		t.Errorf("notifications: %+v", n)
	}
	if empty := summarizeNotifications(&s3.GetBucketNotificationConfigurationOutput{}); empty.State != ConfigUnset {
		t.Errorf("no targets: %+v", empty)
	}

	if l := summarizeLogging(&s3t.LoggingEnabled{TargetBucket: aws.String("logs"), TargetPrefix: aws.String("b/")}); l.Value != "to s3://logs/b/" {
		t.Errorf("logging: %+v", l)
	}
	if l := summarizeLogging(nil); l.State != ConfigUnset || l.Value != "off" {
		t.Errorf("logging off: %+v", l)
	}
}
