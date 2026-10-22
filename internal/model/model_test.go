package model

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// PresignGetURL is pure local crypto (no network), so it can be exercised
// against a model built from a static config.
// newTestModel builds a Model for tests, failing the test on config errors.
func newTestModel(t *testing.T, cf Config) *Model {
	t.Helper()
	m, err := NewModel(cf)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	return m
}

func TestPresignGetURL(t *testing.T) {
	region := "us-east-1"
	m := newTestModel(t, NewConfig("https://s3.example.com", &region, "ak", "sk", "", true, 0))
	bucket := &Object{Key: strPtr("my-bucket")}

	link, err := m.PresignGetURL(bucket, "dir/file.txt", time.Hour)
	if err != nil {
		t.Fatalf("PresignGetURL: %v", err)
	}

	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("returned link is not a URL: %v", err)
	}
	if !strings.Contains(u.Path, "my-bucket") || !strings.Contains(u.Path, "file.txt") {
		t.Errorf("link path missing bucket/key: %q", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Errorf("link not signed: %q", link)
	}
	if q.Get("X-Amz-Expires") != "3600" {
		t.Errorf("X-Amz-Expires = %q, want 3600", q.Get("X-Amz-Expires"))
	}
}

func TestPresignGetURLCapsTTL(t *testing.T) {
	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	bucket := &Object{Key: strPtr("b")}

	link, err := m.PresignGetURL(bucket, "k", PresignMaxTTL+time.Hour)
	if err != nil {
		t.Fatalf("PresignGetURL: %v", err)
	}
	u, _ := url.Parse(link)
	want := strconv.Itoa(int(PresignMaxTTL.Seconds()))
	if got := u.Query().Get("X-Amz-Expires"); got != want {
		t.Errorf("X-Amz-Expires = %q, want %s (capped)", got, want)
	}
}

func TestPresignGetURLValidation(t *testing.T) {
	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	bucket := &Object{Key: strPtr("b")}

	if _, err := m.PresignGetURL(nil, "k", time.Hour); err == nil {
		t.Error("nil bucket: want error")
	}
	if _, err := m.PresignGetURL(bucket, "", time.Hour); err == nil {
		t.Error("empty key: want error")
	}
	if _, err := m.PresignGetURL(bucket, "k", 0); err == nil {
		t.Error("zero ttl: want error")
	}
}

func strPtr(s string) *string { return &s }

func TestNewConfig(t *testing.T) {
	region := "eu-west-1"
	c := NewConfig("https://example.com", &region, "ak", "sk", "tok", true, 1024)

	if c.Url != "https://example.com" {
		t.Errorf("Url = %q", c.Url)
	}
	if c.Region == nil || *c.Region != region {
		t.Errorf("Region = %v, want %q", c.Region, region)
	}
	if c.AccessKey != "ak" || c.SecretKey != "sk" {
		t.Errorf("keys not mapped: %+v", c)
	}
	if !c.SSl {
		t.Errorf("SSl = false, want true")
	}
	if c.MaxBytesPerSec != 1024 {
		t.Errorf("MaxBytesPerSec = %d, want 1024", c.MaxBytesPerSec)
	}

	c2 := NewConfig("u", nil, "", "", "", false, 0)
	if c2.Region != nil {
		t.Errorf("Region = %v, want nil", c2.Region)
	}
}

func TestThrottleStep(t *testing.T) {
	tests := []struct {
		name      string
		rate      float64
		tokens    float64
		elapsed   time.Duration
		n         int
		wantSleep time.Duration
		wantToks  float64
	}{
		{"unlimited passes through", 0, 5, time.Second, 100, 0, 5},
		{"enough tokens, no sleep", 1000, 1000, 0, 100, 0, 900},
		{"exact drain, no sleep", 1000, 100, 0, 100, 0, 0},
		{"burst capped at one second", 1000, 500, 100 * time.Second, 0, 0, 1000},
		{"deficit sleeps and goes negative", 1000, 0, 0, 500, 500 * time.Millisecond, -500},
		{"refill pays prior debt", 1000, -200, time.Second, 300, 0, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sleep, toks := throttleStep(tt.rate, tt.tokens, tt.elapsed, tt.n)
			if sleep != tt.wantSleep {
				t.Errorf("sleep = %v, want %v", sleep, tt.wantSleep)
			}
			if toks != tt.wantToks {
				t.Errorf("tokens = %v, want %v", toks, tt.wantToks)
			}
		})
	}
}

