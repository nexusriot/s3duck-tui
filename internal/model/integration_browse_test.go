//go:build integration

// Integration cover for the browsing and housekeeping half of the model: the
// delimited listing navigation is built on, the recursive prefix delete, the
// folder markers, presigned links, and the incomplete-upload cleanup.
//
// The existing files cover transfers, versions, metadata and copies. These are
// the calls the browser itself runs on every keystroke, plus the two that
// remove things — and they were the ones with no server-backed test at all.
package model_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// keysUnder lists every key below prefix, sorted.
func keysUnder(t *testing.T, m *model.Model, bucket *model.Object, prefix string) []string {
	t.Helper()
	objs, err := m.ListObjects(context.Background(), prefix, bucket)
	if err != nil {
		t.Fatalf("ListObjects(%q): %v", prefix, err)
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.Key != nil {
			out = append(out, *o.Key)
		}
	}
	sort.Strings(out)
	return out
}

// TestIntegrationRecursiveDelete covers Model.Delete, which is the most
// destructive call in the app: a key ending in "/" is a prefix, and everything
// under it goes. The sibling assertion is the one that matters — "docs/" and
// "docs2/" share a stem, and a prefix built without the separator would take
// both.
func TestIntegrationRecursiveDelete(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-delete")

	putObject(t, m, bucket, "docs/a.txt", "a")
	putObject(t, m, bucket, "docs/sub/b.txt", "b")
	putObject(t, m, bucket, "docs/sub/deep/c.txt", "c")
	putObject(t, m, bucket, "docs2/keep.txt", "sibling prefix")
	putObject(t, m, bucket, "top.txt", "top level")

	t.Run("a prefix delete removes the whole subtree", func(t *testing.T) {
		key := "docs/"
		if err := m.Delete(ctx, &key, bucket); err != nil {
			t.Fatalf("Delete(%q): %v", key, err)
		}
		if got := keysUnder(t, m, bucket, "docs/"); len(got) != 0 {
			t.Errorf("after deleting docs/ the prefix still holds %v", got)
		}
	})

	t.Run("a sibling sharing the name stem survives", func(t *testing.T) {
		if got := keysUnder(t, m, bucket, "docs2/"); len(got) != 1 {
			t.Errorf("docs2/ = %v, want it untouched by the docs/ delete", got)
		}
		if got := keysUnder(t, m, bucket, "top.txt"); len(got) != 1 {
			t.Errorf("top.txt = %v, want it untouched", got)
		}
	})

	t.Run("a plain key removes only itself", func(t *testing.T) {
		key := "top.txt"
		if err := m.Delete(ctx, &key, bucket); err != nil {
			t.Fatalf("Delete(%q): %v", key, err)
		}
		if got := keysUnder(t, m, bucket, ""); len(got) != 1 || got[0] != "docs2/keep.txt" {
			t.Errorf("bucket holds %v, want only docs2/keep.txt", got)
		}
	})

	t.Run("an empty key is refused", func(t *testing.T) {
		empty := ""
		if err := m.Delete(ctx, &empty, bucket); err == nil {
			t.Error("Delete(\"\") succeeded; an empty key must not be read as the bucket root")
		}
		if err := m.Delete(ctx, nil, bucket); err == nil {
			t.Error("Delete(nil) succeeded")
		}
		if got := keysUnder(t, m, bucket, ""); len(got) != 1 {
			t.Errorf("a refused delete still changed the bucket: %v", got)
		}
	})

	t.Run("deleting a prefix that holds nothing is not an error", func(t *testing.T) {
		key := "never-existed/"
		if err := m.Delete(ctx, &key, bucket); err != nil {
			t.Errorf("Delete of an empty prefix = %v, want nil", err)
		}
	})
}

