package model

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// PresignGetURL is pure local crypto (no network), so it can be exercised
// against a model built from a static config.
func TestPresignGetURL(t *testing.T) {
	region := "us-east-1"
	m := NewModel(NewConfig("https://s3.example.com", &region, "ak", "sk", true, 0))
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
	m := NewModel(NewConfig("https://s3.example.com", nil, "ak", "sk", true, 0))
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
	m := NewModel(NewConfig("https://s3.example.com", nil, "ak", "sk", true, 0))
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
	c := NewConfig("https://example.com", &region, "ak", "sk", true, 1024)

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

	c2 := NewConfig("u", nil, "", "", false, 0)
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
	nilL.wait(100)
}

func TestRateLimiterWait(t *testing.T) {
	l := newRateLimiter(1000) // 1000 B/s, starts with a 1000-token burst

	// Spending the burst must not block.
	start := time.Now()
	l.wait(1000)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("spending the initial burst blocked for %v", d)
	}

	// With the bucket drained, moving another 500 bytes must throttle ~0.5s.
	start = time.Now()
	l.wait(500)
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("wait did not throttle after draining: slept %v, want >= ~0.5s", d)
	}

	// n <= 0 is always a no-op.
	start = time.Now()
	l.wait(0)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("wait(0) blocked for %v", d)
	}
}

// PrepareUpload does not touch the S3 client or the bucket argument, so it is
// exercised here purely against the local filesystem.
//
// Note: PrepareUpload computes remote keys relative to localPath (the selected
// directory itself), i.e. WITHOUT the top-level directory name. model.Upload
// uses a different base (parent of localPath) and DOES include it. This test
// pins PrepareUpload's current contract; the divergence between the two is a
// known issue, not something this test endorses.
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
	want := map[string]int64{
		"pre/a.txt":          2,
		"pre/sub/b.txt":      4,
		"pre/sub/deep/c.txt": 6,
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

	if err := m.CopyObject(nil, b, "a", "b"); err == nil {
		t.Error("nil source bucket: want error")
	}
	if err := m.CopyObject(b, nil, "a", "b"); err == nil {
		t.Error("nil dest bucket: want error")
	}
	if err := m.CopyObject(b, b, "same", "same"); err == nil {
		t.Error("same bucket + same key: want error")
	}
}

func TestCopyKeysNilBuckets(t *testing.T) {
	m := &Model{}
	b := &Object{Key: strPtr("bucket")}
	if _, err := m.CopyKeys(nil, nil, b, "a", "b", false, nil); err == nil {
		t.Error("nil source bucket: want error")
	}
	if _, err := m.CopyKeys(nil, b, nil, "a", "b", false, nil); err == nil {
		t.Error("nil dest bucket: want error")
	}
}
