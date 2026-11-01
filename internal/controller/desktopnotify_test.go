package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	"github.com/nexusriot/s3duck-tui/internal/jobs"
	"github.com/nexusriot/s3duck-tui/internal/model"
)

func TestResolveNotifyMode(t *testing.T) {
	have := func(string) (string, error) { return "/usr/bin/x", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	desktop := env(map[string]string{"DISPLAY": ":0"})
	ssh := env(nil)

	tests := []struct {
		name, mode, goos string
		getenv           func(string) string
		look             func(string) (string, error)
		want             string
		wantReason       bool
	}{
		{"auto on a linux desktop", cli.NotifyAuto, "linux", desktop, have, cli.NotifyCommand, false},
		{"auto on wayland", cli.NotifyAuto, "linux", env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}), have, cli.NotifyCommand, false},
		// Over SSH there is no display for notify-send to post to.
		{"auto over ssh", cli.NotifyAuto, "linux", ssh, have, cli.NotifyOff, false},
		{"auto without notify-send", cli.NotifyAuto, "linux", desktop, missing, cli.NotifyOff, false},
		{"auto on macOS", cli.NotifyAuto, "darwin", ssh, have, cli.NotifyCommand, false},
		{"command, ssh: asked for", cli.NotifyCommand, "linux", ssh, have, cli.NotifyCommand, false},
		{"command not installed", cli.NotifyCommand, "linux", desktop, missing, cli.NotifyOff, true},
		{"osc9 is taken as asked", cli.NotifyOSC9, "linux", ssh, missing, cli.NotifyOSC9, false},
		{"off", cli.NotifyOff, "linux", desktop, have, cli.NotifyOff, false},
	}
	for _, tt := range tests {
		got, reason := resolveNotifyMode(tt.mode, tt.goos, tt.getenv, tt.look)
		if got != tt.want || (reason != "") != tt.wantReason {
			t.Errorf("%s: = %q (reason %q), want %q (reason given: %v)", tt.name, got, reason, tt.want, tt.wantReason)
		}
	}
}

func TestCommandArgv(t *testing.T) {
	n := notification{title: "s3duck-tui", body: `download "q4\report": done`}
	got := commandArgv("notify-send", n)
	if want := []string{"notify-send", "--app-name=s3duck-tui", "s3duck-tui", `download "q4\report": done`}; !reflect.DeepEqual(got, want) {
		t.Errorf("notify-send argv = %q", got)
	}
	osa := commandArgv("osascript", n)
	if len(osa) != 3 || osa[1] != "-e" {
		t.Fatalf("osascript argv = %q", osa)
	}
	// The body is an AppleScript string literal: its quote and backslash are
	// escaped, so a key cannot close the literal and append script.
	if want := `display notification "download \"q4\\report\": done" with title "s3duck-tui"`; osa[2] != want {
		t.Errorf("osascript script = %s\nwant              %s", osa[2], want)
	}
}

// notify-send bodies are markup, so a key's & < > are escaped there — and
// only there: osascript and the OSC sequences take plain text.
func TestNotifySendBodyIsMarkupEscaped(t *testing.T) {
	n := notification{title: "s3duck-tui", body: "download <q4>&co.csv: done"}
	if got := commandArgv("notify-send", n)[3]; got != "download &lt;q4&gt;&amp;co.csv: done" {
		t.Errorf("notify-send body = %q", got)
	}
	if got := commandArgv("osascript", n)[2]; !strings.Contains(got, "<q4>&co.csv") {
		t.Errorf("osascript text = %q", got)
	}
}

func TestOSCSequence(t *testing.T) {
	n := notification{title: "s3;duck", body: "sync a: done"}
	if got := oscSequence(cli.NotifyOSC9, n, false); got != "\x1b]9;s3;duck: sync a: done\x07" {
		t.Errorf("osc9 = %q", got)
	}
	// 777 splits fields on ";": one in the title would move text into the body.
	if got := oscSequence(cli.NotifyOSC777, n, false); got != "\x1b]777;notify;s3,duck;sync a: done\x07" {
		t.Errorf("osc777 = %q", got)
	}
	tm := oscSequence(cli.NotifyOSC9, notification{title: "t", body: "b"}, true)
	if tm != "\x1bPtmux;\x1b\x1b]9;t: b\x07\x1b\\" {
		t.Errorf("tmux passthrough = %q", tm)
	}
	if oscSequence(cli.NotifyCommand, n, false) != "" {
		t.Error("a non-OSC mode produced a sequence")
	}
}