func TestNewRateLimiter(t *testing.T) {
	if newRateLimiter(0) != nil {
		t.Errorf("newRateLimiter(0) should be nil (unlimited)")
	}
	if newRateLimiter(-5) != nil {
		t.Errorf("newRateLimiter(negative) should be nil (unlimited)")
	}
	if l := newRateLimiter(1000); l == nil || l.rate != 1000 {
		t.Errorf("newRateLimiter(1000) = %v, want rate 1000", l)
	}
	// A nil limiter's wait must be a safe no-op (the unlimited path).
	var nilL *rateLimiter
	_ = nilL.wait(context.Background(), 100)
}

func TestRateLimiterWait(t *testing.T) {
	l := newRateLimiter(1000) // 1000 B/s, starts with a 1000-token burst

	// Spending the burst must not block.
	start := time.Now()
	_ = l.wait(context.Background(), 1000)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("spending the initial burst blocked for %v", d)
	}

	// With the bucket drained, moving another 500 bytes must throttle ~0.5s.
	start = time.Now()
	_ = l.wait(context.Background(), 500)
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("wait did not throttle after draining: slept %v, want >= ~0.5s", d)
	}

	// n <= 0 is always a no-op.
	start = time.Now()
	_ = l.wait(context.Background(), 0)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("wait(0) blocked for %v", d)
	}
}

// A cancel must cut the throttle's sleep short: one 5 MiB chunk at a low cap
// owes tens of seconds, and Cancel used to wait all of them out.
func TestRateLimiterWaitAnswersCancel(t *testing.T) {
	l := newRateLimiter(1000)
	_ = l.wait(context.Background(), 1000) // drain the burst

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := l.wait(ctx, 60_000) // a minute's worth at 1000 B/s
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a cancelled wait slept %v", d)
	}
	if err == nil {
		t.Error("a cancelled wait must report the cancellation")
	}
}

