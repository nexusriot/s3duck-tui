//go:build integration

// Corner cases for this round's features, against a real endpoint.
//
// The unit tests for the preview formatter, the restore tracker and the bucket
// dashboard all feed them bytes and SDK structs built by hand. That proves the
// decisions, and proves nothing about what a server actually sends: a real
// truncated head is cut wherever the window falls, a real HEAD of a missing key
// arrives as an untyped error whose code varies by backend, and a real
// lifecycle rule may use a shape the SDK marks deprecated. These are the cases
// where hand-built fixtures and a server disagree.
package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// previewOf reads an object exactly as Preview does — same window, same
// whole-object test — and renders it.
func previewOf(t *testing.T, m *model.Model, b *model.Object, key string, size int64) previewOutput {
	t.Helper()
	window := int64(previewBytes)
	if codec, _ := compressionByName(key); codec != "" {
		window = previewCompressedBytes
	}
	data, ctype, err := m.GetObjectHead(context.Background(), b, key, window)
	if err != nil {
		t.Fatalf("GetObjectHead(%s): %v", key, err)
	}
	return renderPreview(previewInput{key: key, data: data, ctype: ctype, whole: int64(len(data)) >= size})
}

func gzipBytes(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestIntegrationPreviewCornerCases(t *testing.T) {
	m := itModel(t)
	b := itBucket(t, m, "s3duck-it-preview-corner")

	// A zero-byte object has no satisfiable range at all, so S3 answers the
	// preview's ranged GET with 416. That must read as an empty file, not as
	// an error — and not as a hexdump of nothing.
	t.Run("zero-byte object", func(t *testing.T) {
		itPut(t, m, b, "empty.txt", "")
		out := previewOf(t, m, b, "empty.txt", 0)
		if strings.Contains(out.label, "hexdump") {
			t.Errorf("an empty object should not be binary: %q", out.label)
		}
		if strings.TrimSpace(rendered(out.body)) != "" {
			t.Errorf("body = %q, want nothing", out.body)
		}
	})

	// The head of a gzip larger than the window: the decoder meets the cut
	// mid-stream, which is its normal case here, and must keep what decoded.
	t.Run("gzip larger than the window", func(t *testing.T) {
		var plain strings.Builder
		for i := 0; i < 60000; i++ {
			fmt.Fprintf(&plain, "2026-10-22T10:%02d:%02d level=info req=%08x msg=\"served\"\n", i/60%60, i%60, i*2654435761)
		}
		gz := gzipBytes(t, plain.String())
		if len(gz) <= previewCompressedBytes {
			t.Fatalf("fixture compressed to %d bytes, not larger than the %d window", len(gz), previewCompressedBytes)
		}
		itPut(t, m, b, "big.log.gz", gz)

		out := previewOf(t, m, b, "big.log.gz", int64(len(gz)))
		if !strings.HasPrefix(out.label, "gzip → ") {
			t.Fatalf("label = %q, want the decode step named", out.label)
		}
		got := rendered(out.body)
		if !strings.HasPrefix(got, plain.String()[:120]) {
			t.Errorf("decoded head does not start like the original:\n%.160q", got)
		}
		if len(got) < 10000 {
			t.Errorf("only %d bytes decoded from a %d-byte window; the head should yield far more", len(got), previewCompressedBytes)
		}
	})

	// Minified JSON past the window cannot be parsed, so it takes the
	// token-level re-indenter — the path no complete document ever reaches.
	t.Run("JSON larger than the window", func(t *testing.T) {
		var doc strings.Builder
		doc.WriteString(`[`)
		for i := 0; i < 4000; i++ {
			if i > 0 {
				doc.WriteString(",")
			}
			fmt.Fprintf(&doc, `{"id":%d,"name":"row-%d","note":"a, \"quoted\" value"}`, i, i)
		}
		doc.WriteString(`]`)
		if doc.Len() <= previewBytes {
			t.Fatalf("fixture is %d bytes, not larger than the %d window", doc.Len(), previewBytes)
		}
		itPut(t, m, b, "big.json", doc.String())

		out := previewOf(t, m, b, "big.json", int64(doc.Len()))
		if !strings.Contains(out.label, "not validated") {
			t.Errorf("label = %q, want the truncated-head wording", out.label)
		}
		got := rendered(out.body)
		if !strings.Contains(got, "\n    \"id\": 0,") {
			t.Errorf("not re-indented:\n%.300q", got)
		}
		if !strings.Contains(got, `"note": "a, \"quoted\" value"`) {
			t.Errorf("escapes inside a string were not copied verbatim:\n%.300q", got)
		}
	})

	// Real CSV, with the two things that break a naive table: a quoted field
	// holding the delimiter and a newline, and double-width runes.
	t.Run("CSV with quoted newlines and wide runes", func(t *testing.T) {
		itPut(t, m, b, "people.csv", "id,name,city\n1,\"Ann\nB\",\"Paris, FR\"\n2,日本語テスト,Oslo\n3,🦆duck,Rome\n")
		out := previewOf(t, m, b, "people.csv", -1)
		if !strings.Contains(out.label, "CSV table") {
			t.Fatalf("label = %q", out.label)
		}
		got := rendered(out.body)
		if !strings.Contains(got, "Ann↵B") {
			t.Errorf("an embedded newline must be shown, not obeyed:\n%s", got)
		}
		if !strings.Contains(got, "Paris, FR") {
			t.Errorf("a quoted delimiter must stay in its cell:\n%s", got)
		}
		lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
		if len(lines) != 5 { // header, rule, three rows
			t.Fatalf("got %d lines, want 5:\n%s", len(lines), got)
		}
		// Every row's second separator must sit in the same column, or the
		// wide runes have spilled past their cell.
		col := func(s string) int { return strings.Index(s, "│") }
		for i, l := range lines[2:] {
			if col(l) != col(lines[0]) {
				t.Errorf("row %d is misaligned (%d vs %d):\n%s", i, col(l), col(lines[0]), got)
			}
		}
	})

	// The name says JSON and the bytes say otherwise. The bytes win.
	t.Run("name lies about the content", func(t *testing.T) {
		itPut(t, m, b, "actually-binary.json", "\x00\x01\x02PK\x03\x04binary")
		out := previewOf(t, m, b, "actually-binary.json", -1)
		if !strings.Contains(out.label, "hexdump") {
			t.Errorf("label = %q, want a hexdump", out.label)
		}
	})

	// A log line that opens with a bracket is not a JSON array.
	t.Run("bracketed log lines are not JSON", func(t *testing.T) {
		itPut(t, m, b, "app.log", "[2026-10-22 10:00:00] started\n[INFO] ready\n")
		out := previewOf(t, m, b, "app.log", -1)
		if strings.Contains(out.label, "JSON") {
			t.Errorf("label = %q, want plain text", out.label)
		}
	})
}

// The restore tracker's reading of what a real backend answers. MinIO has no
// archive tier, so the interesting states here are the ones that say "stop
// waiting for this" — and the one that must never be reached by accident.
func TestIntegrationRestoreTrackerAgainstARealBackend(t *testing.T) {
	m := itModel(t)
	ctx := context.Background()
	b := itBucket(t, m, "s3duck-it-restore-corner")
	itPut(t, m, b, "plain.txt", "x")

	pending := restoreEntry{Profile: "p", Bucket: *b.Key, Key: "plain.txt", State: restorePending}
	now := time.Now()

	// A plain object that is not in an archive class: nothing to wait for.
	meta, err := m.HeadObject(ctx, b, "plain.txt")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	next, ready := nextRestoreState(pending, meta, err, now)
	if next.State != restoreNotArchived || ready {
		t.Errorf("a STANDARD object: state %q ready %v, want %q false", next.State, ready, restoreNotArchived)
	}

	// A key that is gone. This is the one that depends on the backend's error
	// shape: a HEAD carries no body, so MinIO answers with an untyped API
	// error, and model.IsNotFound has to recognise it. Getting this wrong
	// leaves the entry retrying a deleted object every minute forever.
	gone := pending
	gone.Key = "no-such-key.txt"
	meta, err = m.HeadObject(ctx, b, "no-such-key.txt")
	if err == nil {
		t.Fatal("HeadObject of a missing key returned no error")
	}
	if !model.IsNotFound(err) {
		t.Errorf("model.IsNotFound did not recognise this backend's 404: %v", err)
	}
	next, _ = nextRestoreState(gone, meta, err, now)
	if next.State != restoreGone {
		t.Errorf("a missing key: state %q, want %q (err was %v)", next.State, restoreGone, err)
	}

	// A restore request this backend cannot serve must fail loudly rather
	// than leaving the user waiting on a restore that was never accepted.
	if err := m.RestoreObject(ctx, b, "plain.txt", 1, "Standard"); err == nil {
		t.Log("note: this endpoint accepted a restore of a STANDARD object")
	} else {
		t.Logf("restore refused as expected: %v", err)
		unknown, _ := nextRestoreState(pending, model.ObjectMeta{}, err, now)
		if unknown.State != restoreUnknown || unknown.Err == "" {
			t.Errorf("a failed check: state %q err %q, want %q with the reason kept", unknown.State, unknown.Err, restoreUnknown)
		}
	}
}

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

// The local diff against bodies a server really returned.
func TestIntegrationDiffLocalCornerCases(t *testing.T) {
	m := itModel(t)
	ctx := context.Background()
	b := itBucket(t, m, "s3duck-it-diff-corner")

	t.Run("line endings only", func(t *testing.T) {
		itPut(t, m, b, "crlf.txt", "a\r\nb\r\nc\r\n")
		remote, err := m.GetVersionContent(ctx, b, "crlf.txt", "", editMaxSize)
		if err != nil {
			t.Fatal(err)
		}
		out, err := compareLocal([]byte("a\nb\nc\n"), remote, "local", "object")
		if err != nil {
			t.Fatal(err)
		}
		if out.identical {
			t.Error("CRLF vs LF is a real difference and must not be called identical")
		}
		if !strings.Contains(out.message, "line endings") {
			t.Errorf("message = %q, want it to name the line endings", out.message)
		}
	})

	t.Run("identical bytes", func(t *testing.T) {
		itPut(t, m, b, "same.txt", "a\nb\n")
		remote, err := m.GetVersionContent(ctx, b, "same.txt", "", editMaxSize)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := compareLocal([]byte("a\nb\n"), remote, "local", "object")
		if !out.identical {
			t.Errorf("round-tripped bytes should be identical: %+v", out)
		}
	})

	t.Run("object over the cap is declined", func(t *testing.T) {
		itPut(t, m, b, "huge.txt", strings.Repeat("x", editMaxSize+1024))
		if _, err := m.GetVersionContent(ctx, b, "huge.txt", "", editMaxSize); err == nil {
			t.Error("an object over the diff cap must be refused, not loaded")
		}
	})
}
