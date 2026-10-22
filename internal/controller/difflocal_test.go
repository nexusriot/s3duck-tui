package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDiffCandidatePrefersTheLocalPane(t *testing.T) {
	got, err := localDiffCandidate("/work/site", "/home/u/Downloads/", "conf/", "conf/prod/app.yaml")
	if err != nil || got != filepath.Join("/work/site", "app.yaml") {
		t.Errorf("with a local pane: %q, %v — want the base name in the pane's directory", got, err)
	}
	got, err = localDiffCandidate("", "/home/u/Downloads/", "conf/", "conf/prod/app.yaml")
	if err != nil || got != filepath.Join("/home/u/Downloads", "prod", "app.yaml") {
		t.Errorf("without one: %q, %v — want where a download would have put it", got, err)
	}
	if _, err := localDiffCandidate("", "/dl/", "", "../../etc/passwd"); err == nil {
		t.Error("a key escaping the download directory must be refused, as for a download")
	}
}

func TestReadLocalCapped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readLocalCapped(p, 5); err != nil || string(data) != "12345" {
		t.Errorf("at the cap: %q, %v", data, err)
	}
	if _, err := readLocalCapped(p, 4); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("over the cap: %v, want a refusal naming the cap", err)
	}
	if _, err := readLocalCapped(dir, 100); err == nil {
		t.Error("a directory is not a file to diff")
	}
}

func TestCompareLocal(t *testing.T) {
	out, err := compareLocal([]byte("a\nb\n"), []byte("a\nb\n"), "l", "r")
	if err != nil || !out.identical {
		t.Errorf("same bytes: %+v %v", out, err)
	}

	out, _ = compareLocal([]byte("a\r\nb\r\n"), []byte("a\nb\n"), "l", "r")
	if out.identical || !strings.Contains(out.message, "line endings") {
		t.Errorf("CRLF vs LF must not be called identical: %+v", out)
	}

	out, _ = compareLocal([]byte{0, 1, 2}, []byte{0, 1, 3}, "l", "r")
	if !strings.Contains(out.message, "binary") {
		t.Errorf("binary: %+v", out)
	}

	out, err = compareLocal([]byte("a\nold\nc\nd\nold2\n"), []byte("a\nnew\nc\nd\nnew2\n"), "l", "r")
	if err != nil || out.body == "" {
		t.Fatalf("real change: %+v %v", out, err)
	}
	if len(out.changes) != 2 {
		t.Errorf("two separate changes should be two stops, got %v", out.changes)
	}
}

func TestNextChangeWraps(t *testing.T) {
	rows := []int{3, 10, 20}
	cases := []struct {
		top     int
		forward bool
		want    int
	}{
		{0, true, 3}, {3, true, 10}, {20, true, 3},
		{20, false, 10}, {3, false, 20}, {0, false, 20},
	}
	for _, c := range cases {
		if got := nextChange(rows, c.top, c.forward); got != c.want {
			t.Errorf("nextChange(top=%d, forward=%v) = %d, want %d", c.top, c.forward, got, c.want)
		}
	}
	if nextChange(nil, 0, true) != -1 {
		t.Error("no changes, no stop")
	}
}
