package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ConfigState is how one part of a bucket's configuration was read. The point
// of the distinction is honesty: "none", "this server does not do that" and
// "you may not look" are three different answers, and reporting the last two
// as the first is how AccessDenied used to read as "Versioning: off".
type ConfigState int

const (
	// ConfigSet: the bucket has this configured; Value and Detail say how.
	ConfigSet ConfigState = iota
	// ConfigUnset: the bucket has none (S3's NoSuch…Configuration answer, or
	// an empty one). Value holds the display default ("off", "none").
	ConfigUnset
	// ConfigUnsupported: the endpoint does not implement the call — common
	// on MinIO and Ceph for website, logging, public access block.
	ConfigUnsupported
	// ConfigDenied: the credentials may not read it.
	ConfigDenied
	// ConfigError: anything else; Err says what.
	ConfigError
)

// ConfigItem is one section of the bucket dashboard.
type ConfigItem struct {
	State ConfigState
	// Value is the one-line answer.
	Value string
	// Detail is the rest, one display line each: rules, statements, tags.
	Detail []string
	Err    error
}

// BucketConfigInfo is a read-only snapshot of a bucket's configuration, one
// item per section of the dashboard.
type BucketConfigInfo struct {
	Region            string
	Versioning        ConfigItem
	Encryption        ConfigItem
	ObjectLock        ConfigItem
	PublicAccessBlock ConfigItem
	Tags              ConfigItem
	Policy            ConfigItem
	Lifecycle         ConfigItem
	CORS              ConfigItem
	Website           ConfigItem
	Replication       ConfigItem
	Notifications     ConfigItem
	Logging           ConfigItem

	// TagSet is the bucket's tags as read, for the tag editor; nil when they
	// could not be read.
	TagSet map[string]string
}

// notImplementedCodes are the answers of an endpoint without the feature.
var notImplementedCodes = map[string]bool{
	"NotImplemented": true, "XNotImplemented": true, "NotSupported": true,
	"MethodNotAllowed": true, "UnsupportedOperation": true,
}

// deniedCodes are the answers of a caller without the permission.
var deniedCodes = map[string]bool{
	"AccessDenied": true, "AllAccessDisabled": true, "Forbidden": true,
}

// classifyConfigErr sorts a failed configuration read. unset are the codes
// with which this particular call says "there is none" — each has its own.
func classifyConfigErr(err error, unset ...string) ConfigState {
	var api smithy.APIError
	if errors.As(err, &api) {
		code := api.ErrorCode()
		for _, u := range unset {
			if code == u {
				return ConfigUnset
			}
		}
		switch {
		case notImplementedCodes[code]:
			return ConfigUnsupported
		case deniedCodes[code]:
			return ConfigDenied
		}
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case http.StatusNotImplemented, http.StatusMethodNotAllowed:
			return ConfigUnsupported
		case http.StatusForbidden:
			return ConfigDenied
		}
	}
	return ConfigError
}

// failed builds the item for a read that did not succeed. def is the display
// default an unset configuration reads as.
func failed(err error, def string, unset ...string) ConfigItem {
	st := classifyConfigErr(err, unset...)
	item := ConfigItem{State: st}
	switch st {
	case ConfigUnset:
		item.Value = def
	case ConfigError:
		item.Err = err
	}
	return item
}