// An object key is user data. Its control characters must not survive into
// an escape sequence, where ESC or BEL would end it and let the rest of the
// key write to the terminal unfiltered.
func TestNotificationTextCannotEscape(t *testing.T) {
	evil := notification{title: "t", body: "download a\x07\x1b]0;pwned\x1b\\b\x9b2J\nmore"}
	seq := oscSequence(cli.NotifyOSC9, evil, false)
	inner := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]9;"), "\x07")
	for _, bad := range []string{"\x07", "\x1b", "\x9b", "\n"} {
		if strings.Contains(inner, bad) {
			t.Errorf("control %q survived: %q", bad, seq)
		}
	}
	long := cleanNotifyText(strings.Repeat("é", 500))
	if n := len([]rune(long)); n != 200 {
		t.Errorf("capped to %d runes, want 200", n)
	}
}

func TestJobNotice(t *testing.T) {
	n := jobNotice(jobs.View{Kind: "download", Desc: "docs/", Status: jobs.Failed, Failed: 3, Bg: true})
	if n.title != notifyTitle || !strings.Contains(n.body, "download docs/") || !strings.Contains(n.body, "(3 failed)") {
		t.Errorf("notice = %+v", n)
	}
	if strings.Contains(n.body, "  ") {
		t.Errorf("padded status leaked into %q", n.body)
	}
}

func TestNotifierPost(t *testing.T) {
	var mu sync.Mutex
	var ran [][]string
	var wrote []string
	done := make(chan struct{}, 4)
	var logged []string

	cmd := &notifier{mode: cli.NotifyCommand, prog: "notify-send",
		run: func(argv []string) error {
			mu.Lock()
			ran = append(ran, argv)
			mu.Unlock()
			done <- struct{}{}
			return errors.New("dbus says no")
		},
		log: func(f string, a ...any) {
			mu.Lock()
			logged = append(logged, f)
			mu.Unlock()
		},
	}
	cmd.post(notification{title: "t", body: "one"})
	cmd.post(notification{title: "t", body: "two"})
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("command never ran")
		}
	}
	// Give the failure logging a moment; it runs after run returns.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(ran) != 2 {
		t.Errorf("ran %d commands, want 2", len(ran))
	}
	if len(logged) != 1 {
		t.Errorf("logged %d failures, want 1 (the first only)", len(logged))
	}
	mu.Unlock()

	osc := &notifier{mode: cli.NotifyOSC777, writeTerm: func(s string) { wrote = append(wrote, s) },
		run: func([]string) error { t.Error("osc mode ran a command"); return nil }}
	osc.post(notification{title: "t", body: "b"})
	if len(wrote) != 1 || !strings.HasPrefix(wrote[0], "\x1b]777;notify;") {
		t.Errorf("osc wrote %q", wrote)
	}

	off := &notifier{mode: cli.NotifyOff, writeTerm: func(string) { t.Error("off wrote") },
		run: func([]string) error { t.Error("off ran"); return nil }}
	off.post(notification{title: "t", body: "b"})

	var nilN *notifier
	nilN.post(notification{}) // must not panic
}

// A Glacier restore becoming readable posts a notification naming the object
// and how long it stays readable — through the real check round, against a
// fake endpoint answering the HEAD as S3 does for a finished restore.
func TestRestoreReadyPostsNotification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-storage-class", "GLACIER")
		w.Header().Set("x-amz-restore", restoredHeader)
		w.Header().Set("Content-Length", "3")
	}))
	defer srv.Close()
	region := "us-east-1"
	mdl, err := model.NewModel(model.NewConfig(srv.URL, &region, "k", "s", "", true, 0))
	if err != nil {
		t.Fatal(err)
	}

	c := NewController(cli.Options{Notify: cli.NotifyOff})
	c.restores = loadRestoreTracker(filepath.Join(t.TempDir(), "restores.json"))
	c.restores.track(restoreEntry{Profile: "p", Bucket: "cold", Key: "logs/2019.tar"})
	posted := make(chan []string, 1)
	c.notify = &notifier{mode: cli.NotifyCommand, prog: "notify-send",
		run: func(argv []string) error { posted <- argv; return nil }}

	c.checkRestores(mdl, "p")

	select {
	case argv := <-posted:
		body := argv[len(argv)-1]
		if !strings.Contains(body, "Restore ready: s3://cold/logs/2019.tar") || !strings.Contains(body, "until") {
			t.Errorf("notification body = %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification for a restore that became readable")
	}

	// A second round finds it still ready: no second notification.
	c.restores.entries[0].Checked = time.Time{}
	c.checkRestores(mdl, "p")
	select {
	case argv := <-posted:
		t.Errorf("notified again: %q", argv)
	case <-time.After(200 * time.Millisecond):
	}
}
