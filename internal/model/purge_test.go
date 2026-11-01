package model

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func ref(key, id string, marker, latest bool, size int64) VersionRef {
	return VersionRef{Key: key, VersionID: id, IsDeleteMark: marker, IsLatest: latest, Size: size}
}

func TestSelectPurge(t *testing.T) {
	refs := []VersionRef{
		ref("x", "3", false, true, 10),
		ref("x", "2", true, false, 0),
		ref("x", "1", false, false, 7),
		ref("x.bak", "9", false, true, 4), // a prefix sibling of "x"
		ref("dir/y", "5", true, true, 0),
		ref("dir/y", "4", false, false, 2),
	}
	keys := func(rs []VersionRef) string {
		var b []string
		for _, r := range rs {
			b = append(b, r.Key+"@"+r.VersionID)
		}
		return strings.Join(b, ",")
	}
	if got := keys(SelectPurge(refs, PurgeAll, "x")); got != "x@3,x@2,x@1" {
		t.Errorf("all versions of x = %s (x.bak must not be taken)", got)
	}
	if got := keys(SelectPurge(refs, PurgeNoncurrent, "x")); got != "x@2,x@1" {
		t.Errorf("old versions of x = %s, want the current one kept", got)
	}
	// A folder purge (no exact key) takes the subtree; noncurrent keeps each
	// key's latest entry, even when that is a delete marker.
	if got := keys(SelectPurge(refs, PurgeNoncurrent, "")); got != "x@2,x@1,dir/y@4" {
		t.Errorf("old versions under the prefix = %s", got)
	}

	s := SummarizePurge(SelectPurge(refs, PurgeAll, ""))
	if s.Keys != 3 || s.Versions != 4 || s.Markers != 2 || s.Bytes != 23 {
		t.Errorf("summary = %+v", s)
	}
}

// versionsServer fakes ListObjectVersions over a fixed set of refs, served
// in pages of pageSize, and records DeleteObjects bodies.
type versionsServer struct {
	mu      sync.Mutex
	deletes []deleteBody
	// failVersion makes DeleteObjects report this version ID as failed.
	failVersion string
	// stall serves a truncated page without new markers.
	stall bool
}