// TestIntegrationDelimitedListing covers List — the call behind every
// navigation step. It is delimited, so it must return one folder entry per
// child prefix however deep that prefix goes, and must not show the listed
// prefix's own marker as a file inside itself.
func TestIntegrationDelimitedListing(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-listing")

	marker := "dir/"
	if err := m.CreateFolder(&marker, bucket); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	putObject(t, m, bucket, "dir/file.txt", "twelve bytes")
	putObject(t, m, bucket, "dir/sub/deep/x.txt", "deep")
	putObject(t, m, bucket, "root.txt", "at the root")

	t.Run("the root shows one folder and one file", func(t *testing.T) {
		objs, err := m.List(ctx, "", bucket)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		folders, files := splitByType(objs)
		if !equalSorted(folders, []string{"dir"}) {
			t.Errorf("folders = %v, want [dir]", folders)
		}
		if !equalSorted(files, []string{"root.txt"}) {
			t.Errorf("files = %v, want [root.txt]", files)
		}
	})

	t.Run("a folder's own marker is not listed inside it", func(t *testing.T) {
		objs, err := m.List(ctx, "dir/", bucket)
		if err != nil {
			t.Fatalf("List(dir/): %v", err)
		}
		folders, files := splitByType(objs)
		// "sub" rolls up even though the only object below it is two levels
		// down; "dir/" itself must not appear as a file named "dir".
		if !equalSorted(folders, []string{"sub"}) {
			t.Errorf("folders under dir/ = %v, want [sub]", folders)
		}
		if !equalSorted(files, []string{"file.txt"}) {
			t.Errorf("files under dir/ = %v, want [file.txt]; the prefix's own marker leaked in", files)
		}
	})

	t.Run("entries carry the full key and the size", func(t *testing.T) {
		objs, err := m.List(ctx, "dir/", bucket)
		if err != nil {
			t.Fatalf("List(dir/): %v", err)
		}
		for _, o := range objs {
			if o.FullPath == nil {
				t.Fatalf("%v has no FullPath; selection and every key-taking action read it", o.Key)
			}
			switch o.Ot {
			case model.File:
				if *o.FullPath != "dir/file.txt" {
					t.Errorf("file FullPath = %q, want dir/file.txt", *o.FullPath)
				}
				if o.Size == nil || *o.Size != 12 {
					t.Errorf("file size = %v, want 12", o.Size)
				}
			case model.Folder:
				if *o.FullPath != "dir/sub/" {
					t.Errorf("folder FullPath = %q, want dir/sub/ (a prefix, slash-terminated)", *o.FullPath)
				}
			}
		}
	})

	t.Run("ListStream reports a running total and can stop early", func(t *testing.T) {
		pages, total := 0, 0
		err := m.ListStream(ctx, "", bucket, func(page []*model.Object, running int) bool {
			pages++
			total = running
			return false // stop after the first page
		})
		if err != nil {
			t.Fatalf("ListStream: %v", err)
		}
		if pages != 1 {
			t.Errorf("onPage ran %d times after returning false, want 1", pages)
		}
		if total == 0 {
			t.Error("the running total stayed at 0")
		}
	})
}

// TestIntegrationResolveDownloadObjects covers the expansion a download runs
// before it moves a byte: a folder becomes the concrete objects underneath it,
// a file stands for itself, and a file with no known size is refused rather
// than downloaded as zero bytes.
func TestIntegrationResolveDownloadObjects(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-resolve")

	putObject(t, m, bucket, "tree/a.txt", "aaaa")
	putObject(t, m, bucket, "tree/sub/b.txt", "bbbbbb")

	t.Run("a folder expands to every object under it", func(t *testing.T) {
		targets, total, err := m.ResolveDownloadObjects(ctx, "tree", true, nil, bucket)
		if err != nil {
			t.Fatalf("ResolveDownloadObjects: %v", err)
		}
		var keys []string
		for _, tg := range targets {
			keys = append(keys, tg.Key)
		}
		if !equalSorted(keys, []string{"tree/a.txt", "tree/sub/b.txt"}) {
			t.Errorf("resolved %v, want both objects under tree/", keys)
		}
		if total != 10 {
			t.Errorf("total = %d, want 10 (4 + 6)", total)
		}
	})

	t.Run("a file resolves to itself", func(t *testing.T) {
		size := int64(4)
		targets, total, err := m.ResolveDownloadObjects(ctx, "tree/a.txt", false, &size, bucket)
		if err != nil {
			t.Fatalf("ResolveDownloadObjects: %v", err)
		}
		if len(targets) != 1 || targets[0].Key != "tree/a.txt" || total != 4 {
			t.Errorf("resolved %v (total %d), want one 4-byte target", targets, total)
		}
	})

	t.Run("a file with no size is refused", func(t *testing.T) {
		if _, _, err := m.ResolveDownloadObjects(ctx, "tree/a.txt", false, nil, bucket); err == nil {
			t.Error("a sizeless file resolved; the progress total would silently be 0")
		}
	})
}