// BucketConfig reads every section of a bucket's configuration. The reads are
// independent, so they run at once — a dozen sequential round trips would make
// the dashboard take seconds to open. No section can fail the call: each one
// records its own outcome, and the dashboard shows them all.
func (m *Model) BucketConfig(ctx context.Context, bucket *Object) (BucketConfigInfo, error) {
	if bucket == nil || bucket.Key == nil {
		return BucketConfigInfo{}, fmt.Errorf("bucket is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	name := aws.String(*bucket.Key)
	info := BucketConfigInfo{Region: "us-east-1"}
	c := m.Client

	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}

	run(func() {
		if loc, err := m.GetBucketLocation(bucket.Key); err == nil && loc != nil && *loc != "" {
			info.Region = *loc
		}
	})
	run(func() {
		out, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: name})
		if err != nil {
			info.Versioning = failed(err, "off")
			return
		}
		info.Versioning = summarizeVersioning(out)
	})
	run(func() {
		out, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: name})
		if err != nil {
			info.Encryption = failed(err, "none", "ServerSideEncryptionConfigurationNotFoundError")
			return
		}
		info.Encryption = summarizeEncryption(out.ServerSideEncryptionConfiguration)
	})
	run(func() {
		out, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: name})
		if err != nil {
			info.ObjectLock = failed(err, "off", "ObjectLockConfigurationNotFoundError")
			return
		}
		info.ObjectLock = summarizeObjectLock(out.ObjectLockConfiguration)
	})
	run(func() {
		out, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: name})
		if err != nil {
			info.PublicAccessBlock = failed(err, "none at bucket level", "NoSuchPublicAccessBlockConfiguration")
			return
		}
		info.PublicAccessBlock = summarizePublicAccessBlock(out.PublicAccessBlockConfiguration)
	})
	run(func() {
		out, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: name})
		if err != nil {
			info.Tags = failed(err, "none", "NoSuchTagSet", "NoSuchTagSetError")
			if info.Tags.State == ConfigUnset {
				info.TagSet = map[string]string{}
			}
			return
		}
		info.TagSet = map[string]string{}
		for _, t := range out.TagSet {
			info.TagSet[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		info.Tags = summarizeTags(info.TagSet)
	})
	run(func() {
		out, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: name})
		if err != nil {
			info.Policy = failed(err, "none", "NoSuchBucketPolicy")
			return
		}
		info.Policy = summarizePolicy(aws.ToString(out.Policy))
	})
	run(func() {
		out, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: name})
		if err != nil {
			info.Lifecycle = failed(err, "none", "NoSuchLifecycleConfiguration")
			return
		}
		info.Lifecycle = summarizeLifecycle(out.Rules)
	})
	run(func() {
		out, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: name})
		if err != nil {
			info.CORS = failed(err, "none", "NoSuchCORSConfiguration")
			return
		}
		info.CORS = summarizeCORS(out.CORSRules)
	})
	run(func() {
		out, err := c.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{Bucket: name})
		if err != nil {
			info.Website = failed(err, "off", "NoSuchWebsiteConfiguration")
			return
		}
		info.Website = summarizeWebsite(out)
	})
	run(func() {
		out, err := c.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: name})
		if err != nil {
			info.Replication = failed(err, "none", "ReplicationConfigurationNotFoundError")
			return
		}
		info.Replication = summarizeReplication(out.ReplicationConfiguration)
	})
	run(func() {
		out, err := c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: name})
		if err != nil {
			info.Notifications = failed(err, "none")
			return
		}
		info.Notifications = summarizeNotifications(out)
	})
	run(func() {
		out, err := c.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: name})
		if err != nil {
			info.Logging = failed(err, "off")
			return
		}
		info.Logging = summarizeLogging(out.LoggingEnabled)
	})

	wg.Wait()
	return info, ctx.Err()
}

func summarizeVersioning(out *s3.GetBucketVersioningOutput) ConfigItem {
	if out == nil || out.Status == "" {
		// Never enabled: S3 answers with no status at all.
		return ConfigItem{State: ConfigUnset, Value: "off"}
	}
	item := ConfigItem{State: ConfigSet, Value: string(out.Status)}
	if out.MFADelete == s3t.MFADeleteStatusEnabled {
		item.Value += ", MFA delete on"
	}
	return item
}