type deleteBody struct {
	Objects []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"Object"`
}

func (vs *versionsServer) handler(t *testing.T, refs []VersionRef, pageSize int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && q.Has("versions"):
			start := 0
			if km := q.Get("key-marker"); km != "" {
				fmt.Sscan(km, &start)
			}
			end := start + pageSize
			if end > len(refs) {
				end = len(refs)
			}
			var b strings.Builder
			b.WriteString(`<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
			for _, x := range refs[start:end] {
				tag := "Version"
				if x.IsDeleteMark {
					tag = "DeleteMarker"
				}
				fmt.Fprintf(&b, `<%s><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%v</IsLatest><Size>%d</Size><LastModified>%s</LastModified></%s>`,
					tag, x.Key, x.VersionID, x.IsLatest, x.Size, x.LastModified.UTC().Format(time.RFC3339), tag)
			}
			switch {
			case end < len(refs) && vs.stall:
				// More results, but no markers to fetch them with.
				b.WriteString(`<IsTruncated>true</IsTruncated>`)
			case end < len(refs):
				fmt.Fprintf(&b, `<IsTruncated>true</IsTruncated><NextKeyMarker>%d</NextKeyMarker><NextVersionIdMarker>v</NextVersionIdMarker>`, end)
			default:
				b.WriteString(`<IsTruncated>false</IsTruncated>`)
			}
			b.WriteString(`</ListVersionsResult>`)
			_, _ = w.Write([]byte(b.String()))
		case r.Method == http.MethodPost && q.Has("delete"):
			body, _ := io.ReadAll(r.Body)
			var d deleteBody
			if err := xml.Unmarshal(body, &d); err != nil {
				t.Errorf("delete body: %v\n%s", err, body)
			}
			vs.mu.Lock()
			vs.deletes = append(vs.deletes, d)
			fail := vs.failVersion
			vs.mu.Unlock()
			var out strings.Builder
			out.WriteString(`<DeleteResult>`)
			for _, o := range d.Objects {
				if o.VersionID == fail {
					fmt.Fprintf(&out, `<Error><Key>%s</Key><VersionId>%s</VersionId><Code>AccessDenied</Code><Message>locked</Message></Error>`, o.Key, o.VersionID)
				}
			}
			out.WriteString(`</DeleteResult>`)
			_, _ = w.Write([]byte(out.String()))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func stamped(refs []VersionRef) []VersionRef {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range refs {
		at := base.Add(-time.Duration(i) * time.Hour)
		refs[i].LastModified = &at
	}
	return refs
}

func TestListVersionsUnderPagesAndRefusesAStall(t *testing.T) {
	refs := stamped([]VersionRef{ref("a", "1", false, true, 1), ref("b", "2", true, true, 0), ref("c", "3", false, true, 3)})
	vs := &versionsServer{}
	f := newFakeS3(t, vs.handler(t, refs, 2))
	var got []VersionRef
	if err := fakeModel(t, f, false).ListVersionsUnder(context.Background(), BucketObject("b"), "", func(p []VersionRef) bool {
		got = append(got, p...)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[1].IsDeleteMark || got[2].Size != 3 {
		t.Errorf("listed %+v", got)
	}

	stall := &versionsServer{stall: true}
	f2 := newFakeS3(t, stall.handler(t, refs, 2))
	err := fakeModel(t, f2, false).ListVersionsUnder(context.Background(), BucketObject("b"), "", func([]VersionRef) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "continuation marker") {
		t.Errorf("a stalled listing ended with %v; a purge planned from it would leave versions behind", err)
	}
}

// Every delete names its version — "null" included, the ID an object written
// without versioning carries — or the request writes a marker instead of
// removing anything. Batches stay within S3's 1000; a refused version is
// reported, and the rest still go.
func TestDeleteVersionsSendsVersionIDsAndReportsRefusals(t *testing.T) {
	var refs []VersionRef
	for i := 0; i < 1200; i++ {
		refs = append(refs, ref(fmt.Sprintf("k%04d", i), fmt.Sprint("v", i), false, false, 1))
	}
	refs[5].VersionID = "null"
	vs := &versionsServer{failVersion: "v7"}
	f := newFakeS3(t, vs.handler(t, nil, 1))
	n, failures, err := fakeModel(t, f, false).DeleteVersions(context.Background(), BucketObject("b"), refs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1199 || len(failures) != 1 || failures[0].Ref.VersionID != "v7" {
		t.Errorf("deleted %d, failures %+v; want 1199 and v7 refused", n, failures)
	}
	if len(vs.deletes) != 2 || len(vs.deletes[0].Objects) != 1000 || len(vs.deletes[1].Objects) != 200 {
		t.Errorf("batches = %d", len(vs.deletes))
	}
	if o := vs.deletes[0].Objects[5]; o.VersionID != "null" {
		t.Errorf("the null version was sent as %q", o.VersionID)
	}
	for _, o := range vs.deletes[0].Objects {
		if o.VersionID == "" {
			t.Fatalf("%s was sent without a version id: that writes a delete marker", o.Key)
		}
	}
}

// A deleted key is one whose latest entry is a delete marker; what comes back
// on undelete is the newest real version beneath it.
func TestFindDeleted(t *testing.T) {
	refs := stamped([]VersionRef{
		ref("gone.txt", "m2", true, true, 0),
		ref("gone.txt", "v2", false, false, 20),
		ref("gone.txt", "v1", false, false, 10),
		ref("live.txt", "v9", false, true, 5),
		ref("live.txt", "m1", true, false, 0), // deleted once, then rewritten
		ref("bare.txt", "m3", true, true, 0),  // a marker over nothing
	})
	vs := &versionsServer{}
	f := newFakeS3(t, vs.handler(t, refs, 2))
	found, truncated, err := fakeModel(t, f, false).FindDeleted(context.Background(), BucketObject("b"), "", 0, nil)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if len(found) != 2 {
		t.Fatalf("found %+v, want gone.txt and bare.txt", found)
	}
	byKey := map[string]DeletedObject{}
	for _, d := range found {
		byKey[d.Key] = d
	}
	if g := byKey["gone.txt"]; g.MarkerID != "m2" || g.Size != 20 || g.Versions != 2 {
		t.Errorf("gone.txt = %+v, want marker m2, newest version 20 bytes, 2 versions", g)
	}
	if b := byKey["bare.txt"]; b.Versions != 0 {
		t.Errorf("bare.txt = %+v", b)
	}

	capped, truncated, err := fakeModel(t, f, false).FindDeleted(context.Background(), BucketObject("b"), "", 1, nil)
	if err != nil || !truncated || len(capped) != 1 {
		t.Errorf("cap 1: %d found, truncated=%v, err=%v", len(capped), truncated, err)
	}
}

// Emptying a bucket deletes every version by ID, so a versioned bucket can
// then be deleted; it used to write markers and leave the bucket non-empty.
func TestEmptyBucketPurgesEveryVersion(t *testing.T) {
	refs := stamped([]VersionRef{ref("a", "2", false, true, 1), ref("a", "1", false, false, 1), ref("b", "3", true, true, 0)})
	vs := &versionsServer{}
	f := newFakeS3(t, vs.handler(t, refs, 1000))
	if err := fakeModel(t, f, false).EmptyBucket(context.Background(), BucketObject("b")); err != nil {
		t.Fatal(err)
	}
	if len(vs.deletes) != 1 || len(vs.deletes[0].Objects) != 3 {
		t.Fatalf("deletes = %+v", vs.deletes)
	}
	for _, o := range vs.deletes[0].Objects {
		if o.VersionID == "" {
			t.Errorf("%s deleted without a version id", o.Key)
		}
	}
}

func TestUndeleteRemovesOnlyTheMarker(t *testing.T) {
	var got *http.Request
	f := newFakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusNoContent)
	})
	m := fakeModel(t, f, false)
	if err := m.Undelete(context.Background(), BucketObject("b"), DeletedObject{Key: "gone.txt", MarkerID: "m2"}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodDelete || got.URL.Query().Get("versionId") != "m2" {
		t.Errorf("undelete sent %s %s, want DELETE ?versionId=m2", got.Method, got.URL)
	}
	if err := m.Undelete(context.Background(), BucketObject("b"), DeletedObject{Key: "x"}); err == nil {
		t.Error("an undelete with no marker id was sent")
	}
}

func TestIsAccessDenied(t *testing.T) {
	f := newFakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>no</Message></Error>`))
	})
	_, err := fakeModel(t, f, false).ListBuckets(context.Background())
	if !IsAccessDenied(err) {
		t.Errorf("IsAccessDenied(%v) = false", err)
	}
	if IsAccessDenied(errors.New("connection refused")) {
		t.Error("a network error read as access denied")
	}
}
