package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	"github.com/nexusriot/s3duck-tui/internal/jobs"
)

// Desktop notifications for the moments the bell already marks: a
// backgrounded transfer finishing, a Glacier restore becoming readable. The
// bell only reaches someone looking at this terminal; a notification reaches
// them in another window.
//
// Two delivery paths, because neither works everywhere:
//
//   - command: notify-send (freedesktop) or osascript (macOS) — reliable on a
//     desktop, useless over SSH, where there is no display to post to;
//   - osc9 / osc777: an escape sequence the *terminal* turns into a
//     notification (iTerm2, WezTerm, kitty, foot, Ghostty, Windows Terminal…)
//     — which works over SSH, since it travels with the output.
//
// auto picks the command when one can reach a display and stays quiet
// otherwise: guessing an OSC on a terminal that does not speak it prints junk
// into the frame, so the escape paths are opt-in (--notify / S3DUCK_NOTIFY).

// notification is one message to post.
type notification struct {
	title, body string
}

const notifyTitle = "s3duck-tui"

// jobNotice words a finished background job.
func jobNotice(v jobs.View) notification {
	status := strings.TrimSpace(v.Status.String())
	body := fmt.Sprintf("%s %s: %s", v.Kind, v.Desc, status)
	if v.Failed > 0 {
		body += fmt.Sprintf(" (%d failed)", v.Failed)
	}
	return notification{title: notifyTitle, body: body}
}

// notifyCommand names the notification program for a platform.
func notifyCommand(goos string) string {
	if goos == "darwin" {
		return "osascript"
	}
	return "notify-send"
}

// resolveNotifyMode turns the requested mode into the one used. auto becomes
// command when the platform's program is installed and — off macOS — a
// display is there to show it on; otherwise off. An explicit command mode with
// no program installed is off too, with the reason, for the activity log.
func resolveNotifyMode(mode, goos string, getenv func(string) string, lookPath func(string) (string, error)) (resolved, reason string) {
	switch mode {
	case cli.NotifyOff, cli.NotifyOSC9, cli.NotifyOSC777:
		return mode, ""
	}
	prog := notifyCommand(goos)
	if _, err := lookPath(prog); err != nil {
		if mode == cli.NotifyCommand {
			return cli.NotifyOff, prog + " is not installed"
		}
		return cli.NotifyOff, ""
	}
	if mode == cli.NotifyAuto && goos != "darwin" && goos != "windows" &&
		getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
		// Over SSH notify-send has nowhere to post and fails noisily.
		return cli.NotifyOff, ""
	}
	return cli.NotifyCommand, ""
}

// cleanNotifyText makes a message safe to put inside an escape sequence or an
// argument: control characters — ESC and BEL above all, which would end the
// sequence early and let an object key write raw escapes to the terminal —
// become spaces, and the result is capped.
func cleanNotifyText(s string) string {
	const max = 200
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			r = ' '
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > max {
		out = string(rs[:max-1]) + "…"
	}
	return out
}

// commandArgv is the program invocation for n. Arguments are passed as argv,
// never through a shell; for osascript the text is an AppleScript string
// literal, so its quotes and backslashes are escaped.
func commandArgv(prog string, n notification) []string {
	title, body := cleanNotifyText(n.title), cleanNotifyText(n.body)
	if prog == "osascript" {
		q := func(s string) string {
			s = strings.ReplaceAll(s, `\`, `\\`)
			return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		return []string{"osascript", "-e", "display notification " + q(body) + " with title " + q(title)}
	}
	// The body is markup to libnotify daemons (GNOME, KDE): "a&b" or
	// "<x>.log" in a key blanked or garbled it.
	markup := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return []string{"notify-send", "--app-name=" + notifyTitle, title, markup.Replace(body)}
}

// oscSequence is the terminal escape for n. OSC 777 separates its fields with
// ";", so one in the title would shift the body into it. Inside tmux the
// sequence is wrapped for passthrough (tmux 3.3+ with allow-passthrough on);
// without the wrapper tmux swallows it.
func oscSequence(mode string, n notification, inTmux bool) string {
	title, body := cleanNotifyText(n.title), cleanNotifyText(n.body)
	var seq string
	switch mode {
	case cli.NotifyOSC9:
		seq = "\x1b]9;" + title + ": " + body + "\x07"
	case cli.NotifyOSC777:
		seq = "\x1b]777;notify;" + strings.ReplaceAll(title, ";", ",") + ";" + body + "\x07"
	default:
		return ""
	}
	if inTmux {
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	return seq
}

// notifier posts notifications in the resolved mode. post is safe from any
// goroutine and never blocks its caller.
type notifier struct {
	mode   string
	prog   string
	inTmux bool
	// writeTerm puts an escape sequence on the terminal (from the UI
	// goroutine, so it cannot land in the middle of a frame).
	writeTerm func(string)
	// run starts the command; replaced in tests.
	run func(argv []string) error
	// log records the first delivery failure, so a broken notify-send is
	// visible in the activity log without filling it.
	log      func(format string, args ...any)
	failOnce sync.Once
}

// newNotifier resolves mode for this machine.
func newNotifier(mode string, writeTerm func(string), log func(string, ...any)) *notifier {
	resolved, reason := resolveNotifyMode(mode, runtime.GOOS, os.Getenv, exec.LookPath)
	n := &notifier{
		mode:      resolved,
		prog:      notifyCommand(runtime.GOOS),
		inTmux:    os.Getenv("TMUX") != "",
		writeTerm: writeTerm,
		run:       runNotifyCommand,
		log:       log,
	}
	if reason != "" && log != nil {
		log("notifications off: %s", reason)
	}
	return n
}

// runNotifyCommand runs argv with no terminal attached: its stdout and stderr
// would otherwise be written straight over the TUI.
func runNotifyCommand(argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Run()
}

func (n *notifier) post(msg notification) {
	if n == nil {
		return
	}
	switch n.mode {
	case cli.NotifyCommand:
		argv := commandArgv(n.prog, msg)
		go func() {
			if err := n.run(argv); err != nil && n.log != nil {
				n.failOnce.Do(func() { n.log("desktop notification failed (%s): %v", argv[0], err) })
			}
		}()
	case cli.NotifyOSC9, cli.NotifyOSC777:
		if seq := oscSequence(n.mode, msg, n.inTmux); seq != "" && n.writeTerm != nil {
			n.writeTerm(seq)
		}
	}
}