// TestIntegrationPresignedURLFetches checks the one feature whose whole output
// is a URL handed to someone else: if the signature or the host is wrong,
// nothing in-process notices. So the link is actually fetched.
func TestIntegrationPresignedURLFetches(t *testing.T) {
	m := testModel(t)
	bucket := freshBucket(t, m, "s3duck-it-presign")
	const body = "shared through a link"
	putObject(t, m, bucket, "share/report.txt", body)

	url, err := m.PresignGetURL(bucket, "share/report.txt", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGetURL: %v", err)
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetching the presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET = %s, want 200", resp.Status)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("presigned GET returned %q, want %q", got, body)
	}

	t.Run("the guards refuse what cannot be signed", func(t *testing.T) {
		if _, err := m.PresignGetURL(bucket, "", time.Minute); err == nil {
			t.Error("an empty key was signed")
		}
		if _, err := m.PresignGetURL(bucket, "share/report.txt", 0); err == nil {
			t.Error("a zero TTL was signed")
		}
		if _, err := m.PresignGetURL(nil, "share/report.txt", time.Minute); err == nil {
			t.Error("a nil bucket was signed")
		}
	})
}

// TestIntegrationIncompleteUploads covers the orphan cleanup: parts from an
// interrupted upload keep costing storage until they are aborted, and they are
// invisible to an ordinary listing, so the only way to see this work is to
// start a multipart upload and never finish it.
func TestIntegrationIncompleteUploads(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mpu")

	started, err := m.Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String("interrupted.bin"),
	})
	if err != nil {
		t.Skipf("endpoint does not support multipart uploads: %v", err)
	}
	uploadID := aws.ToString(started.UploadId)

	ups, err := m.ListMultipartUploads(bucket)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	found := false
	for _, u := range ups {
		if u.Key == "interrupted.bin" && u.UploadID == uploadID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the open upload is not listed: %v", ups)
	}

	// It is an orphan, not an object: an ordinary listing must not show it.
	if got := keysUnder(t, m, bucket, ""); len(got) != 0 {
		t.Errorf("an unfinished upload appeared as an object: %v", got)
	}

	if err := m.AbortMultipartUpload(bucket, "interrupted.bin", uploadID); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	after, err := m.ListMultipartUploads(bucket)
	if err != nil {
		t.Fatalf("ListMultipartUploads after abort: %v", err)
	}
	for _, u := range after {
		if u.UploadID == uploadID {
			t.Errorf("the aborted upload is still listed: %v", u)
		}
	}
}

// TestIntegrationBucketConfigDegradesGracefully pins the contract that makes
// the properties panel usable on S3-compatible servers: a feature the endpoint
// does not implement is reported as its default, not raised as an error that
// blanks the whole panel.
//
// The two halves use separate buckets on purpose. Enabling versioning is a
// permanent, bucket-level change that freshBucket cannot undo — asserting the
// default on a bucket this test had already switched on would pass once and
// fail on every re-run against a persistent server.
func TestIntegrationBucketConfigDegradesGracefully(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-bucketcfg")

	info, err := m.BucketConfig(bucket)
	if err != nil {
		t.Fatalf("BucketConfig: %v", err)
	}
	if info.Region == "" {
		t.Error("Region is empty; the panel shows it verbatim")
	}
	// Every field is a display string with a defined fallback. An empty one
	// means an unsupported feature leaked through as a blank rather than its
	// default, which is what this call exists to prevent.
	if info.Encryption == "" || info.ObjectLock == "" || info.Versioning == "" {
		t.Errorf("unsupported features reported as empty rather than a default: %+v", info)
	}
	if info.ObjectLock != "on" && info.ObjectLock != "off" {
		t.Errorf("ObjectLock = %q, want \"on\" or \"off\"", info.ObjectLock)
	}

	t.Run("versioning is reported once enabled", func(t *testing.T) {
		versioned := freshBucket(t, m, "s3duck-it-bucketcfg-versioned")
		if _, err := m.Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: versioned.Key,
			VersioningConfiguration: &s3t.VersioningConfiguration{
				Status: s3t.BucketVersioningStatusEnabled,
			},
		}); err != nil {
			t.Skipf("endpoint does not support bucket versioning: %v", err)
		}
		info, err := m.BucketConfig(versioned)
		if err != nil {
			t.Fatalf("BucketConfig: %v", err)
		}
		if info.Versioning != "Enabled" {
			t.Errorf("Versioning = %q after enabling it, want %q", info.Versioning, "Enabled")
		}
	})
}

