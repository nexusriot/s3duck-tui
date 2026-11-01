package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeS3 is just enough of an S3 endpoint to see what a client sends: it
// records each request's method, path, query and Authorization header, and
// answers every call with the handler's canned response.
type fakeS3 struct {
	mu   sync.Mutex
	reqs []*http.Request
	srv  *httptest.Server
}

func newFakeS3(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeS3 {
	t.Helper()
	f := &fakeS3{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		f.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.reqs...)
}

func fakeModel(t *testing.T, f *fakeS3, anonymous bool) *Model {
	t.Helper()
	region := "us-east-1"
	cf := NewConfig(f.srv.URL, &region, "AKIDEXAMPLE", "secret", "", true, 0)
	cf.Anonymous = anonymous
	m, err := NewModel(cf)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	return m
}

const emptyListing = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>pub</Name><Prefix>logs/</Prefix><KeyCount>1</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>logs/a.txt</Key><Size>3</Size></Contents></ListBucketResult>`

// The whole point of an anonymous profile: requests go out with no
// signature at all, even though the profile still holds (ignored) keys.
func TestAnonymousRequestsAreUnsigned(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(emptyListing))
	})
	ok, err := fakeModel(t, f, true).HasPrefix(context.Background(), BucketObject("pub"), "logs/")
	if err != nil || !ok {
		t.Fatalf("HasPrefix = %v, %v; want true, nil", ok, err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	if auth := reqs[0].Header.Get("Authorization"); auth != "" {
		t.Errorf("anonymous request carried Authorization %q", auth)
	}
	if got := reqs[0].URL.Query().Get("max-keys"); got != "1" {
		t.Errorf("max-keys = %q, want 1 (a probe, not a listing)", got)
	}

	// The same call from a keyed profile is signed — so the test above is
	// observing the anonymous switch, not a server that drops the header.
	f2 := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(emptyListing))
	})
	if _, err := fakeModel(t, f2, false).HasPrefix(context.Background(), BucketObject("pub"), "logs/"); err != nil {
		t.Fatal(err)
	}
	if auth := f2.requests()[0].Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("signed request Authorization = %q", auth)
	}
}

func TestHasPrefixEmpty(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>pub</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`))
	})
	ok, err := fakeModel(t, f, false).HasPrefix(context.Background(), BucketObject("pub"), "nothing/")
	if err != nil || ok {
		t.Errorf("HasPrefix on an empty prefix = %v, %v; want false, nil", ok, err)
	}
}

// An anonymous profile never calls ListBuckets (S3 refuses it unsigned); it
// lists the names it was given.
func TestAnonymousListBucketsUsesPublicBuckets(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	m := fakeModel(t, f, true)
	m.Cf.PublicBuckets = []string{" open-data ", "", "noaa", "open-data"}
	got, err := m.ListBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range got {
		if b.Ot != Bucket {
			t.Errorf("%s is not a bucket row", *b.Key)
		}
		names = append(names, *b.Key)
	}
	if strings.Join(names, ",") != "open-data,noaa" {
		t.Errorf("buckets = %v, want [open-data noaa] (trimmed, deduplicated, in order)", names)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("listing public buckets sent %d request(s)", n)
	}

	m.Cf.PublicBuckets = nil
	if _, err := m.ListBuckets(context.Background()); !errors.Is(err, ErrNoPublicBuckets) {
		t.Errorf("no names: err = %v, want ErrNoPublicBuckets", err)
	}
}

func TestCredentialSourceAnonymous(t *testing.T) {
	cf := Config{Anonymous: true, AWSProfile: "ignored"}
	if got := cf.CredentialSource(); !strings.Contains(got, "anonymous") {
		t.Errorf("CredentialSource = %q", got)
	}
}

