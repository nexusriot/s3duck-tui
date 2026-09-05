package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditLine(t *testing.T) {
	when := time.Date(2026, 9, 2, 18, 15, 0, 0, time.UTC)
	got := auditLine(when, "prod", "Delete: 3 ok, 0 failed")
	want := "2026-09-02T18:15:00Z\tprod\tDelete: 3 ok, 0 failed\n"
	if got != want {
		t.Errorf("auditLine = %q, want %q", got, want)
	}
	// No profile open yet.
	if got := auditLine(when, "", "x"); !strings.Contains(got, "\t-\t") {
		t.Errorf("missing profile should render as -: %q", got)
	}
	// A message must not be able to forge an entry or shift the columns.
	got = auditLine(when, "p", "line1\nline2\twith tab\r")
	if strings.Count(got, "\n") != 1 {
		t.Errorf("embedded newline was not folded: %q", got)
	}
	if strings.Count(got, "\t") != 2 {
		t.Errorf("embedded tab was not folded: %q", got)
	}
}

func TestAppendAuditCreatesRotatesAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "activity.log")

	if err := appendAudit(path, "one\n", 1<<20); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendAudit(path, "two\n", 1<<20); err != nil {
		t.Fatalf("second append: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one\ntwo\n" {
		t.Errorf("log = %q, want both lines appended", body)
	}
	// The file holds credentials-adjacent operational history: owner-only.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("log mode = %v, want 0600", perm)
	}

	// Past the cap the file rotates once and the new line starts a fresh one.
	if err := appendAudit(path, "three\n", 8); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != "three\n" {
		t.Errorf("after rotation the log holds %q, want just the new line", body)
	}
	if body, err := os.ReadFile(path + ".1"); err != nil || string(body) != "one\ntwo\n" {
		t.Errorf("rotated file = %q (%v)", body, err)
	}
}

func TestAuditPathHonoursXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	if got := AuditPath("/home/u"); got != "/tmp/state/s3duck-tui/activity.log" {
		t.Errorf("AuditPath = %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got := AuditPath("/home/u"); got != "/home/u/.local/state/s3duck-tui/activity.log" {
		t.Errorf("AuditPath fallback = %q", got)
	}
}

func TestKeysPath(t *testing.T) {
	if got := KeysPath("/home/u"); got != "/home/u/.config/s3duck-tui/keys.json" {
		t.Errorf("KeysPath = %q", got)
	}
}