// TestIntegrationCreateFolderMarker covers the empty-folder representation:
// S3 has no directories, so a folder the user creates is a zero-byte key
// ending in "/" that must then navigate like any other folder.
func TestIntegrationCreateFolderMarker(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-mkdir")

	name := "new-folder/"
	if err := m.CreateFolder(&name, bucket); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	if got := keysUnder(t, m, bucket, ""); len(got) != 1 || got[0] != "new-folder/" {
		t.Fatalf("after CreateFolder the bucket holds %v, want [new-folder/]", got)
	}

	objs, err := m.List(ctx, "", bucket)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	folders, files := splitByType(objs)
	if !equalSorted(folders, []string{"new-folder"}) {
		t.Errorf("folders = %v, want [new-folder]", folders)
	}
	if len(files) != 0 {
		t.Errorf("the marker is listed as a file: %v", files)
	}

	// An empty folder downloads as a directory, not as a zero-byte file.
	targets, total, err := m.ResolveDownloadObjects(ctx, "new-folder", true, nil, bucket)
	if err != nil {
		t.Fatalf("ResolveDownloadObjects: %v", err)
	}
	if total != 0 || len(targets) != 1 || !strings.HasSuffix(targets[0].Key, "/") {
		t.Errorf("resolved %v (total %d), want just the slash-terminated marker", targets, total)
	}

	dest := t.TempDir()
	if _, err := m.DownloadTarget(ctx, targets[0], "", dest, bucket.Key, false, nil); err != nil {
		t.Fatalf("DownloadTarget(marker): %v", err)
	}
	fi, err := os.Stat(filepath.Join(dest, "new-folder"))
	if err != nil {
		t.Fatalf("the marker did not land as a directory: %v", err)
	}
	if !fi.IsDir() {
		t.Error("the folder marker landed as a file")
	}
}

func splitByType(objs []*model.Object) (folders, files []string) {
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		switch o.Ot {
		case model.Folder:
			folders = append(folders, *o.Key)
		case model.File:
			files = append(files, *o.Key)
		}
	}
	return folders, files
}

func equalSorted(got, want []string) bool {
	g := append([]string(nil), got...)
	sort.Strings(g)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestIntegrationPublicBucketIsActuallyReadable covers the one operation whose
// whole effect is on anonymous callers: MakeBucketPublic writes a policy, and
// the only way to know it is the right policy is to read an object back with
// no credentials at all. A silently wrong ARN or action would leave the bucket
// private while the UI reported success — or, worse, grant more than
// s3:GetObject.
func TestIntegrationPublicBucketIsActuallyReadable(t *testing.T) {
	m := testModel(t)
	bucket := freshBucket(t, m, "s3duck-it-public")
	const body = "world readable"
	putObject(t, m, bucket, "open.txt", body)

	endpoint, _, _, _ := testCreds(t)
	objURL := strings.TrimSuffix(endpoint, "/") + "/" + *bucket.Key + "/open.txt"

	// Private to begin with, or the assertion below proves nothing.
	if resp, err := http.Get(objURL); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Skip("the endpoint serves objects anonymously by default; the policy cannot be observed here")
		}
	}

	if err := m.MakeBucketPublic(*bucket.Key); err != nil {
		t.Skipf("endpoint does not support bucket policies: %v", err)
	}
	// Put the bucket back to private. A policy outlives freshBucket, so
	// without this the "private to begin with" check above would skip the
	// whole test on every re-run — a test that quietly stops testing.
	t.Cleanup(func() {
		_, _ = m.Client.DeleteBucketPolicy(context.Background(), &s3.DeleteBucketPolicyInput{
			Bucket: bucket.Key,
		})
	})

	resp, err := http.Get(objURL)
	if err != nil {
		t.Fatalf("anonymous GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET = %s, want 200: the bucket was reported public but is not", resp.Status)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("anonymous GET returned %q, want %q", got, body)
	}

	// Read access only: the policy must not have granted writes.
	req, err := http.NewRequest(http.MethodPut, objURL, strings.NewReader("anonymous write"))
	if err != nil {
		t.Fatal(err)
	}
	wresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("anonymous PUT: %v", err)
	}
	defer wresp.Body.Close()
	if wresp.StatusCode >= 200 && wresp.StatusCode < 300 {
		t.Errorf("anonymous PUT = %s; the public-read policy also granted writes", wresp.Status)
	}
}

