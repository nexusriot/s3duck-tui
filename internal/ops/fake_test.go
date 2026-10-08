package ops

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// This file is the whole reason the ports are narrow: a download's
// orchestration can be exercised against twenty lines of fake instead of a
// live S3 endpoint.

// fakeFetcher stands in for the model's downloader.
type fakeFetcher struct {
	mu sync.Mutex
	// failFor maps an object key to the error its transfer should return.
	failFor map[string]error
	// blockFor names keys whose transfer waits for ctx before returning, used
	// to test cancellation draining.
	blockFor map[string]bool
	// got records every key fetched, in completion order, with its overwrite
	// flag.
	got       []fetched
	progress  bool
	fetchHook func(key string)
}

type fetched struct {
	key       string
	overwrite bool
}

func (f *fakeFetcher) DownloadTarget(
	ctx context.Context,
	t model.DownloadTarget,
	_, _ string,
	_ *string,
	overwrite bool,
	progress func(written, total int64, key string),
) (int64, error) {
	if f.fetchHook != nil {
		f.fetchHook(t.Key)
	}
	f.mu.Lock()
	err := f.failFor[t.Key]
	block := f.blockFor[t.Key]
	f.mu.Unlock()

	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, err
	}
	if f.progress && progress != nil {
		progress(t.Size/2, t.Size, t.Key)
		progress(t.Size, t.Size, t.Key)
	}

	f.mu.Lock()
	f.got = append(f.got, fetched{key: t.Key, overwrite: overwrite})
	f.mu.Unlock()
	return t.Size, nil
}

func (f *fakeFetcher) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.got))
	for _, g := range f.got {
		out = append(out, g.key)
	}
	return out
}

func (f *fakeFetcher) overwroteKey(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.got {
		if g.key == key {
			return g.overwrite
		}
	}
	return false
}

// fakeChecker stands in for the post-transfer verifier.
type fakeChecker struct {
	results map[string]model.VerifyResult
	errs    map[string]error
}

func (c *fakeChecker) VerifyLocalFile(_ context.Context, _ *model.Object, key, _ string) (model.VerifyResult, error) {
	if err := c.errs[key]; err != nil {
		return model.VerifyResult{}, err
	}
	return c.results[key], nil
}

// memFS is an in-memory stand-in for the destination filesystem: which paths
// already exist (so the overwrite branches fire) and which directories were
// created.
type memFS struct {
	mu       sync.Mutex
	existing map[string]bool
	made     []string
	mkdirErr error
}

func newMemFS(existing ...string) *memFS {
	m := &memFS{existing: map[string]bool{}}
	for _, e := range existing {
		m.existing[e] = true
	}
	return m
}

func (m *memFS) Exists(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A fake destination is matched by suffix so a test can name "a/x.txt"
	// without reproducing the temp-dir prefix the operation builds.
	for e := range m.existing {
		if path == e || strings.HasSuffix(path, "/"+e) {
			return true
		}
	}
	return false
}

func (m *memFS) MkdirAll(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mkdirErr != nil {
		return m.mkdirErr
	}
	m.made = append(m.made, path)
	return nil
}

func (m *memFS) madeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.made)
}

// recorder is a Reporter that records progress and plays scripted answers.
type recorder struct {
	mu       sync.Mutex
	samples  []Progress
	asked    []Question
	logs     []string
	answers  []Answer
	fallback Answer
	// onAsk runs before each answer is handed back, so a test can cancel the
	// context from inside a question.
	onAsk func(Question)
}

func (r *recorder) Progress(p Progress) {
	r.mu.Lock()
	r.samples = append(r.samples, p)
	r.mu.Unlock()
}

func (r *recorder) Ask(q Question) Answer {
	r.mu.Lock()
	r.asked = append(r.asked, q)
	hook := r.onAsk
	var a Answer
	if len(r.answers) > 0 {
		a, r.answers = r.answers[0], r.answers[1:]
	} else {
		a = r.fallback
	}
	r.mu.Unlock()
	if hook != nil {
		hook(q)
	}
	return a
}

func (r *recorder) Log(format string, args ...any) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) askedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.asked))
	for _, q := range r.asked {
		out = append(out, q.Path)
	}
	return out
}

func (r *recorder) lastProgress() (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return Progress{}, false
	}
	return r.samples[len(r.samples)-1], true
}

// blockingGate is a Gate that is already full, so Acquire only returns when the
// context ends.
type blockingGate struct{}

func (blockingGate) Acquire(ctx context.Context) bool { <-ctx.Done(); return false }
func (blockingGate) Release()                         {}

func bucketObj(name string) *model.Object {
	return &model.Object{Key: &name, Ot: model.Bucket}
}

func targets(spec ...any) []model.DownloadTarget {
	out := make([]model.DownloadTarget, 0, len(spec)/2)
	for i := 0; i < len(spec); i += 2 {
		out = append(out, model.DownloadTarget{Key: spec[i].(string), Size: int64(spec[i+1].(int))})
	}
	return out
}
