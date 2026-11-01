//go:build e2e

package e2e

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// The versioned-bucket round through the real keys. Each scenario ends in
// what the server holds — versions, markers, tags — not in what the screen
// says.

func (s *store) versioned() {
	s.t.Helper()
	if _, err := s.m.Client.PutBucketVersioning(context.Background(), &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(s.name),
		VersioningConfiguration: &s3t.VersioningConfiguration{Status: s3t.BucketVersioningStatusEnabled},
	}); err != nil {
		s.t.Fatalf("enable versioning: %v", err)
	}
	// Versions survive the store's plain cleanup; purge them so the bucket
	// can be removed. Registered after newStore's, so it runs first.
	s.t.Cleanup(func() { _ = s.m.EmptyBucket(context.Background(), s.bucket) })
}

func (s *store) del(key string) {
	s.t.Helper()
	if err := s.m.Delete(context.Background(), &key, s.bucket); err != nil {
		s.t.Fatalf("delete %s: %v", key, err)
	}
}

// versions lists "key@kind" for every version under prefix, sorted.
func (s *store) versions(prefix string) []string {
	s.t.Helper()
	var out []string
	err := s.m.ListVersionsUnder(context.Background(), s.bucket, prefix, func(p []model.VersionRef) bool {
		for _, r := range p {
			kind := "v"
			if r.IsDeleteMark {
				kind = "marker"
			}
			out = append(out, r.Key+"@"+kind)
		}
		return true
	})
	if err != nil {
		s.t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// , D lists what was deleted under the folder; u undeletes the marked one,
// and the object lists — with its last content — again.
func TestDeletedViewUndeletes(t *testing.T) {
	s := newStore(t)
	s.versioned()
	s.put("docs/a.txt", "first")
	s.put("docs/a.txt", "second")
	s.put("docs/b.txt", "bee")
	s.del("docs/b.txt")
	s.del("docs/a.txt") // most recently deleted: the view's first row
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "docs/"))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "docs/")
	u.typeText(",")
	u.typeText("D")
	u.waitFor("Deleted under", "a.txt", "b.txt", "restores 6 B, 2 version(s) kept")
	u.press("Space") // marks the first row, a.txt
	u.typeText("u")
	u.waitFor("Undelete docs/a.txt?", "Removes the delete marker")
	u.press("Enter")
	u.waitFor("Undeleted 1 object(s)")

	if got, ok := s.get("docs/a.txt"); !ok || got != "second" {
		t.Errorf("docs/a.txt = %q (exists %v), want the newest version back", got, ok)
	}
	if _, ok := s.get("docs/b.txt"); ok {
		t.Error("docs/b.txt was not marked but came back")
	}
}

// , X on a folder, "Every version": the runner's confirmation states the
// counts, and afterwards nothing under the folder remains — no version, no
// marker. A sibling folder sharing the name stem is untouched.
func TestPurgeFolderEveryVersion(t *testing.T) {
	s := newStore(t)
	s.versioned()
	s.put("logs/a.log", "1")
	s.put("logs/a.log", "22")
	s.put("logs/b.log", "333")
	s.del("logs/b.log")
	s.put("logs2/keep.log", "k")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("logs/", "logs2/")
	u.highlight("logs/")
	u.typeText(",")
	u.typeText("X")
	u.waitFor("Purge logs/", "Every version", "Old versions only")
	u.press("Enter") // Every version
	u.waitFor("EVERY version of logs/", "2 key(s): 3 version(s), 6 B, and 1 delete marker(s)", "permanent")
	u.press("Enter") // OK
	u.waitFor("Done")
	u.press("Enter")

	if got := s.versions("logs/"); len(got) != 0 {
		t.Errorf("left under logs/: %v", got)
	}
	if got := s.versions("logs2/"); len(got) != 1 {
		t.Errorf("logs2/ = %v, want its one version untouched", got)
	}
}

