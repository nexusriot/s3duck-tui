//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
)

// An anonymous profile lists the public buckets it names, browses one whose
// policy allows anonymous reads, and is refused by a private one.
func TestAnonymousProfileBrowsesPublicBucket(t *testing.T) {
	pub := newStore(t)
	pub.put("data/one.csv", "a,b")
	makePublic(t, pub)
	priv := newStore(t)
	priv.put("secret.txt", "s")

	h := newHome(t)
	r := region()
	h.writeProfiles(&cfg.Config{
		Name: "e2e", BaseUrl: endpoint(), Region: &r, DownloadDir: h.downloads,
		Anonymous: true, PublicBuckets: []string{pub.name, priv.name},
	})

	u := startTUIWith(t, h, noClipboard)
	u.waitFor("(profiles)", "e2e")
	u.press("Enter")
	u.waitFor("[ANONYMOUS]", pub.name, priv.name)

	u.typeText("g")
	u.press("C-u")
	u.typeText("s3://" + pub.name + "/data/")
	u.press("Enter")
	u.waitFor(fmt.Sprintf("(%s)/data/", pub.name), "one.csv")

	// Ctrl+W on an anonymous profile is the object's plain URL — which an
	// unauthenticated GET can fetch.
	u.highlight("data/one.csv")
	u.press("C-w")
	// The URL is shown (copied, or not for want of a clipboard) and may wrap.
	screen := u.waitFor("data/one.csv", "/"+pub.name+"/")
	if strings.Contains(screen, "X-Amz-") {
		t.Errorf("anonymous link carries signing parameters:\n%s", screen)
	}
	resp, err := http.Get(strings.TrimRight(endpoint(), "/") + "/" + pub.name + "/data/one.csv")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("public GET: %v %v", resp, err)
	}
	resp.Body.Close()

	// A private bucket refuses the unsigned listing.
	u.press("Enter") // dismiss the copy report
	u.typeText("g")
	u.press("C-u")
	u.typeText("s3://" + priv.name + "/")
	u.press("Enter")
	u.waitFor("Access Denied")
}