func TestPresignPutURL(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {})
	m := fakeModel(t, f, false)
	b := BucketObject("inbox")

	link, err := m.PresignPutURL(b, "drop/report.pdf", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u.Path, "/inbox/drop/report.pdf") {
		t.Errorf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "3600" {
		t.Errorf("X-Amz-Expires = %q, want 3600", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("link is not signed")
	}
	// Only the host is signed: anything more would be a header the uploader
	// has to reproduce exactly.
	if sh := q.Get("X-Amz-SignedHeaders"); sh != "host" {
		t.Errorf("signed headers = %q, want host", sh)
	}

	long, err := m.PresignPutURL(b, "k", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if lu, _ := url.Parse(long); lu.Query().Get("X-Amz-Expires") != "604800" {
		t.Errorf("TTL not capped at 7 days: %s", lu.Query().Get("X-Amz-Expires"))
	}

	for _, key := range []string{"", "folder/"} {
		if _, err := m.PresignPutURL(b, key, time.Hour); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
	if _, err := m.PresignPutURL(b, "k", 0); err == nil {
		t.Error("zero TTL accepted")
	}
	if _, err := fakeModel(t, f, true).PresignPutURL(b, "k", time.Hour); !errors.Is(err, ErrAnonymousCannotSign) {
		t.Errorf("anonymous: err = %v, want ErrAnonymousCannotSign", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("presigning sent %d request(s); it is local", n)
	}
}

func TestSetBucketVersioningSendsStatus(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
	})
	m := fakeModel(t, f, false)
	for _, on := range []bool{true, false} {
		if err := m.SetBucketVersioning(context.Background(), BucketObject("b"), on); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	for i, want := range []string{"<Status>Enabled</Status>", "<Status>Suspended</Status>"} {
		if reqs[i].Method != http.MethodPut || !reqs[i].URL.Query().Has("versioning") {
			t.Errorf("request %d = %s %s, want PUT ?versioning", i, reqs[i].Method, reqs[i].URL)
		}
		if !strings.Contains(bodies[i], want) {
			t.Errorf("request %d body %q lacks %s", i, bodies[i], want)
		}
	}
	if err := m.SetBucketVersioning(context.Background(), nil, true); err == nil {
		t.Error("nil bucket accepted")
	}
}

// Unsigned, a "presigned" GET is just the object's URL; the expiry the
// presigner tacks on would promise a deadline nothing enforces.
func TestAnonymousPresignGetIsPlainURL(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {})
	link, err := fakeModel(t, f, true).PresignGetURL(BucketObject("b"), "dir/a b.txt", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.srv.URL + "/b/dir/a%20b.txt"; link != want {
		t.Errorf("link = %q, want %q", link, want)
	}
}

// An anonymous profile cannot call GetBucketLocation (it needs
// s3:GetBucketLocation, refused to every unsigned caller); RefreshClient
// asks with an unsigned HEAD and takes the x-amz-bucket-region header, which
// S3 sends whatever the access decision — here, with a 403.
func TestAnonymousRefreshClientUsesHeadRegion(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-bucket-region", "eu-west-1")
		w.WriteHeader(http.StatusForbidden)
	})
	m := fakeModel(t, f, true)
	b := "pub"
	if err := m.RefreshClient(&b); err != nil {
		t.Fatalf("RefreshClient: %v", err)
	}
	if got := aws.ToString(m.Cf.Region); got != "eu-west-1" {
		t.Errorf("region = %q, want eu-west-1", got)
	}
	reqs := f.requests()
	if len(reqs) == 0 {
		t.Fatal("no request sent")
	}
	for _, r := range reqs {
		if r.Method != http.MethodHead {
			t.Errorf("anonymous region lookup sent %s %s, want HEAD only", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anonymous region lookup was signed")
		}
	}
}

// A keyed profile still asks GetBucketLocation (GET ?location), and a failed
// lookup leaves the region untouched.
func TestKeyedRefreshClientUsesGetBucketLocation(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">ap-south-1</LocationConstraint>`))
	})
	m := fakeModel(t, f, false)
	b := "priv"
	if err := m.RefreshClient(&b); err != nil {
		t.Fatalf("RefreshClient: %v", err)
	}
	if got := aws.ToString(m.Cf.Region); got != "ap-south-1" {
		t.Errorf("region = %q, want ap-south-1", got)
	}
	if r := f.requests()[0]; r.Method != http.MethodGet || !r.URL.Query().Has("location") {
		t.Errorf("keyed lookup = %s %s, want GET ?location", r.Method, r.URL)
	}

	broken := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	m2 := fakeModel(t, broken, true)
	if err := m2.RefreshClient(&b); err == nil {
		t.Error("an anonymous lookup with no region header succeeded")
	}
	if got := aws.ToString(m2.Cf.Region); got != "us-east-1" {
		t.Errorf("a failed lookup changed the region to %q", got)
	}
}

func TestHasPrefixErrors(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	m := fakeModel(t, f, true)
	if _, err := m.HasPrefix(context.Background(), nil, "x/"); err == nil {
		t.Error("nil bucket accepted")
	}
	// A nil context is defaulted, not dereferenced; the 403 is still an error.
	var noCtx context.Context
	if _, err := m.HasPrefix(noCtx, BucketObject("b"), "x/"); err == nil {
		t.Error("a 403 read as a successful probe")
	}
}

func TestCredentialSourceBranches(t *testing.T) {
	for _, tt := range []struct {
		cf   Config
		want string
	}{
		{Config{AWSProfile: "sso"}, "aws profile sso"},
		{Config{SessionToken: "t"}, "session token"},
		{Config{}, "static keys"},
	} {
		if got := tt.cf.CredentialSource(); !strings.Contains(got, tt.want) {
			t.Errorf("%+v: %q lacks %q", tt.cf, got, tt.want)
		}
	}
}