// v with two items marked is the bulk version action: "Old versions only"
// keeps what each key is now and removes its history.
func TestVersionsKeyWithMarksPurgesOldVersions(t *testing.T) {
	s := newStore(t)
	s.versioned()
	s.put("a.txt", "1")
	s.put("a.txt", "2")
	s.put("b.txt", "1")
	s.put("b.txt", "22")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("a.txt", "b.txt")
	u.press("C-s") // mark everything visible
	u.waitFor("Selected: 2")
	u.typeText("v")
	u.waitFor("Purge 2 item(s)", "Old versions only")
	u.press("Tab", "Enter") // Old versions only
	u.waitFor("OLD versions of 2 item(s)", "2 key(s): 2 version(s)")
	u.press("Enter")
	u.waitFor("Done")
	u.press("Enter")

	if got := strings.Join(s.versions(""), ","); got != "a.txt@v,b.txt@v" {
		t.Errorf("versions now %s, want one per key", got)
	}
	if got, _ := s.get("b.txt"); got != "22" {
		t.Errorf("b.txt = %q, want its current content kept", got)
	}
}

// m with two marked: one bulk form, tags merged into each object's own.
func TestBulkTagsOnMarkedObjects(t *testing.T) {
	s := newStore(t)
	s.put("a.txt", "a")
	s.put("b.txt", "b")
	if _, err := s.m.Client.PutObjectTagging(context.Background(), &s3.PutObjectTaggingInput{
		Bucket: aws.String(s.name), Key: aws.String("b.txt"),
		Tagging: &s3t.Tagging{TagSet: []s3t.Tag{{Key: aws.String("team"), Value: aws.String("blue")}}},
	}); err != nil {
		t.Fatal(err)
	}
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("a.txt", "b.txt")
	u.press("C-s")
	u.waitFor("Selected: 2")
	u.typeText("m")
	u.waitFor("Metadata & tags — 2 objects", "Set tags (key=value per line)")
	u.press("Tab", "Tab", "Tab", "Tab") // to Set tags
	u.typeText("env=prod")
	u.press("Tab", "Tab", "Enter") // past Remove tag keys, onto Apply
	u.waitFor("Change 2 object(s)", "tag env = prod")
	u.press("Enter")
	u.waitFor("Done")
	u.press("Enter")

	for key, want := range map[string]string{"a.txt": "env=prod", "b.txt": "env=prod,team=blue"} {
		out, err := s.m.Client.GetObjectTagging(context.Background(), &s3.GetObjectTaggingInput{Bucket: aws.String(s.name), Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tg := range out.TagSet {
			got = append(got, aws.ToString(tg.Key)+"="+aws.ToString(tg.Value))
		}
		sort.Strings(got)
		if strings.Join(got, ",") != want {
			t.Errorf("%s tags = %v, want %s", key, got, want)
		}
	}
}

// Deleting a versioned bucket that has history and delete markers works from
// the bucket list: the confirmation says the history goes too, and the bucket
// is gone afterwards. It used to fail with BucketNotEmpty every time.
func TestDeleteVersionedBucketFromTheList(t *testing.T) {
	s := newStore(t)
	s.versioned()
	s.put("x.txt", "1")
	s.put("x.txt", "2")
	s.del("x.txt")
	h := newHome(t)
	h.writeProfiles(h.profile("", ""))

	u := startTUIWith(t, h, noClipboard)
	u.waitFor("(profiles)", "e2e")
	u.press("Enter")
	u.waitFor("(buckets)")
	u.press("/")
	u.typeText(s.name)
	u.press("Enter")
	u.waitUntil("the filter to leave only the test bucket", func(scr string) bool {
		return strings.Count(scr, "●  e2e-") == 1 && strings.Contains(scr, "●  "+s.name)
	})
	u.press("DC")
	u.waitFor("Every old version and delete marker in it is deleted too")
	u.press("Enter")
	u.waitUntil("the bucket to be gone", func(string) bool {
		_, err := s.m.Client.HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String(s.name)})
		return err != nil
	})
}
