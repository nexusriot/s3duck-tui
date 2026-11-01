package model

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
)

// staleCopyServer fakes enough of S3 for a multipart copy whose source is now
// actualSize bytes with ETag "fresh", recording each part's range and If-Match.
type staleCopyServer struct {
	mu        sync.Mutex
	ranges    []string
	ifMatch   []string
	completed bool
}

func newStaleCopyServer(t *testing.T, actualSize int64) (*staleCopyServer, *fakeS3) {
	t.Helper()
	sv := &staleCopyServer{}
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprint(actualSize))
			w.Header().Set("ETag", `"fresh"`)
			w.Header().Set("Content-Type", "text/csv")
		case r.Method == http.MethodGet && q.Has("tagging"):
			_, _ = w.Write([]byte(`<Tagging><TagSet></TagSet></Tagging>`))
		case r.Method == http.MethodPost && q.Has("uploads"):
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>U</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPut && q.Has("partNumber"):
			sv.mu.Lock()
			sv.ranges = append(sv.ranges, r.Header.Get("x-amz-copy-source-range"))
			sv.ifMatch = append(sv.ifMatch, r.Header.Get("x-amz-copy-source-if-match"))
			sv.mu.Unlock()
			_, _ = w.Write([]byte(`<CopyPartResult><ETag>"p"</ETag></CopyPartResult>`))
		case r.Method == http.MethodPost && q.Has("uploadId"):
			sv.mu.Lock()
			sv.completed = true
			sv.mu.Unlock()
			_, _ = w.Write([]byte(`<CompleteMultipartUploadResult><ETag>"e-4"</ETag></CompleteMultipartUploadResult>`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	return sv, f
}

func shrinkCopyLimits(t *testing.T) {
	t.Helper()
	threshold, part := MultipartCopyThreshold, MultipartCopyPartSize
	t.Cleanup(func() { MultipartCopyThreshold, MultipartCopyPartSize = threshold, part })
	MultipartCopyThreshold = 5 << 20
	MultipartCopyPartSize = 5 << 20
}

// The ranges must cover the object as it is when the copy runs, not the size
// a listing reported. Planned from a stale 12 MiB, a 20 MiB object was copied
// as its first 12 MiB and the move then deleted the full source.
func TestMultipartCopyPlansFromTheCurrentSize(t *testing.T) {
	shrinkCopyLimits(t)
	sv, f := newStaleCopyServer(t, 20<<20)
	m := fakeModel(t, f, false)

	if err := m.CopyObjectSized(context.Background(), BucketObject("b"), BucketObject("b"), "src", "dst", 12<<20); err != nil {
		t.Fatal(err)
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sort.Strings(sv.ranges)
	want := []string{"bytes=0-5242879", "bytes=10485760-15728639", "bytes=15728640-20971519", "bytes=5242880-10485759"}
	if fmt.Sprint(sv.ranges) != fmt.Sprint(want) {
		t.Errorf("part ranges = %v\nwant (the current 20 MiB) %v", sv.ranges, want)
	}
	// Every part is conditional on the version the ranges were planned from.
	for i, im := range sv.ifMatch {
		if im != `"fresh"` {
			t.Errorf("part %d If-Match = %q, want \"fresh\"", i, im)
		}
	}
	if !sv.completed {
		t.Error("copy not completed")
	}
}

// A metadata save hands its own (possibly hours-old) attributes, size
// included; rewriting a large object in place from that size truncated it.
func TestMetadataSaveOfLargeObjectUsesCurrentSize(t *testing.T) {
	shrinkCopyLimits(t)
	sv, f := newStaleCopyServer(t, 20<<20)
	m := fakeModel(t, f, false)

	stale := ObjectMeta{Size: 12 << 20, ETag: "old", ContentType: "text/plain", UserMetadata: map[string]string{"k": "v"}}
	if err := m.PutObjectMeta(context.Background(), BucketObject("b"), "big.csv", stale); err != nil {
		t.Fatal(err)
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if len(sv.ranges) != 4 {
		t.Errorf("%d parts, want 4 covering 20 MiB: %v", len(sv.ranges), sv.ranges)
	}
	// The new metadata is the caller's, not the HEAD's.
	var created *http.Request
	for _, r := range f.requests() {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			created = r
		}
	}
	if created == nil || created.Header.Get("Content-Type") != "text/plain" || created.Header.Get("x-amz-meta-k") != "v" {
		t.Errorf("multipart create did not carry the edited metadata: %v", created)
	}
}
