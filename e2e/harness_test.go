//go:build e2e

// Package e2e drives the real s3duck-tui binary, in a real terminal, against a
// real MinIO. Every other suite stops short of that: the unit tests call
// functions, and the integration suites call the model and the controller's
// flows directly. Neither can see a keypress reach an action, a dialog take
// focus, or a confirmation's answer reach the operation behind it — and those
// are exactly the seams where the 2026-10 review found its worst bugs (a
// one-file upload that sent a whole directory, an undo that destroyed what the
// user chose to keep, a config file rewritten before its error was shown).
//
// The terminal is tmux: the binary runs in a detached session, keys go in with
// send-keys and the screen comes back as text with capture-pane. Each test gets
// its own tmux server, its own HOME (config, downloads, activity log) and its
// own bucket, so a run depends on nothing outside the test and leaves nothing
// behind.
//
// The suite is tag-gated and skips without S3DUCK_TEST_ENDPOINT, so a plain
// `go test ./...` never runs it. `make e2e` runs it hermetically: MinIO and a
// runner container on a private compose network (see e2e/docker-compose.yml).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
)

// binPath is the s3duck-tui binary under test, built once by TestMain from the
// working tree — never a stale install.
var binPath string

// The screen is generous so nothing a test looks for is truncated: list names,
// the details panel and the dialogs all fit side by side.
const (
	screenCols = 220
	screenRows = 50
)

// waitTimeout bounds every wait for the screen or the bucket to reach a state.
// Generous, because a cold MinIO and a loaded CI runner are slow; a test that
// is going to pass never waits for it.
const waitTimeout = 20 * time.Second

func TestMain(m *testing.M) {
	if os.Getenv("S3DUCK_TEST_ENDPOINT") == "" {
		fmt.Println("e2e: S3DUCK_TEST_ENDPOINT is not set; skipping (run `make e2e`)")
		os.Exit(0)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Println("e2e: tmux is required:", err)
		os.Exit(1)
	}

	// S3DUCK_E2E_BINARY runs the suite against a binary built elsewhere — a
	// release build, or an older commit to show a scenario catches the bug it
	// was written for. By default the working tree is built.
	if bin := os.Getenv("S3DUCK_E2E_BINARY"); bin != "" {
		binPath = bin
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "s3duck-e2e-bin-")
	if err != nil {
		fmt.Println("e2e:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "s3duck-tui")
	// -buildvcs=false: the tree may be mounted read-only without git in the
	// image, and VCS stamping is irrelevant to what is being tested.
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/s3duck-tui")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("e2e: building the binary:", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// --- the S3 side -----------------------------------------------------------

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func endpoint() string  { return os.Getenv("S3DUCK_TEST_ENDPOINT") }
func accessKey() string { return env("S3DUCK_TEST_ACCESS_KEY", "minioadmin") }
func secretKey() string { return env("S3DUCK_TEST_SECRET_KEY", "minioadmin") }
func region() string    { return env("S3DUCK_TEST_REGION", "us-east-1") }

// store seeds and inspects one throwaway bucket, independently of the app: the
// assertions must be about what the server holds, not about what the app says.
type store struct {
	t      *testing.T
	m      *model.Model
	name   string
	bucket *model.Object
}

func newStore(t *testing.T) *store {
	t.Helper()
	r := region()
	m, err := model.NewModel(model.NewConfig(endpoint(), &r, accessKey(), secretKey(), "", true, 0))
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	name := bucketName(t)
	if err := m.CreateBucket(&name, false); err != nil {
		t.Fatalf("creating bucket %s: %v", name, err)
	}
	s := &store{t: t, m: m, name: name, bucket: &model.Object{Key: aws.String(name), Ot: model.Bucket}}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = m.EmptyBucket(ctx, s.bucket)
		_ = m.DeleteBucket(&name)
	})
	return s
}

// bucketName derives a valid, unique bucket name from the test's name.
func bucketName(t *testing.T) string {
	base := strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "-"))
	base = strings.Trim(strings.TrimPrefix(base, "test"), "-")
	if len(base) > 40 {
		base = base[:40]
	}
	return fmt.Sprintf("e2e-%s-%06d", strings.Trim(base, "-"), rand.Intn(1000000))
}

func (s *store) put(key, body string) {
	s.t.Helper()
	_, err := s.m.Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.name),
		Key:    aws.String(key),
		Body:   strings.NewReader(body),
	})
	if err != nil {
		s.t.Fatalf("seeding %s: %v", key, err)
	}
}