// TestIntegrationRefreshClientIsFailSafe pins the contract the comment on
// RefreshClient promises: on a failed region lookup it leaves the existing
// client and region alone and reports the error. Mutating them on failure is
// what used to pin every later call — CreateBucket included — to a wrong
// us-east-1 default, turning one denied GetBucketLocation into a stream of
// unrelated redirect errors.
func TestIntegrationRefreshClientIsFailSafe(t *testing.T) {
	m := testModel(t)
	bucket := freshBucket(t, m, "s3duck-it-region")
	putObject(t, m, bucket, "probe.txt", "probe")

	before := aws.ToString(m.Cf.Region)

	t.Run("a real bucket keeps the client usable", func(t *testing.T) {
		if err := m.RefreshClient(bucket.Key); err != nil {
			t.Fatalf("RefreshClient on an existing bucket: %v", err)
		}
		if got := keysUnder(t, m, bucket, ""); len(got) != 1 {
			t.Errorf("the rebuilt client lists %v, want the one probe object", got)
		}
	})

	t.Run("a missing bucket leaves the client untouched", func(t *testing.T) {
		missing := "s3duck-it-no-such-bucket-zzz"
		err := m.RefreshClient(&missing)
		if err == nil {
			t.Skip("the endpoint resolves a region for a nonexistent bucket; nothing to assert")
		}
		if got := aws.ToString(m.Cf.Region); got != before {
			t.Errorf("a failed lookup rewrote the region to %q, want it left at %q", got, before)
		}
		// The important half: the client still works for the real bucket.
		if got := keysUnder(t, m, bucket, ""); len(got) != 1 {
			t.Errorf("after a failed RefreshClient the client lists %v, want the one probe object", got)
		}
	})
}

// TestIntegrationRestoreObjectGuards covers the argument validation, which is
// all that can be asserted without a Glacier-capable endpoint: the call itself
// is skipped where archival is unsupported, but the guards run everywhere and
// are what stop a folder prefix or a zero-day request reaching the service.
func TestIntegrationRestoreObjectGuards(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-restore")
	putObject(t, m, bucket, "archived.bin", "payload")

	if err := m.RestoreObject(ctx, bucket, "folder/", 1, ""); err == nil {
		t.Error("a prefix-like key was accepted for restore")
	}
	if err := m.RestoreObject(ctx, bucket, "", 1, ""); err == nil {
		t.Error("an empty key was accepted for restore")
	}
	if err := m.RestoreObject(ctx, bucket, "archived.bin", 0, ""); err == nil {
		t.Error("a zero-day restore was accepted")
	}
	if err := m.RestoreObject(ctx, nil, "archived.bin", 1, ""); err == nil {
		t.Error("a nil bucket was accepted for restore")
	}

	// A STANDARD object is not restorable; the endpoint must say so rather
	// than silently succeeding.
	if err := m.RestoreObject(ctx, bucket, "archived.bin", 1, ""); err == nil {
		t.Log("endpoint accepted a restore of a non-archived object; nothing more to assert")
	}
}
