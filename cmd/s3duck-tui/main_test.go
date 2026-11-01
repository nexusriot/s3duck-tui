package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture runs run(args) with stdout and stderr redirected, returning the
// status and what was printed. None of these paths reach the TUI.
func capture(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code = run(append([]string{"/usr/bin/s3duck-tui"}, args...))
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return code, string(o), string(e)
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunVersionAndHelp(t *testing.T) {
	code, out, _ := capture(t, "--version")
	if code != 0 || !strings.Contains(out, "0.14.0") {
		t.Errorf("--version: %d %q", code, out)
	}
	code, out, _ = capture(t, "-h")
	if code != 0 || !strings.Contains(out, "Usage: s3duck-tui") {
		t.Errorf("-h: %d %q", code, out)
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("S3DUCK_NOTIFY", "")
	two := writeConfig(t, `[{"name":"a"},{"name":"b"}]`)
	tests := []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{"unknown flag", []string{"--nope"}, "", "unknown flag --nope"},
		{"missing config", []string{"-c", "/nonexistent/c.json"}, "", "--config"},
		{"unknown profile", []string{"-c", two, "-p", "prod"}, "", `no profile named "prod"`},
		{"ambiguous location", []string{"-c", two, "s3://b/"}, "", "choose one with --profile"},
		{"bad env", []string{"-c", two}, "loud", "S3DUCK_NOTIFY"},
	}
	for _, tt := range tests {
		t.Setenv("S3DUCK_NOTIFY", tt.env)
		code, _, errOut := capture(t, tt.args...)
		if code != 2 || !strings.Contains(errOut, tt.wantErr) {
			t.Errorf("%s: status %d, stderr %q; want 2 and %q", tt.name, code, errOut, tt.wantErr)
		}
	}
}
