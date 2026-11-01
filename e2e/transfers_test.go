//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A transfer sent to the background posts a desktop notification when it
// finishes: here through a stand-in notify-send that records its arguments.
func TestBackgroundDownloadNotifies(t *testing.T) {
	s := newStore(t)
	// Big enough, with the throttle below, to still be running when
	// Background is pressed.
	s.put("big.bin", strings.Repeat("x", 600<<10))
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 200 << 10
	h.writeProfiles(p)

	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "notified")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(filepath.Join(bin, "notify-send"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	u := startTUIWith(t, h, []string{"PATH=" + bin + ":" + os.Getenv("PATH")}, "--notify", "command")
	u.openProfile(s.name, "")
	u.waitFor("big.bin")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter") // OK
	u.waitFor("Background")
	u.press("Enter") // Background

	deadline := time.Now().Add(waitTimeout)
	for {
		data, err := os.ReadFile(record)
		// The job is described as the queue lists it: kind, what and where
		// ("download 1 obj → …/Downloads/"), then how it ended.
		if err == nil && len(data) > 0 {
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(got) != 3 || got[0] != "--app-name=s3duck-tui" || got[1] != "s3duck-tui" ||
				!strings.HasPrefix(got[2], "download ") || !strings.HasSuffix(got[2], ": done") ||
				!strings.Contains(got[2], h.downloads) {
				t.Errorf("notify-send argv = %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no notification posted (record: %q, %v)", data, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got, ok := readFile(t, filepath.Join(h.downloads, "big.bin")); !ok || len(got) != 600<<10 {
		t.Errorf("the download itself: %d bytes, exists %v", len(got), ok)
	}
}

// --notify osc777 writes the notification to the terminal itself; inside
// tmux it is wrapped for passthrough. pipe-pane sees the raw bytes the app
// wrote, before tmux interprets them.
func TestBackgroundDownloadNotifiesThroughTerminal(t *testing.T) {
	s := newStore(t)
	s.put("big.bin", strings.Repeat("x", 600<<10))
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 200 << 10
	h.writeProfiles(p)

	raw := filepath.Join(t.TempDir(), "pane.raw")
	u := startTUIWith(t, h, noClipboard, "--notify", "osc777")
	u.tmux("pipe-pane", "-O", "-t", "e2e", "cat >> "+raw)
	u.openProfile(s.name, "")
	u.waitFor("big.bin")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter") // OK
	u.waitFor("Background")
	u.press("Enter") // Background

	want := "\x1bPtmux;\x1b\x1b]777;notify;s3duck-tui;download "
	deadline := time.Now().Add(waitTimeout)
	for {
		data, _ := os.ReadFile(raw)
		if i := strings.Index(string(data), want); i >= 0 {
			rest := string(data[i+len(want):])
			end := strings.Index(rest, "\x07\x1b\\")
			if end < 0 || !strings.HasSuffix(rest[:end], ": done") {
				t.Errorf("sequence not terminated as a passthrough, or not done: %q", rest[:min(len(rest), 160)])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no OSC 777 notification in the pane output (%d bytes captured)", len(data))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Ctrl+Q under running transfers asks first; yes cancels them and lets each
// clean up before the process exits. An unfinished upload is aborted on the
// server. An unfinished download keeps what it has as a resumable partial —
// no file at the target name — and the next run, after a restart, resumes it
// to the object byte for byte with nothing left over. (Quitting used to stop
// the process under both, which did neither.)
func TestQuitCancelsTransfersAndADownloadResumesAfterRestart(t *testing.T) {
	s := newStore(t)
	body := strings.Repeat("d", 64<<20) // past the 5×5 MiB fetched at once
	s.put("big.bin", body)
	h := newHome(t)
	h.file("Downloads/up.bin", strings.Repeat("u", 12<<20))
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 2 << 20 // keeps both transfers running for several seconds
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("big.bin")

	// A download, sent to the background.
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Background")
	u.press("Enter")
	u.waitUntil("the download's partial file to exist", func(string) bool {
		m, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
		return len(m) > 0
	})

	// A multipart upload from the local pane, also sent to the background.
	u.press("l")
	u.waitFor("[local]", "up.bin")
	u.press("/")
	u.typeText("up.bin")
	u.press("Enter")
	u.waitUntil("the filter to leave only up.bin", func(scr string) bool {
		return strings.Contains(scr, "up.bin") && !strings.Contains(scr, ".s3duck-part")
	})
	u.press("C-y")
	u.waitFor("Background")
	u.press("Enter")
	u.waitUntil("the upload to have started its multipart upload", func(string) bool {
		ups, err := s.m.ListMultipartUploads(s.bucket)
		return err == nil && len(ups) > 0
	})

	u.press("C-q")
	u.waitFor("2 transfer(s) still running", "Quit anyway?")
	u.press("Enter") // OK
	u.waitForExit()

	if _, err := os.Stat(filepath.Join(h.downloads, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("the interrupted download is at the target name")
	}
	partials, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
	sidecars, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-resume"))
	if len(partials) != 1 || len(sidecars) != 1 {
		t.Fatalf("after quit: partials %v, sidecars %v; want one resumable partial", partials, sidecars)
	}
	if ups, err := s.m.ListMultipartUploads(s.bucket); err != nil || len(ups) != 0 {
		t.Errorf("multipart uploads left open after quit: %v %v", ups, err)
	}
	logData, _ := os.ReadFile(filepath.Join(h.stateDir(), "s3duck-tui", "activity.log"))
	if !strings.Contains(string(logData), "quit: cancelled 2 transfer(s)") || strings.Contains(string(logData), "did not finish") {
		t.Errorf("activity log does not record the cancel:\n%s", logData)
	}

	// A new run of the app picks the download up where it stopped.
	p.MaxBytesPerSec = 0
	h.writeProfiles(p)
	u = startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Download complete.")
	u.press("Enter")
	if got, _ := readFile(t, filepath.Join(h.downloads, "big.bin")); got != body {
		t.Errorf("the resumed download is %d bytes and differs from the object", len(got))
	}
	if left, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*")); len(left) != 0 {
		t.Errorf("left after the resume: %v", left)
	}
}

// Answering Cancel to the quit question keeps the app — and its transfers —
// running, and a later Ctrl+Q asks again.
func TestQuitQuestionCanBeDeclined(t *testing.T) {
	s := newStore(t)
	s.put("big.bin", strings.Repeat("d", 4<<20))
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 256 << 10
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Background")
	u.press("Enter")

	u.press("C-q")
	u.waitFor("1 transfer(s) still running")
	u.press("Tab", "Enter") // Cancel
	u.waitGone("still running")
	u.press("C-q")
	u.waitFor("1 transfer(s) still running")
	u.press("Tab", "Enter")
	u.waitGone("still running")
}

// A throttled download cancelled part-way leaves a hidden partial file and
// no target; downloading again resumes it, and the result is the object
// byte for byte, with nothing left over.
func TestDownloadCancelThenResume(t *testing.T) {
	s := newStore(t)
	body := strings.Repeat("0123456789abcdef", (40<<20)/16)
	s.put("big.bin", body)
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 8 << 20
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitUntil("some of it to be on disk", func(string) bool {
		m, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
		if len(m) == 0 {
			return false
		}
		fi, err := os.Stat(m[0])
		return err == nil && fi.Size() >= 12<<20
	})
	u.press("Tab", "Enter") // Cancel (Background is first)
	u.waitFor("Download canceled.")
	u.press("Enter") // Done — the summary must not keep Cancel's button position

	if _, err := os.Stat(filepath.Join(h.downloads, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("a cancelled download left a file at the target name")
	}
	partials, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
	if len(partials) != 1 {
		t.Fatalf("partial files after the cancel: %v", partials)
	}
	fi, _ := os.Stat(partials[0])
	if fi.Size() == 0 || fi.Size() >= int64(len(body)) {
		t.Fatalf("the kept partial is %d bytes", fi.Size())
	}

	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Download complete.")
	u.press("Enter")

	got, _ := readFile(t, filepath.Join(h.downloads, "big.bin"))
	if got != body {
		t.Errorf("the resumed file is %d bytes and differs from the object", len(got))
	}
	left, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*"))
	if len(left) != 0 {
		t.Errorf("left after the resumed download: %v", left)
	}
}