// makePublic lets anonymous callers list and read the store's bucket.
func makePublic(t *testing.T, s *store) {
	t.Helper()
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::%[1]s"]},
		{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%[1]s/*"]}]}`, s.name)
	if _, err := s.m.Client.PutBucketPolicy(context.Background(), &s3.PutBucketPolicyInput{
		Bucket: aws.String(s.name), Policy: aws.String(policy)}); err != nil {
		t.Fatalf("making the bucket public: %v", err)
	}
}

// The options form turns a keyed profile anonymous (o, tick the box, name the
// buckets, Save): the file records it, Ctrl+V checks every named bucket
// unsigned — and names the one that refuses — and an upload link is refused
// once the profile is open.
func TestAnonymousViaOptionsFormAndVerify(t *testing.T) {
	pub := newStore(t)
	pub.put("k.txt", "x")
	makePublic(t, pub)
	priv := newStore(t)
	h := newHome(t)
	h.writeProfiles(h.profile("", ""))

	u := startTUIWith(t, h, noClipboard)
	u.waitFor("(profiles)", "e2e")
	u.typeText("o")
	u.waitFor("Options: e2e", "Anonymous (no credentials")
	u.press("Tab")  // the Anonymous checkbox
	u.typeText(" ") // tick it
	u.press("Tab")  // the public buckets field
	u.typeText(pub.name + ", " + priv.name)
	for i := 0; i < 10; i++ { // past the remaining nine fields, onto Save
		u.press("Tab")
	}
	u.press("Enter")
	u.waitFor("Options saved")
	u.press("Enter")

	loaded, err := cfg.LoadConfiguration(h.configPath())
	if err != nil || len(loaded) != 1 {
		t.Fatalf("config after save: %v %v", loaded, err)
	}
	if p := loaded[0]; !p.Anonymous || strings.Join(p.PublicBuckets, ",") != pub.name+","+priv.name {
		t.Errorf("saved anonymous=%v public_buckets=%v", p.Anonymous, p.PublicBuckets)
	}
	u.waitFor("anonymous (unsigned requests")

	// Ctrl+V: the private bucket refuses the unsigned probe, by name.
	u.press("C-v")
	u.waitFor("error checking profile e2e: public bucket", "AccessDenied")
	u.press("Enter")

	// With only the public one named, the check passes.
	if err := os.WriteFile(h.configPath(), []byte(strings.Replace(string(mustRead(t, h.configPath())), ","+fmt.Sprintf("%q", priv.name), "", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	u.quit()
	u = startTUIWith(t, h, noClipboard)
	u.waitFor("(profiles)", "e2e")
	u.press("C-v")
	u.waitFor("successfully checked profile e2e: 1 public bucket(s) readable")
	u.press("Enter")

	u.press("Enter") // open it
	u.waitFor("[ANONYMOUS]", pub.name)
	u.press("Enter") // into the public bucket
	u.waitFor(fmt.Sprintf("(%s)/", pub.name), "k.txt")
	u.typeText(",")
	u.typeText("p")
	u.waitFor("an anonymous profile has no credentials to sign")
}

// A read-only profile refuses both new writes this round added: an upload
// link (a write handed to someone else) and the versioning switch. The
// server is left as it was.
func TestReadOnlyRefusesUploadLinkAndVersioning(t *testing.T) {
	s := newStore(t)
	s.put("k.txt", "x")
	h := newHome(t)
	p := h.profile(s.name, "")
	p.ReadOnly = true
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("k.txt")

	u.typeText(",")
	u.typeText("p")
	u.waitFor("is marked read-only, so it cannot hand out")
	u.press("Enter")

	u.typeText(",")
	u.typeText("b")
	u.waitFor("Bucket config", "v versioning on/off")
	u.typeText("v")
	u.waitFor("is marked read-only, so it cannot change")

	out, err := s.m.Client.GetBucketVersioning(context.Background(), &s3.GetBucketVersioningInput{Bucket: aws.String(s.name)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "" {
		t.Errorf("a read-only profile changed versioning to %q", out.Status)
	}
}

// x on the profiles screen: the export cycles formats and writes the shown one
// to the download directory, with the secret left out.
func TestExportProfileWritesAFile(t *testing.T) {
	h := newHome(t)
	h.writeProfiles(h.profile("", ""))

	u := startTUI(t, h)
	u.waitFor("(profiles)", "e2e")
	u.typeText("x")
	u.waitFor("Export e2e", "AWS CLI", "endpoint_url = "+endpoint())
	u.typeText("f")
	u.waitFor("mc alias set e2e")
	u.typeText("w")
	want := filepath.Join(h.downloads, "s3duck-e2e.mc.txt")
	// The notice wraps a long path, so its two halves are matched apart.
	u.waitFor("Written to "+filepath.Dir(want), filepath.Base(want))

	got, ok := readFile(t, want)
	if !ok || !strings.Contains(got, "mc alias set e2e "+endpoint()) || !strings.Contains(got, "not exported") {
		t.Errorf("export file:\n%s", got)
	}
}

// A profile name is user-typed and the profiles screen renders colour tags,
// so a name that looks like one must still be shown as itself — in the list
// and in the details panel beside it.
func TestProfileNameThatLooksLikeMarkup(t *testing.T) {
	h := newHome(t)
	p := h.profile("", "")
	p.Name = "[red]prod"
	h.writeProfiles(p)

	u := startTUI(t, h)
	u.waitFor("(profiles)")
	screen := u.waitFor("[red]prod")
	if strings.Count(screen, "[red]prod") < 2 {
		t.Errorf("the name should appear in both the list and the details panel:\n%s", screen)
	}
	// The details panel must still describe that profile, not a mangled name.
	// The details panel pads after the label, so the two are matched apart.
	u.waitFor("Config: [red]prod", endpoint())
}
