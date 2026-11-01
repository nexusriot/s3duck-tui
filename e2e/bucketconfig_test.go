//go:build e2e

package e2e

import (
	"context"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// v on the bucket dashboard turns versioning on, and the server agrees.
func TestDashboardEnablesVersioning(t *testing.T) {
	s := newStore(t)
	s.put("k.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("k.txt")

	u.typeText(",")
	u.typeText("b")
	u.waitFor("Bucket config", "Versioning", "v versioning on/off")
	u.typeText("v")
	u.waitFor("Enable versioning on " + s.name)
	u.press("Enter") // OK
	u.waitUntil("dashboard reloaded with versioning on", func(sc string) bool {
		return regexp.MustCompile(`Versioning\s+Enabled`).MatchString(sc)
	})

	out, err := s.m.Client.GetBucketVersioning(context.Background(), &s3.GetBucketVersioningInput{Bucket: aws.String(s.name)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != s3t.BucketVersioningStatusEnabled {
		t.Errorf("server says versioning is %q", out.Status)
	}

	// And back: suspend.
	u.typeText("v")
	u.waitFor("Suspend versioning on " + s.name)
	u.press("Enter")
	u.waitUntil("dashboard reloaded with versioning suspended", func(sc string) bool {
		return regexp.MustCompile(`Versioning\s+Suspended`).MatchString(sc)
	})
	u.press("Escape")
}

// , b shows the new sections, and t writes the bucket's tags.
func TestBucketDashboardEditsTags(t *testing.T) {
	s := newStore(t)
	s.put("k.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("k.txt")

	u.typeText(",")
	u.typeText("b")
	u.waitFor("Bucket config", "Public access block", "Lifecycle", "Replication", "Website hosting", "Policy")
	u.typeText("t")
	u.waitFor("Tags (key=value per line)")
	u.typeText("env=e2e")
	u.press("Tab", "Enter") // Save
	u.waitFor("env=e2e", "1 tag(s)")

	out, err := s.m.Client.GetBucketTagging(context.Background(), &s3.GetBucketTaggingInput{Bucket: aws.String(s.name)})
	if err != nil {
		t.Fatalf("reading the tags back: %v", err)
	}
	if len(out.TagSet) != 1 || aws.ToString(out.TagSet[0].Key) != "env" || aws.ToString(out.TagSet[0].Value) != "e2e" {
		t.Errorf("bucket tags = %+v", out.TagSet)
	}
	u.press("Escape")
}