// get returns an object's body, and whether it exists.
func (s *store) get(key string) (string, bool) {
	s.t.Helper()
	c, err := s.m.GetObjectContent(context.Background(), s.bucket, key, 1<<20)
	if err != nil {
		return "", false
	}
	return string(c.Data), true
}

// keys lists every key under prefix, sorted.
func (s *store) keys(prefix string) []string {
	s.t.Helper()
	objs, err := s.m.ListObjects(context.Background(), prefix, s.bucket)
	if err != nil {
		s.t.Fatalf("listing %q: %v", prefix, err)
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, aws.ToString(o.Key))
	}
	sort.Strings(out)
	return out
}

// expect asserts the bucket's exact contents under prefix: key → body.
func (s *store) expect(prefix string, want map[string]string) {
	s.t.Helper()
	got := s.keys(prefix)
	var wantKeys []string
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(wantKeys)
	if strings.Join(got, "\n") != strings.Join(wantKeys, "\n") {
		s.t.Errorf("bucket keys under %q:\n  got  %v\n  want %v", prefix, got, wantKeys)
	}
	for k, body := range want {
		if strings.HasSuffix(k, "/") {
			continue // a folder marker: existence is the assertion
		}
		if b, ok := s.get(k); ok && b != body {
			s.t.Errorf("%s holds %q, want %q", k, b, body)
		}
	}
}

// --- the user's machine ----------------------------------------------------

// home is a throwaway HOME: the profile list, the download directory, the
// keymap and the activity log all live under it, so nothing a test does can
// touch — or depend on — the machine running it.
type home struct {
	t         *testing.T
	dir       string
	downloads string
}

func newHome(t *testing.T) *home {
	t.Helper()
	dir := t.TempDir()
	h := &home{t: t, dir: dir, downloads: filepath.Join(dir, "Downloads")}
	for _, d := range []string{filepath.Dir(h.configPath()), h.downloads, h.stateDir()} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *home) configPath() string {
	return filepath.Join(h.dir, ".config", "s3duck-tui", "config.json")
}

func (h *home) stateDir() string { return filepath.Join(h.dir, ".local", "state") }

// profile is the stored profile every test opens: the test's MinIO, downloads
// under this HOME, and — when bucket is set — a remembered location, so the
// profile reopens straight into the test's bucket instead of a bucket list
// shared with every other test.
func (h *home) profile(bucket, prefix string) *cfg.Config {
	r := region()
	return &cfg.Config{
		Name:        "e2e",
		BaseUrl:     endpoint(),
		Region:      &r,
		AccessKey:   accessKey(),
		SecretKey:   secretKey(),
		DownloadDir: h.downloads,
		LastBucket:  bucket,
		LastPrefix:  prefix,
	}
}

func (h *home) writeProfiles(profiles ...*cfg.Config) {
	h.t.Helper()
	data, err := json.Marshal(profiles)
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeConfig(data)
}

func (h *home) writeConfig(data []byte) {
	h.t.Helper()
	if err := os.WriteFile(h.configPath(), data, 0600); err != nil {
		h.t.Fatal(err)
	}
}

// file writes a local file under HOME, creating its directories.
func (h *home) file(rel, body string) string {
	h.t.Helper()
	p := filepath.Join(h.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// --- the terminal ----------------------------------------------------------

// tui is one running s3duck-tui in its own tmux server.
type tui struct {
	t    *testing.T
	sock string
}

// startTUI launches the binary with h as its HOME and waits for the first
// frame. The tmux server is private to the test (-L), configured from nothing
// (-f /dev/null plus explicit options), and killed on cleanup; when the test
// failed, the last screen is logged first, which is usually the whole story.
func startTUI(t *testing.T, h *home) *tui {
	t.Helper()
	u := &tui{t: t, sock: fmt.Sprintf("s3duck-e2e-%d-%d", os.Getpid(), rand.Int63())}

	conf := filepath.Join(t.TempDir(), "tmux.conf")
	// remain-on-exit keeps the pane after the app exits, so a crash leaves
	// its panic on screen for the failure dump, and a clean exit can be
	// observed as a dead pane rather than a vanished session.
	confText := "set -g remain-on-exit on\n" +
		"set -g default-terminal screen-256color\n" +
		"set -g history-limit 10000\n"
	if err := os.WriteFile(conf, []byte(confText), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := fmt.Sprintf("HOME=%q XDG_STATE_HOME=%q exec %q", h.dir, h.stateDir(), binPath)
	u.tmux("-f", conf, "new-session", "-d", "-s", "e2e",
		"-x", fmt.Sprint(screenCols), "-y", fmt.Sprint(screenRows), cmd)

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last screen:\n%s", u.screen())
		}
		_ = exec.Command("tmux", "-L", u.sock, "kill-server").Run()
	})
	return u
}

