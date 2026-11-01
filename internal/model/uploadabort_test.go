package model

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A cancelled multipart upload must still be aborted. The SDK's own cleanup
// sends the abort on the upload's context — the one the user just cancelled —
// so it never left the client, and the parts stayed on the server, billed.
func TestCancelledUploadIsAborted(t *testing.T) {
	partStarted := make(chan struct{}, 8)
	aborted := make(chan string, 1)
	var once sync.Once
	release := make(chan struct{})
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>UP-1</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPut && q.Has("partNumber"):
			select {
			case partStarted <- struct{}{}:
			default:
			}
			// Hold the part until the client gives up on it (or the test
			// ends: the server's Close waits for every handler).
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case r.Method == http.MethodDelete && q.Has("uploadId"):
			once.Do(func() { aborted <- q.Get("uploadId") })
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	// Registered after the server, so it runs before the server's Close.
	t.Cleanup(func() { close(release) })
	m := fakeModel(t, f, false)

	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local, make([]byte, 11<<20), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-partStarted
		cancel()
	}()
	err := m.UploadFile(ctx, local, "dir/big.bin", BucketObject("b"), nil)
	if err == nil {
		t.Fatal("a cancelled upload reported success")
	}
	select {
	case id := <-aborted:
		if id != "UP-1" {
			t.Errorf("aborted upload %q, want UP-1", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled multipart upload was never aborted")
	}
}
