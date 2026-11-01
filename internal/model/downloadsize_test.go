package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// serveBody answers every ranged GET with body as the whole object.
func serveBody(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(body))
	}
}

// A download whose byte count is not the listed size must not replace the
// target: here the object shrank between the listing and the download, and
// the old code renamed the short file over the user's copy as a success.
func TestDownloadRefusesASizeMismatch(t *testing.T) {
	f := newFakeS3(t, serveBody("short"))
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	target := filepath.Join(dir, "report.csv")
	if err := os.WriteFile(target, []byte("the user's existing copy"), 0600); err != nil {
		t.Fatal(err)
	}
	b := "b"
	_, err := m.DownloadTarget(context.Background(), DownloadTarget{Key: "report.csv", Size: 100}, "", dir, &b, true, nil)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want ErrSizeMismatch", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "the user's existing copy" {
		t.Errorf("target replaced with %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.s3duck-part")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestDownloadOfTheListedSizeSucceeds(t *testing.T) {
	f := newFakeS3(t, serveBody("hello"))
	m := fakeModel(t, f, false)
	dir := t.TempDir()
	b := "b"
	n, err := m.DownloadTarget(context.Background(), DownloadTarget{Key: "a/hello.txt", Size: 5}, "", dir, &b, false, nil)
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a", "hello.txt")); string(got) != "hello" {
		t.Errorf("file = %q", got)
	}
}
