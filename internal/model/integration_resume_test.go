//go:build integration

package model_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// Against a real server: a download cancelled part-way keeps its prefix, the
// next one resumes it with a ranged GET pinned by If-Match, and the result is
// the object byte for byte. Then a stale ETag — the object replaced since —
// is refused by the server and nothing is kept.
func TestIntegrationDownloadResume(t *testing.T) {
	plain := testModel(t)
	b := freshBucket(t, plain, "s3duck-it-resume")
	// Throttled, so the cancel lands while ranges are still in flight: on a
	// local server an unthrottled 20 MiB is over before it can be stopped.
	endpoint, region, access, secret := testCreds(t)
	m, err := model.NewModel(model.NewConfig(endpoint, &region, access, secret, "", true, 8<<20))
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("0123456789abcdef"), (64<<20)/16) // past the 5×5 MiB the downloader fetches at once
	putObject(t, plain, b, "big.bin", string(body))
	head, err := plain.HeadObject(context.Background(), b, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := model.DownloadTarget{Key: "big.bin", Size: int64(len(body)), ETag: head.ETag}

	ctx, cancel := context.WithCancel(context.Background())
	var seen int64
	_, err = m.DownloadTarget(ctx, target, "", dir, b.Key, false, func(w, _ int64, _ string) {
		if atomic.AddInt64(&seen, 0) == 0 && w >= 6<<20 {
			atomic.StoreInt64(&seen, w)
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first attempt: %v", err)
	}
	var partial string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".s3duck-part") {
			partial = filepath.Join(dir, e.Name())
		}
	}
	fi, err := os.Stat(partial)
	if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(body)) {
		t.Fatalf("after the cancel: partial %q, %v, %v", partial, fi, err)
	}
	kept := fi.Size()

	var first int64 = -1
	n, err := m.DownloadTarget(context.Background(), target, "", dir, b.Key, false, func(w, _ int64, _ string) {
		if first < 0 {
			first = w
		}
	})
	if err != nil || n != int64(len(body)) {
		t.Fatalf("resume: %d, %v", n, err)
	}
	if first != kept {
		t.Errorf("the resume started at %d, want the %d kept", first, kept)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "big.bin"))
	if !bytes.Equal(got, body) {
		t.Fatal("the resumed file differs from the object")
	}

	stale := target
	stale.ETag = "00000000000000000000000000000000"
	_, err = m.DownloadTarget(context.Background(), stale, "", dir, b.Key, true, nil)
	if !errors.Is(err, model.ErrObjectChanged) {
		t.Errorf("a stale version was downloaded: %v", err)
	}
}