func summarizeEncryption(conf *s3t.ServerSideEncryptionConfiguration) ConfigItem {
	if conf == nil || len(conf.Rules) == 0 || conf.Rules[0].ApplyServerSideEncryptionByDefault == nil {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	r := conf.Rules[0]
	d := r.ApplyServerSideEncryptionByDefault
	item := ConfigItem{State: ConfigSet, Value: string(d.SSEAlgorithm)}
	if k := aws.ToString(d.KMSMasterKeyID); k != "" {
		item.Value += ", key " + k
	}
	if r.BucketKeyEnabled {
		item.Value += ", bucket key on"
	}
	return item
}

func summarizeObjectLock(conf *s3t.ObjectLockConfiguration) ConfigItem {
	if conf == nil || conf.ObjectLockEnabled != s3t.ObjectLockEnabledEnabled {
		return ConfigItem{State: ConfigUnset, Value: "off"}
	}
	item := ConfigItem{State: ConfigSet, Value: "on"}
	if conf.Rule != nil && conf.Rule.DefaultRetention != nil {
		dr := conf.Rule.DefaultRetention
		switch {
		case dr.Days > 0:
			item.Value += fmt.Sprintf(", default retention %s %d day(s)", dr.Mode, dr.Days)
		case dr.Years > 0:
			item.Value += fmt.Sprintf(", default retention %s %d year(s)", dr.Mode, dr.Years)
		}
	}
	return item
}

func summarizePublicAccessBlock(conf *s3t.PublicAccessBlockConfiguration) ConfigItem {
	if conf == nil {
		return ConfigItem{State: ConfigUnset, Value: "none at bucket level"}
	}
	flags := []struct {
		name string
		on   bool
	}{
		{"BlockPublicAcls", conf.BlockPublicAcls},
		{"IgnorePublicAcls", conf.IgnorePublicAcls},
		{"BlockPublicPolicy", conf.BlockPublicPolicy},
		{"RestrictPublicBuckets", conf.RestrictPublicBuckets},
	}
	on := 0
	var detail []string
	for _, f := range flags {
		state := "off"
		if f.on {
			state = "on"
			on++
		}
		detail = append(detail, fmt.Sprintf("%s: %s", f.name, state))
	}
	switch on {
	case len(flags):
		return ConfigItem{State: ConfigSet, Value: "all public access blocked"}
	case 0:
		return ConfigItem{State: ConfigSet, Value: "configured, every block off", Detail: detail}
	}
	return ConfigItem{State: ConfigSet, Value: fmt.Sprintf("%d of %d blocks on", on, len(flags)), Detail: detail}
}

func summarizeTags(tags map[string]string) ConfigItem {
	if len(tags) == 0 {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	item := ConfigItem{State: ConfigSet, Value: fmt.Sprintf("%d tag(s)", len(tags))}
	for _, k := range keys {
		item.Detail = append(item.Detail, k+"="+tags[k])
	}
	return item
}

// summarizePolicy counts the statements and lays the document out indented.
// The policy is shown whole: which principal may do what is in the details,
// and a summary that dropped them would be a summary nobody can audit.
func summarizePolicy(doc string) ConfigItem {
	if strings.TrimSpace(doc) == "" {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	var parsed struct {
		Statement json.RawMessage `json:"Statement"`
	}
	value := "set"
	if json.Unmarshal([]byte(doc), &parsed) == nil {
		var list []json.RawMessage
		if json.Unmarshal(parsed.Statement, &list) == nil {
			value = fmt.Sprintf("%d statement(s)", len(list))
		} else if len(parsed.Statement) > 0 {
			value = "1 statement"
		}
	}
	var buf bytes.Buffer
	text := doc
	if json.Indent(&buf, []byte(doc), "", "  ") == nil {
		text = buf.String()
	}
	return ConfigItem{State: ConfigSet, Value: value, Detail: strings.Split(text, "\n")}
}

// lifecycleFilter renders a rule's scope.
func lifecycleFilter(r s3t.LifecycleRule) string {
	switch f := r.Filter.(type) {
	case *s3t.LifecycleRuleFilterMemberPrefix:
		if f.Value != "" {
			return "prefix " + f.Value
		}
	case *s3t.LifecycleRuleFilterMemberTag:
		return fmt.Sprintf("tag %s=%s", aws.ToString(f.Value.Key), aws.ToString(f.Value.Value))
	case *s3t.LifecycleRuleFilterMemberObjectSizeGreaterThan:
		return fmt.Sprintf("objects over %d bytes", f.Value)
	case *s3t.LifecycleRuleFilterMemberObjectSizeLessThan:
		return fmt.Sprintf("objects under %d bytes", f.Value)
	case *s3t.LifecycleRuleFilterMemberAnd:
		var parts []string
		if p := aws.ToString(f.Value.Prefix); p != "" {
			parts = append(parts, "prefix "+p)
		}
		for _, t := range f.Value.Tags {
			parts = append(parts, fmt.Sprintf("tag %s=%s", aws.ToString(t.Key), aws.ToString(t.Value)))
		}
		if f.Value.ObjectSizeGreaterThan > 0 {
			parts = append(parts, fmt.Sprintf("over %d bytes", f.Value.ObjectSizeGreaterThan))
		}
		if f.Value.ObjectSizeLessThan > 0 {
			parts = append(parts, fmt.Sprintf("under %d bytes", f.Value.ObjectSizeLessThan))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " and ")
		}
	}
	// The rule-level Prefix is deprecated for writing, but a bucket configured
	// through the old API still returns its rules in that shape.
	//lint:ignore SA1019 read-only: legacy rules still carry it
	if p := aws.ToString(r.Prefix); p != "" {
		return "prefix " + p
	}
	return "whole bucket"
}

func summarizeLifecycle(rules []s3t.LifecycleRule) ConfigItem {
	if len(rules) == 0 {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	enabled := 0
	item := ConfigItem{State: ConfigSet}
	for _, r := range rules {
		mark := "✗"
		if r.Status == s3t.ExpirationStatusEnabled {
			mark = "✓"
			enabled++
		}
		var acts []string
		for _, t := range r.Transitions {
			if t.Date != nil {
				acts = append(acts, fmt.Sprintf("→ %s on %s", t.StorageClass, t.Date.Format("2006-01-02")))
			} else {
				acts = append(acts, fmt.Sprintf("→ %s after %dd", t.StorageClass, t.Days))
			}
		}
		if e := r.Expiration; e != nil {
			switch {
			case e.Date != nil:
				acts = append(acts, "expire on "+e.Date.Format("2006-01-02"))
			case e.Days > 0:
				acts = append(acts, fmt.Sprintf("expire after %dd", e.Days))
			}
			if e.ExpiredObjectDeleteMarker {
				acts = append(acts, "remove expired delete markers")
			}
		}
		for _, t := range r.NoncurrentVersionTransitions {
			acts = append(acts, fmt.Sprintf("noncurrent → %s after %dd", t.StorageClass, t.NoncurrentDays))
		}
		if e := r.NoncurrentVersionExpiration; e != nil && e.NoncurrentDays > 0 {
			acts = append(acts, fmt.Sprintf("noncurrent expire after %dd", e.NoncurrentDays))
		}
		if a := r.AbortIncompleteMultipartUpload; a != nil && a.DaysAfterInitiation > 0 {
			acts = append(acts, fmt.Sprintf("abort incomplete uploads after %dd", a.DaysAfterInitiation))
		}
		id := aws.ToString(r.ID)
		if id == "" {
			id = "(unnamed)"
		}
		item.Detail = append(item.Detail, fmt.Sprintf("%s %s — %s: %s", mark, id, lifecycleFilter(r), strings.Join(acts, "; ")))
	}
	item.Value = fmt.Sprintf("%d rule(s), %d enabled", len(rules), enabled)
	return item
}

func summarizeCORS(rules []s3t.CORSRule) ConfigItem {
	if len(rules) == 0 {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	item := ConfigItem{State: ConfigSet, Value: fmt.Sprintf("%d rule(s)", len(rules))}
	for _, r := range rules {
		line := fmt.Sprintf("%s from %s", strings.Join(r.AllowedMethods, ", "), strings.Join(r.AllowedOrigins, ", "))
		var extra []string
		if len(r.AllowedHeaders) > 0 {
			extra = append(extra, "headers "+strings.Join(r.AllowedHeaders, ", "))
		}
		if len(r.ExposeHeaders) > 0 {
			extra = append(extra, "exposes "+strings.Join(r.ExposeHeaders, ", "))
		}
		if r.MaxAgeSeconds > 0 {
			extra = append(extra, fmt.Sprintf("max age %ds", r.MaxAgeSeconds))
		}
		if len(extra) > 0 {
			line += " (" + strings.Join(extra, "; ") + ")"
		}
		item.Detail = append(item.Detail, line)
	}
	return item
}

func summarizeWebsite(out *s3.GetBucketWebsiteOutput) ConfigItem {
	if out == nil {
		return ConfigItem{State: ConfigUnset, Value: "off"}
	}
	if r := out.RedirectAllRequestsTo; r != nil {
		target := aws.ToString(r.HostName)
		if r.Protocol != "" {
			target = string(r.Protocol) + "://" + target
		}
		return ConfigItem{State: ConfigSet, Value: "redirects every request to " + target}
	}
	var parts []string
	if out.IndexDocument != nil {
		parts = append(parts, "index "+aws.ToString(out.IndexDocument.Suffix))
	}
	if out.ErrorDocument != nil {
		parts = append(parts, "error page "+aws.ToString(out.ErrorDocument.Key))
	}
	if len(out.RoutingRules) > 0 {
		parts = append(parts, fmt.Sprintf("%d routing rule(s)", len(out.RoutingRules)))
	}
	if len(parts) == 0 {
		return ConfigItem{State: ConfigUnset, Value: "off"}
	}
	return ConfigItem{State: ConfigSet, Value: "on: " + strings.Join(parts, ", ")}
}

// replicationFilter renders a replication rule's scope.
func replicationFilter(r s3t.ReplicationRule) string {
	switch f := r.Filter.(type) {
	case *s3t.ReplicationRuleFilterMemberPrefix:
		if f.Value != "" {
			return "prefix " + f.Value
		}
	case *s3t.ReplicationRuleFilterMemberTag:
		return fmt.Sprintf("tag %s=%s", aws.ToString(f.Value.Key), aws.ToString(f.Value.Value))
	case *s3t.ReplicationRuleFilterMemberAnd:
		var parts []string
		if p := aws.ToString(f.Value.Prefix); p != "" {
			parts = append(parts, "prefix "+p)
		}
		for _, t := range f.Value.Tags {
			parts = append(parts, fmt.Sprintf("tag %s=%s", aws.ToString(t.Key), aws.ToString(t.Value)))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " and ")
		}
	}
	// Same as lifecycleFilter: V1 replication rules carry the prefix here.
	//lint:ignore SA1019 read-only: legacy rules still carry it
	if p := aws.ToString(r.Prefix); p != "" {
		return "prefix " + p
	}
	return "whole bucket"
}

func summarizeReplication(conf *s3t.ReplicationConfiguration) ConfigItem {
	if conf == nil || len(conf.Rules) == 0 {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	item := ConfigItem{State: ConfigSet, Value: fmt.Sprintf("%d rule(s)", len(conf.Rules))}
	if role := aws.ToString(conf.Role); role != "" {
		item.Value += ", role " + role
	}
	for _, r := range conf.Rules {
		mark := "✗"
		if r.Status == s3t.ReplicationRuleStatusEnabled {
			mark = "✓"
		}
		dest := "?"
		class := ""
		if r.Destination != nil {
			dest = aws.ToString(r.Destination.Bucket)
			if r.Destination.StorageClass != "" {
				class = " as " + string(r.Destination.StorageClass)
			}
		}
		id := aws.ToString(r.ID)
		if id == "" {
			id = "(unnamed)"
		}
		item.Detail = append(item.Detail, fmt.Sprintf("%s %s — %s → %s%s", mark, id, replicationFilter(r), dest, class))
	}
	return item
}

func summarizeNotifications(out *s3.GetBucketNotificationConfigurationOutput) ConfigItem {
	if out == nil {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	line := func(kind, arn string, events []s3t.Event) string {
		ev := make([]string, len(events))
		for i, e := range events {
			ev[i] = string(e)
		}
		return fmt.Sprintf("%s %s ← %s", kind, arn, strings.Join(ev, ", "))
	}
	var detail []string
	for _, t := range out.TopicConfigurations {
		detail = append(detail, line("SNS", aws.ToString(t.TopicArn), t.Events))
	}
	for _, q := range out.QueueConfigurations {
		detail = append(detail, line("SQS", aws.ToString(q.QueueArn), q.Events))
	}
	for _, l := range out.LambdaFunctionConfigurations {
		detail = append(detail, line("Lambda", aws.ToString(l.LambdaFunctionArn), l.Events))
	}
	eventBridge := out.EventBridgeConfiguration != nil
	if len(detail) == 0 && !eventBridge {
		return ConfigItem{State: ConfigUnset, Value: "none"}
	}
	value := fmt.Sprintf("%d target(s)", len(detail))
	if eventBridge {
		value += ", EventBridge on"
	}
	return ConfigItem{State: ConfigSet, Value: value, Detail: detail}
}

func summarizeLogging(le *s3t.LoggingEnabled) ConfigItem {
	if le == nil || aws.ToString(le.TargetBucket) == "" {
		return ConfigItem{State: ConfigUnset, Value: "off"}
	}
	return ConfigItem{State: ConfigSet, Value: "to s3://" + aws.ToString(le.TargetBucket) + "/" + aws.ToString(le.TargetPrefix)}
}

// PutBucketTags replaces the bucket's tag set; an empty map removes them all
// (DeleteBucketTagging — S3 refuses an empty PutBucketTagging).
func (m *Model) PutBucketTags(ctx context.Context, bucket *Object, tags map[string]string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if len(tags) == 0 {
		_, err := m.Client.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: aws.String(*bucket.Key)})
		return err
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	set := make([]s3t.Tag, 0, len(tags))
	for _, k := range keys {
		set = append(set, s3t.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	_, err := m.Client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket:  aws.String(*bucket.Key),
		Tagging: &s3t.Tagging{TagSet: set},
	})
	return err
}