// PrepareUpload does not touch the S3 client or the bucket argument, so it is
// exercised here purely against the local filesystem.
//
// For a directory, keys are relative to the PARENT of localPath — the uploaded
// tree keeps its top-level directory name, matching model.Upload's derivation
// exactly (the two used to diverge, which made the preview promise
// destinations the objects never landed on).
func TestPrepareUploadSingleFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "hello.txt")
	content := []byte("hello world")
	if err := os.WriteFile(fp, content, 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("no prefix", func(t *testing.T) {
		targets, total, err := (&Model{}).PrepareUpload(fp, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 {
			t.Fatalf("len(targets) = %d, want 1", len(targets))
		}
		if targets[0].RemotePath != "hello.txt" {
			t.Errorf("RemotePath = %q, want %q", targets[0].RemotePath, "hello.txt")
		}
		if targets[0].Size != int64(len(content)) || total != int64(len(content)) {
			t.Errorf("size/total = %d/%d, want %d", targets[0].Size, total, len(content))
		}
	})

	t.Run("with prefix normalization", func(t *testing.T) {
		targets, _, err := (&Model{}).PrepareUpload(fp, "data", nil)
		if err != nil {
			t.Fatal(err)
		}
		if targets[0].RemotePath != "data/hello.txt" {
			t.Errorf("RemotePath = %q, want %q", targets[0].RemotePath, "data/hello.txt")
		}
	})
}

func TestPrepareUploadDirectory(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.txt", []byte("aa"))              // size 2
	mustWrite("sub/b.txt", []byte("bbbb"))        // size 4
	mustWrite("sub/deep/c.txt", []byte("cccccc")) // size 6

	targets, total, err := (&Model{}).PrepareUpload(root, "pre", nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 {
		t.Errorf("total = %d, want 12", total)
	}

	got := make(map[string]int64, len(targets))
	for _, tg := range targets {
		got[tg.RemotePath] = tg.Size
	}
	base := filepath.Base(root)
	want := map[string]int64{
		"pre/" + base + "/a.txt":          2,
		"pre/" + base + "/sub/b.txt":      4,
		"pre/" + base + "/sub/deep/c.txt": 6,
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want keys %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("RemotePath %q size = %d, want %d", k, got[k], v)
		}
	}
}

func TestPrepareUploadMissingPath(t *testing.T) {
	_, _, err := (&Model{}).PrepareUpload(filepath.Join(t.TempDir(), "nope"), "", nil)
	if err == nil {
		t.Errorf("expected error for non-existent path, got nil")
	}
}

func TestPlanFolderCopy(t *testing.T) {
	tests := []struct {
		name       string
		sameBucket bool
		src, dst   string
		wantSrc    string
		wantDst    string
		wantErr    bool
	}{
		{"normalizes missing slashes", true, "a", "b", "a/", "b/", false},
		{"keeps existing slashes", true, "a/", "b/", "a/", "b/", false},
		{"same-bucket identical prefix rejected", true, "a", "a", "", "", true},
		{"same-bucket into own subtree rejected", true, "a", "a/b", "", "", true},
		{"cross-bucket identical prefix allowed", false, "a", "a", "a/", "a/", false},
		{"cross-bucket into same-named subtree allowed", false, "a", "a/b", "a/", "a/b/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dst, err := planFolderCopy(tt.sameBucket, tt.src, tt.dst)
			if (err != nil) != tt.wantErr {
				t.Fatalf("planFolderCopy err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if src != tt.wantSrc || dst != tt.wantDst {
				t.Errorf("planFolderCopy = (%q,%q), want (%q,%q)", src, dst, tt.wantSrc, tt.wantDst)
			}
		})
	}
}

func TestRemapKey(t *testing.T) {
	cases := []struct {
		src, dst, key, want string
	}{
		{"photos/", "backup/", "photos/2024/x.jpg", "backup/2024/x.jpg"},
		{"photos/", "backup/", "photos/", "backup/"},
		{"a/", "b/c/", "a/deep/f", "b/c/deep/f"},
	}
	for _, tt := range cases {
		if got := remapKey(tt.src, tt.dst, tt.key); got != tt.want {
			t.Errorf("remapKey(%q,%q,%q) = %q, want %q", tt.src, tt.dst, tt.key, got, tt.want)
		}
	}
}

// CopyObject's guard clauses return before any network call, so they're
// exercised against a client-less model. (The success path issues a real
// CopyObject and so is out of scope for a unit test — a cross-bucket copy of an
// identical key is legitimate and is covered indirectly by the CopyKeys/
// planFolderCopy tests.)
func TestCopyObjectValidation(t *testing.T) {
	m := &Model{}
	b := &Object{Key: strPtr("bucket")}

	if err := m.CopyObject(context.TODO(), nil, b, "a", "b"); err == nil {
		t.Error("nil source bucket: want error")
	}
	if err := m.CopyObject(context.TODO(), b, nil, "a", "b"); err == nil {
		t.Error("nil dest bucket: want error")
	}
	if err := m.CopyObject(context.TODO(), b, b, "same", "same"); err == nil {
		t.Error("same bucket + same key: want error")
	}
}

func TestCopyKeysNilBuckets(t *testing.T) {
	m := &Model{}
	b := &Object{Key: strPtr("bucket")}
	if _, err := m.CopyKeys(context.TODO(), nil, b, "a", "b", false, nil, nil); err == nil {
		t.Error("nil source bucket: want error")
	}
	if _, err := m.CopyKeys(context.TODO(), b, nil, "a", "b", false, nil, nil); err == nil {
		t.Error("nil dest bucket: want error")
	}
}

func TestNormalizeBucketLocation(t *testing.T) {
	cases := map[string]string{
		"":           "us-east-1", // S3 reports the classic region as empty
		"EU":         "eu-west-1", // pre-2009 Ireland buckets use the legacy alias
		"eu-west-1":  "eu-west-1",
		"ap-south-1": "ap-south-1",
	}
	for in, want := range cases {
		if got := normalizeBucketLocation(in); got != want {
			t.Errorf("normalizeBucketLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCopySourceEscapesPlus(t *testing.T) {
	// S3 decodes x-amz-copy-source query-style: a literal "+" means a space,
	// so it must be sent as %2B or "report+final.pdf" is looked up as
	// "report final.pdf".
	got := copySource("b", "dir/report+final.pdf")
	if strings.Contains(got, "+") {
		t.Errorf("unescaped + in copy source: %q", got)
	}
	if !strings.Contains(got, "%2B") {
		t.Errorf("+ should be %%2B: %q", got)
	}
	// Spaces and unicode still escape as before.
	got = copySource("b", "a b/ü.txt")
	if strings.Contains(got, " ") {
		t.Errorf("unescaped space: %q", got)
	}
}

func TestSafeLocalPath(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dl")

	// Keys whose cleaned path escapes the destination are rejected — S3 keys
	// may legally contain ".." segments (zip-slip).
	for _, key := range []string{
		"../evil.txt",
		"photos/../../../etc/cron.d/x",
		"a/../..",
		"../outside/",
	} {
		if got, err := SafeLocalPath(dest, "", key); err == nil {
			t.Errorf("SafeLocalPath(%q) = %q, want traversal error", key, got)
		}
	}

	// Prefix stripping and containment for honest keys.
	got, err := SafeLocalPath(dest, "docs/", "docs/sub/a.txt")
	if err != nil {
		t.Fatalf("SafeLocalPath: %v", err)
	}
	if want := filepath.Join(dest, "sub", "a.txt"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Inner dot segments that stay local are allowed.
	if _, err := SafeLocalPath(dest, "", "a/../b.txt"); err != nil {
		t.Errorf("contained dot segments should pass: %v", err)
	}

	// A key equal to the prefix maps to the destination itself (folder marker).
	got, err = SafeLocalPath(dest, "docs/", "docs/")
	if err != nil || got != filepath.Clean(dest) {
		t.Errorf("marker at prefix: got %q, %v", got, err)
	}
}

// TestWalkFollowingRootSymlink pins the symlinked-root behavior: filepath.Walk
// lstats its root, so a symlinked directory used to "walk" to one skipped
// symlink entry — an upload that transfers nothing, or a sync listing that
// plans deleting the whole destination.
func TestWalkFollowingRootSymlink(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"a.txt": "one", "sub/b.txt": "two"} {
		if err := os.WriteFile(filepath.Join(real, filepath.FromSlash(rel)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "proj")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	t.Run("WalkLocal sees the files through the link", func(t *testing.T) {
		entries, err := WalkLocal(link)
		if err != nil {
			t.Fatalf("WalkLocal: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("walked %d entries through a symlinked root, want 2", len(entries))
		}
	})

	t.Run("PrepareUpload keys keep the link's own name", func(t *testing.T) {
		m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
		targets, total, err := m.PrepareUpload(link, "up/", &Object{Key: strPtr("b"), Ot: Bucket})
		if err != nil {
			t.Fatalf("PrepareUpload: %v", err)
		}
		if len(targets) != 2 || total != int64(len("one")+len("two")) {
			t.Fatalf("targets = %d (total %d), want 2 files through the symlinked root", len(targets), total)
		}
		for _, tg := range targets {
			if !strings.HasPrefix(tg.RemotePath, "up/proj/") {
				t.Errorf("remote key %q, want it under up/proj/ (the symlink's name, not the target's)", tg.RemotePath)
			}
		}
	})

	t.Run("a broken symlink root errors instead of walking empty", func(t *testing.T) {
		dangling := filepath.Join(t.TempDir(), "gone")
		if err := os.Symlink(filepath.Join(t.TempDir(), "never"), dangling); err != nil {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		if _, err := WalkLocal(dangling); err == nil {
			t.Error("want an error for a dangling symlink root")
		}
	})
}

// TestPrepareUploadListsEmptyDirectories pins the folder markers. Upload sends
// exactly PrepareUpload's targets (there is no second key derivation to drift
// from the preview), so an empty directory must be in the list as a "/" key or
// the upload would silently stop creating it.
func TestPrepareUploadListsEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "proj")
	for _, d := range []string{"sub", "empty", "nested/inner"} {
		if err := os.MkdirAll(filepath.Join(tree, filepath.FromSlash(d)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}

	targets, total, err := (&Model{}).PrepareUpload(tree, "dest", nil)
	if err != nil {
		t.Fatalf("PrepareUpload: %v", err)
	}
	got := map[string]bool{}
	for _, tg := range targets {
		got[tg.RemotePath] = true
	}
	want := []string{"dest/proj/sub/b.txt", "dest/proj/empty/", "dest/proj/nested/inner/"}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("missing target %q in %v", k, got)
		}
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (markers carry no bytes)", total)
	}

	// An empty root is one marker: that is the whole upload.
	vacant := filepath.Join(root, "vacant")
	if err := os.Mkdir(vacant, 0755); err != nil {
		t.Fatal(err)
	}
	targets, _, err = (&Model{}).PrepareUpload(vacant, "", nil)
	if err != nil || len(targets) != 1 || targets[0].RemotePath != "vacant/" {
		t.Errorf("empty root targets = %+v, %v; want one vacant/ marker", targets, err)
	}
}

// A same-bucket folder copy whose destination is an ancestor of the source is
// fine — unless the source holds the sub-path that maps onto itself. Then the
// first copy overwrites a source object before it is read, and a move deletes
// the original without it having been written anywhere.
func TestCheckSelfOverlap(t *testing.T) {
	objs := func(keys ...string) []s3t.Object {
		out := make([]s3t.Object, 0, len(keys))
		for _, k := range keys {
			out = append(out, s3t.Object{Key: aws.String(k)})
		}
		return out
	}
	if err := checkSelfOverlap(true, "a/a/", "a/", objs("a/a/f", "a/a/a/f")); err == nil {
		t.Error("a/a/a/f → a/a/f lands on a source object; want a refusal")
	}
	if err := checkSelfOverlap(true, "x/sub/", "x/", objs("x/sub/f", "x/sub/g/h")); err != nil {
		t.Errorf("moving a folder up into its parent with no self-mapping sub-path: %v", err)
	}
	if err := checkSelfOverlap(false, "a/a/", "a/", objs("a/a/f", "a/a/a/f")); err != nil {
		t.Errorf("a different bucket cannot overlap: %v", err)
	}
	if err := checkSelfOverlap(true, "a/", "b/", objs("a/f")); err != nil {
		t.Errorf("disjoint prefixes cannot overlap: %v", err)
	}
}

// The uploader's body is never seekable, so the SDK cannot grow the part size
// itself; at a fixed 5 MiB an upload over 48.8 GiB failed on its last part.
func TestUploadPartSizeFitsThePartLimit(t *testing.T) {
	const mib = int64(1 << 20)
	for _, size := range []int64{0, -1, 1, 5 * mib, 10000 * 5 * mib, 10000*5*mib + 1, 60 << 30, 5 << 40} {
		ps := uploadPartSize(size)
		if ps < 5*mib {
			t.Errorf("size %d: part %d is below the 5 MiB minimum", size, ps)
		}
		if size > 0 && (size+ps-1)/ps > 10000 {
			t.Errorf("size %d: part %d needs %d parts, over 10 000", size, ps, (size+ps-1)/ps)
		}
	}
}

// A copy that sends no encryption takes the destination bucket's default, so
// the profile's SSE has to be sent on every copy, and — without one — a copy
// within the bucket keeps the source's.
func TestCopyEncryption(t *testing.T) {
	same := copySpec{srcBucket: "b", dstBucket: "b"}
	other := copySpec{srcBucket: "b", dstBucket: "c"}
	kms := &ObjectMeta{SSE: "aws:kms", SSEKMSKeyID: "arn:key"}

	m := &Model{Cf: &Config{Write: WriteOptions{SSE: "AES256"}}}
	if sse, _ := m.copyEncryption(same, kms); sse != "AES256" {
		t.Errorf("profile SSE = %q, want the profile's AES256 to win", sse)
	}

	m = &Model{Cf: &Config{}}
	if sse, key := m.copyEncryption(same, kms); sse != "aws:kms" || key != "arn:key" {
		t.Errorf("in-bucket copy = %q/%q, want the source's KMS encryption", sse, key)
	}
	if sse, _ := m.copyEncryption(other, kms); sse != "" {
		t.Errorf("cross-bucket copy = %q, want the destination bucket's default", sse)
	}
	if sse, _ := m.copyEncryption(same, nil); sse != "" {
		t.Errorf("unknown source = %q, want the bucket default", sse)
	}
}

// Join cleans "./" away and a root already ends in a separator, so a string
// prefix test rejected every key when downloading into "." or "/".
func TestSafeLocalPathAcceptsDotAndRoot(t *testing.T) {
	if got, err := SafeLocalPath("./", "", "a/x"); err != nil || got != filepath.FromSlash("a/x") {
		t.Errorf("into ./: %q, %v", got, err)
	}
	root := string(os.PathSeparator)
	if got, err := SafeLocalPath(root, "", "a/x"); err != nil || got != filepath.Join(root, "a", "x") {
		t.Errorf("into the root: %q, %v", got, err)
	}
	for _, key := range []string{"../x", "a/../../x"} {
		if _, err := SafeLocalPath("./", "", key); err == nil {
			t.Errorf("%q escapes ./ but was accepted", key)
		}
	}
}

// A backslash is a separator on Windows: a version download must keep only
// the key's last segment there too, or "..\..\x.bat" names a file outside
// the download directory.
func TestVersionFileNameStripsBackslashPaths(t *testing.T) {
	got := VersionFileName(`..\..\Startup\x.bat`, "abcdef1234")
	if got != "x.abcdef12.bat" {
		t.Errorf("VersionFileName = %q, want x.abcdef12.bat", got)
	}
	if got := VersionFileName("..", "v"); strings.Contains(got, "..") && !strings.HasPrefix(got, "object") {
		t.Errorf("VersionFileName(..) = %q", got)
	}
}

// Symlinks BELOW a walked root. filepath.Walk lstats every entry, so a linked
// file used to vanish from uploads and sync listings, and a linked directory
// read as empty — which, under delete-extraneous, planned deleting every
// remote copy of its files.
func TestWalksFollowFileLinksAndReportDirectoryLinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "photos"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "photos", "p.jpg"), []byte("img"), 0600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(root, "linked.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "photos"), filepath.Join(root, "photos")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "gone"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}

	entries, opaque, err := WalkLocalTree(root)
	if err != nil {
		t.Fatalf("WalkLocalTree: %v", err)
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		sizes[e.Rel] = e.Size
	}
	if len(sizes) != 2 || sizes["plain.txt"] != 1 || sizes["linked.txt"] != 5 {
		t.Errorf("entries = %v, want plain.txt (1) and linked.txt with its target's size (5)", sizes)
	}
	sort.Strings(opaque)
	if strings.Join(opaque, ",") != "dangling,photos" {
		t.Errorf("opaque = %v, want the unfollowed links [dangling photos]", opaque)
	}

	m := newTestModel(t, NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	targets, _, err := m.PrepareUpload(root, "", &Object{Key: strPtr("b"), Ot: Bucket})
	if err != nil {
		t.Fatalf("PrepareUpload: %v", err)
	}
	keys := map[string]int64{}
	for _, tg := range targets {
		keys[tg.RemotePath] = tg.Size
	}
	if keys["tree/linked.txt"] != 5 {
		t.Errorf("upload targets = %v, want the linked file sent as tree/linked.txt (5 bytes)", keys)
	}
}

// The SDK paginator stops silently on a truncated page whose continuation
// token is missing or repeats, handing back a partial listing with no error.
func TestTruncatedWithoutToken(t *testing.T) {
	tok := func(s string) *string { return &s }
	cases := []struct {
		name     string
		out      *s3.ListObjectsV2Output
		prevNext string
		want     bool
	}{
		{"last page", &s3.ListObjectsV2Output{IsTruncated: false}, "", false},
		{"truncated with a token", &s3.ListObjectsV2Output{IsTruncated: true, NextContinuationToken: tok("t1")}, "", false},
		{"next token moves on", &s3.ListObjectsV2Output{IsTruncated: true, NextContinuationToken: tok("t2")}, "t1", false},
		{"truncated, no token", &s3.ListObjectsV2Output{IsTruncated: true}, "", true},
		{"truncated, empty token", &s3.ListObjectsV2Output{IsTruncated: true, NextContinuationToken: tok("")}, "", true},
		{"truncated, same token again", &s3.ListObjectsV2Output{IsTruncated: true, NextContinuationToken: tok("t1")}, "t1", true},
	}
	for _, tc := range cases {
		if got := truncatedWithoutToken(tc.out, tc.prevNext); got != tc.want {
			t.Errorf("%s: truncatedWithoutToken = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A target name near the filesystem's 255-byte limit must still get a temp
// name that fits; copying it whole made such objects undownloadable.
func TestTempStemFitsLongNames(t *testing.T) {
	long := strings.Repeat("報", 84) // 252 bytes, a valid name on its own
	stem := tempStem(long)
	if len(stem) > tempStemMax {
		t.Errorf("stem is %d bytes, want at most %d", len(stem), tempStemMax)
	}
	if !utf8.ValidString(stem) {
		t.Error("the stem was cut inside a rune")
	}
	if got := tempStem("report.pdf"); got != "report.pdf" {
		t.Errorf("a short name was changed to %q", got)
	}

	f, err := os.CreateTemp(t.TempDir(), tempStem(long)+".*.s3duck-part")
	if err != nil {
		t.Fatalf("temp file for a 252-byte name: %v", err)
	}
	f.Close()
}