func (u *tui) tmux(args ...string) string {
	u.t.Helper()
	cmd := exec.Command("tmux", append([]string{"-L", u.sock}, args...)...)
	// tmux wants a TERM for its own sake even when nothing ever attaches.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	out, err := cmd.CombinedOutput()
	if err != nil {
		u.t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// screen is what the terminal shows right now, as plain text.
func (u *tui) screen() string {
	out, err := exec.Command("tmux", "-L", u.sock, "capture-pane", "-p", "-t", "e2e").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("<capture-pane failed: %v: %s>", err, out)
	}
	return string(out)
}

// press sends named keys (tmux names: Enter, Tab, Escape, Down, BSpace, DC,
// Space, C-d ...). Every press waits for the screen to settle afterwards: tview
// redraws asynchronously, and the next key must not race the dialog it is
// meant for.
func (u *tui) press(keys ...string) {
	u.t.Helper()
	for _, k := range keys {
		u.tmux("send-keys", "-t", "e2e", k)
		u.settle()
	}
}

// typeText sends literal text, as typed.
func (u *tui) typeText(s string) {
	u.t.Helper()
	u.tmux("send-keys", "-t", "e2e", "-l", s)
	u.settle()
}

// settle waits until two captures 60ms apart agree, bounded at a second. It
// is what lets a test send its next key only once the previous one has been
// drawn, without a sleep tuned to one machine.
func (u *tui) settle() {
	prev := u.screen()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(60 * time.Millisecond)
		cur := u.screen()
		if cur == prev {
			return
		}
		prev = cur
	}
}

// waitFor blocks until the screen contains every one of wants.
func (u *tui) waitFor(wants ...string) string {
	u.t.Helper()
	return u.waitUntil(fmt.Sprintf("screen to show %q", wants), func(s string) bool {
		for _, w := range wants {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	})
}

// waitGone blocks until the screen no longer contains text.
func (u *tui) waitGone(text string) {
	u.t.Helper()
	u.waitUntil(fmt.Sprintf("%q to disappear", text), func(s string) bool {
		return !strings.Contains(s, text)
	})
}

func (u *tui) waitUntil(desc string, ok func(screen string) bool) string {
	u.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		s := u.screen()
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			u.t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(80 * time.Millisecond)
	}
}

var fullPathRe = regexp.MustCompile(`FullPath:\s+(\S+)`)

// highlighted reads the details panel's FullPath line: the key (or local
// path) of the row under the cursor, or "" on the ".." row. Only the
// single-pane layout has a details panel.
func highlighted(screen string) string {
	if m := fullPathRe.FindStringSubmatch(screen); m != nil {
		return m[1]
	}
	return ""
}

// highlight moves the cursor down until fullPath is the highlighted row. It
// fails rather than wrapping forever when the row is not in the listing.
func (u *tui) highlight(fullPath string) {
	u.t.Helper()
	for i := 0; i < 60; i++ {
		if highlighted(u.screen()) == fullPath {
			return
		}
		u.press("Down")
	}
	u.t.Fatalf("could not move the cursor onto %q", fullPath)
}

// quit presses Ctrl+Q and waits for the process to exit cleanly.
func (u *tui) quit() {
	u.t.Helper()
	u.press("C-q")
	deadline := time.Now().Add(waitTimeout)
	for {
		state := strings.TrimSpace(u.tmux("display-message", "-p", "-t", "e2e", "#{pane_dead} #{pane_dead_status}"))
		if strings.HasPrefix(state, "1") {
			if state != "1 0" {
				u.t.Fatalf("s3duck-tui exited with %q", state)
			}
			return
		}
		if time.Now().After(deadline) {
			u.t.Fatalf("s3duck-tui did not exit after Ctrl+Q")
		}
		time.Sleep(80 * time.Millisecond)
	}
}

// openProfile waits for the profiles screen, opens the (only) profile and
// waits for the remembered location's listing to arrive.
func (u *tui) openProfile(bucket, prefix string) {
	u.t.Helper()
	u.waitFor("(profiles)", "e2e")
	u.press("Enter")
	u.waitFor(fmt.Sprintf("(%s)/%s", bucket, prefix))
}

// readFile is os.ReadFile for assertions.
func readFile(t *testing.T, path string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}
