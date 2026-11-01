//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	cfg "github.com/nexusriot/s3duck-tui/internal/config"
)

// The quick-wins round (0.12.0) through the real binary: the command line,
// go-to, copy URI, upload links, anonymous profiles, the versioning switch and
// notifications. Each ends in what the server, the disk or the screen holds.

// noClipboard keeps a run on a desktop host off the user's real clipboard:
// with no display the copy fails and the app shows the text instead, which
// is what the scenarios read anyway.
var noClipboard = []string{"DISPLAY=", "WAYLAND_DISPLAY="}

// A location on the command line opens there in place of the profile list —
// as a folder when the key has no slash but holds objects, and as its folder
// with the object highlighted when it is an object.
func TestLaunchArgumentOpensLocation(t *testing.T) {
	s := newStore(t)
	s.put("a/b/c.txt", "c")
	s.put("a/top.txt", "t")
	h := newHome(t)
	h.writeProfiles(h.profile("", ""))

	u := startTUIWith(t, h, nil, "-p", "e2e", "s3://"+s.name+"/a/b")
	u.waitFor(fmt.Sprintf("(%s)/a/b/", s.name), "c.txt")
	u.quit()

	u = startTUIWith(t, h, nil, "s3://"+s.name+"/a/top.txt") // one profile: no -p needed
	u.waitFor(fmt.Sprintf("(%s)/a/", s.name), "top.txt")
	u.waitUntil("a/top.txt highlighted", func(sc string) bool { return highlighted(sc) == "a/top.txt" })
}

// g goes to a URI, then to a relative path with "..", landing on an object;
// , c then names that object as a URI (copied, or shown when there is no
// clipboard — either way on screen).
func TestGoToThenCopyURI(t *testing.T) {
	s := newStore(t)
	s.put("a/b/c.txt", "c")
	s.put("a/top.txt", "t")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")

	u.typeText("g")
	u.waitFor("Go to", "s3://"+s.name+"/")
	u.press("C-u")
	u.typeText("s3://" + s.name + "/a/b/")
	u.press("Enter")
	u.waitFor(fmt.Sprintf("(%s)/a/b/", s.name), "c.txt")

	u.typeText("g")
	u.waitFor("s3://" + s.name + "/a/b/")
	u.press("C-u")
	u.typeText("../top.txt")
	u.press("Enter")
	u.waitFor(fmt.Sprintf("(%s)/a/", s.name))
	u.waitUntil("a/top.txt highlighted", func(sc string) bool { return highlighted(sc) == "a/top.txt" })

	u.typeText(",")
	u.typeText("c")
	u.waitFor("s3://" + s.name + "/a/top.txt")
}

// , p makes an upload link; a plain HTTP PUT through it — no credentials —
// lands the body at the key.
func TestPresignedUploadLinkAcceptsAPut(t *testing.T) {
	s := newStore(t)
	s.put("inbox/readme.txt", "r")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "inbox/"))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "inbox/")
	u.waitFor("readme.txt")

	u.typeText(",")
	u.typeText("p")
	u.waitFor("Presigned upload link", "Object key", "Expires in")
	u.press("C-u")
	u.typeText("inbox/delivered.txt")
	u.press("Tab", "Tab", "Enter") // expiry, then Create link
	screen := u.waitFor("Upload with:", "s3://"+s.name+"/inbox/delivered.txt")
	if strings.Contains(screen, "replaces it") {
		t.Error("a new key was reported as replacing an object")
	}

	link := linkFromScreen(t, screen)
	req, err := http.NewRequest(http.MethodPut, link, strings.NewReader("hello from curl"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT through the link: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT through the link: %s", resp.Status)
	}
	if got, ok := s.get("inbox/delivered.txt"); !ok || got != "hello from curl" {
		t.Errorf("inbox/delivered.txt = %q (exists %v)", got, ok)
	}

	// The link is a credential: the activity log notes it, without the URL.
	logData, _ := os.ReadFile(filepath.Join(h.stateDir(), "s3duck-tui", "activity.log"))
	if !strings.Contains(string(logData), "presigned upload link") || strings.Contains(string(logData), "X-Amz-Signature") {
		t.Errorf("activity log:\n%s", logData)
	}
}

// linkFromScreen reassembles the link the upload-link panel wraps across
// lines, below its curl example.
func linkFromScreen(t *testing.T, screen string) string {
	t.Helper()
	return linkAfter(t, screen, "curl -T FILE")
}

// linkAfter reads a presigned link wrapped across the lines below anchor in
// an overlay. The overlay's border columns are found on the anchor line (the
// list behind it has borders of its own, and a focused overlay draws ║ rather
// than │); the link is the text between them on the following lines, from the
// first non-blank one up to the next blank.
func linkAfter(t *testing.T, screen, anchor string) string {
	t.Helper()
	isBorder := func(r rune) bool { return r == '│' || r == '║' }
	lines := strings.Split(screen, "\n")
	start, left, right := -1, -1, -1
	for i, l := range lines {
		rs := []rune(l)
		at := strings.Index(l, anchor)
		if at < 0 {
			continue
		}
		col := len([]rune(l[:at]))
		for c := col; c >= 0; c-- {
			if isBorder(rs[c]) {
				left = c
				break
			}
		}
		for c := col; c < len(rs); c++ {
			if isBorder(rs[c]) {
				right = c
				break
			}
		}
		start = i + 1
	}
	if start < 0 || left < 0 || right < 0 {
		t.Fatalf("no %q inside an overlay on screen", anchor)
	}
	var b strings.Builder
	for _, l := range lines[start:] {
		rs := []rune(l)
		if len(rs) <= right {
			break
		}
		inner := strings.TrimSpace(string(rs[left+1 : right]))
		if inner == "" {
			if b.Len() > 0 {
				break
			}
			continue
		}
		b.WriteString(inner)
	}
	link := b.String()
	if !regexp.MustCompile(`^https?://\S+X-Amz-Signature=[0-9a-f]+$`).MatchString(link) {
		t.Fatalf("could not read the link off the screen: %q", link)
	}
	return link
}

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

// A transfer sent to the background posts a desktop notification when it
// finishes: here through a stand-in notify-send that records its arguments.
func TestBackgroundDownloadNotifies(t *testing.T) {
	s := newStore(t)
	// Big enough, with the throttle below, to still be running when
	// Background is pressed.
	s.put("big.bin", strings.Repeat("x", 600<<10))
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 200 << 10
	h.writeProfiles(p)

	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "notified")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(filepath.Join(bin, "notify-send"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	u := startTUIWith(t, h, []string{"PATH=" + bin + ":" + os.Getenv("PATH")}, "--notify", "command")
	u.openProfile(s.name, "")
	u.waitFor("big.bin")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter") // OK
	u.waitFor("Background")
	u.press("Enter") // Background

	deadline := time.Now().Add(waitTimeout)
	for {
		data, err := os.ReadFile(record)
		// The job is described as the queue lists it: kind, what and where
		// ("download 1 obj → …/Downloads/"), then how it ended.
		if err == nil && len(data) > 0 {
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(got) != 3 || got[0] != "--app-name=s3duck-tui" || got[1] != "s3duck-tui" ||
				!strings.HasPrefix(got[2], "download ") || !strings.HasSuffix(got[2], ": done") ||
				!strings.Contains(got[2], h.downloads) {
				t.Errorf("notify-send argv = %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no notification posted (record: %q, %v)", data, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got, ok := readFile(t, filepath.Join(h.downloads, "big.bin")); !ok || len(got) != 600<<10 {
		t.Errorf("the download itself: %d bytes, exists %v", len(got), ok)
	}
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

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
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

// , p on an existing object offers that object's key and says the link would
// replace it; a PUT through the link does.
func TestUploadLinkWarnsBeforeReplacing(t *testing.T) {
	s := newStore(t)
	s.put("inbox/report.csv", "old")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "inbox/"))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "inbox/")
	u.waitFor("report.csv")
	u.highlight("inbox/report.csv")

	u.typeText(",")
	u.typeText("p")
	u.waitFor("Object key", "inbox/report.csv")
	u.press("Tab", "Tab", "Enter") // expiry, then Create link
	screen := u.waitFor("Upload with:", "replaces it")

	req, err := http.NewRequest(http.MethodPut, linkFromScreen(t, screen), strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT through the link: %v %v", resp, err)
	}
	resp.Body.Close()
	if got, _ := s.get("inbox/report.csv"); got != "new" {
		t.Errorf("inbox/report.csv = %q, want the uploaded body", got)
	}
}

// Ctrl+W on a keyed profile signs a GET link (shown when there is no
// clipboard), which an unauthenticated client can follow.
func TestSignedShareLinkDownloads(t *testing.T) {
	s := newStore(t)
	s.put("doc.txt", "shared body")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("doc.txt")
	u.highlight("doc.txt")
	u.press("C-w")
	u.waitFor("Presigned link expiry for", "15 min", "7 days")
	u.press("Enter") // 15 min
	screen := u.waitFor("Clipboard unavailable", "X-Amz-")

	link := linkAfter(t, screen, "Clipboard unavailable")
	if !strings.Contains(link, "X-Amz-Expires=900") {
		t.Errorf("link = %q, want a 15-minute expiry", link)
	}
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "shared body" {
		t.Errorf("GET through the link: %s %q", resp.Status, body)
	}
}

// --notify osc777 writes the notification to the terminal itself; inside
// tmux it is wrapped for passthrough. pipe-pane sees the raw bytes the app
// wrote, before tmux interprets them.
func TestBackgroundDownloadNotifiesThroughTerminal(t *testing.T) {
	s := newStore(t)
	s.put("big.bin", strings.Repeat("x", 600<<10))
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 200 << 10
	h.writeProfiles(p)

	raw := filepath.Join(t.TempDir(), "pane.raw")
	u := startTUIWith(t, h, noClipboard, "--notify", "osc777")
	u.tmux("pipe-pane", "-O", "-t", "e2e", "cat >> "+raw)
	u.openProfile(s.name, "")
	u.waitFor("big.bin")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter") // OK
	u.waitFor("Background")
	u.press("Enter") // Background

	want := "\x1bPtmux;\x1b\x1b]777;notify;s3duck-tui;download "
	deadline := time.Now().Add(waitTimeout)
	for {
		data, _ := os.ReadFile(raw)
		if i := strings.Index(string(data), want); i >= 0 {
			rest := string(data[i+len(want):])
			end := strings.Index(rest, "\x07\x1b\\")
			if end < 0 || !strings.HasSuffix(rest[:end], ": done") {
				t.Errorf("sequence not terminated as a passthrough, or not done: %q", rest[:min(len(rest), 160)])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no OSC 777 notification in the pane output (%d bytes captured)", len(data))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Ctrl+Q under running transfers asks first; yes cancels them and lets each
// clean up before the process exits. An unfinished upload is aborted on the
// server. An unfinished download keeps what it has as a resumable partial —
// no file at the target name — and the next run, after a restart, resumes it
// to the object byte for byte with nothing left over. (Quitting used to stop
// the process under both, which did neither.)
func TestQuitCancelsTransfersAndADownloadResumesAfterRestart(t *testing.T) {
	s := newStore(t)
	body := strings.Repeat("d", 64<<20) // past the 5×5 MiB fetched at once
	s.put("big.bin", body)
	h := newHome(t)
	h.file("Downloads/up.bin", strings.Repeat("u", 12<<20))
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 2 << 20 // keeps both transfers running for several seconds
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.waitFor("big.bin")

	// A download, sent to the background.
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Background")
	u.press("Enter")
	u.waitUntil("the download's partial file to exist", func(string) bool {
		m, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
		return len(m) > 0
	})

	// A multipart upload from the local pane, also sent to the background.
	u.press("l")
	u.waitFor("[local]", "up.bin")
	u.press("/")
	u.typeText("up.bin")
	u.press("Enter")
	u.waitUntil("the filter to leave only up.bin", func(scr string) bool {
		return strings.Contains(scr, "up.bin") && !strings.Contains(scr, ".s3duck-part")
	})
	u.press("C-y")
	u.waitFor("Background")
	u.press("Enter")
	u.waitUntil("the upload to have started its multipart upload", func(string) bool {
		ups, err := s.m.ListMultipartUploads(s.bucket)
		return err == nil && len(ups) > 0
	})

	u.press("C-q")
	u.waitFor("2 transfer(s) still running", "Quit anyway?")
	u.press("Enter") // OK
	u.waitForExit()

	if _, err := os.Stat(filepath.Join(h.downloads, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("the interrupted download is at the target name")
	}
	partials, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-part"))
	sidecars, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*.s3duck-resume"))
	if len(partials) != 1 || len(sidecars) != 1 {
		t.Fatalf("after quit: partials %v, sidecars %v; want one resumable partial", partials, sidecars)
	}
	if ups, err := s.m.ListMultipartUploads(s.bucket); err != nil || len(ups) != 0 {
		t.Errorf("multipart uploads left open after quit: %v %v", ups, err)
	}
	logData, _ := os.ReadFile(filepath.Join(h.stateDir(), "s3duck-tui", "activity.log"))
	if !strings.Contains(string(logData), "quit: cancelled 2 transfer(s)") || strings.Contains(string(logData), "did not finish") {
		t.Errorf("activity log does not record the cancel:\n%s", logData)
	}

	// A new run of the app picks the download up where it stopped.
	p.MaxBytesPerSec = 0
	h.writeProfiles(p)
	u = startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Download complete.")
	u.press("Enter")
	if got, _ := readFile(t, filepath.Join(h.downloads, "big.bin")); got != body {
		t.Errorf("the resumed download is %d bytes and differs from the object", len(got))
	}
	if left, _ := filepath.Glob(filepath.Join(h.downloads, ".big.bin.*")); len(left) != 0 {
		t.Errorf("left after the resume: %v", left)
	}
}

// Answering Cancel to the quit question keeps the app — and its transfers —
// running, and a later Ctrl+Q asks again.
func TestQuitQuestionCanBeDeclined(t *testing.T) {
	s := newStore(t)
	s.put("big.bin", strings.Repeat("d", 4<<20))
	h := newHome(t)
	p := h.profile(s.name, "")
	p.MaxBytesPerSec = 256 << 10
	h.writeProfiles(p)

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "")
	u.highlight("big.bin")
	u.press("C-d")
	u.waitFor("Total size")
	u.press("Enter")
	u.waitFor("Background")
	u.press("Enter")

	u.press("C-q")
	u.waitFor("1 transfer(s) still running")
	u.press("Tab", "Enter") // Cancel
	u.waitGone("still running")
	u.press("C-q")
	u.waitFor("1 transfer(s) still running")
	u.press("Tab", "Enter")
	u.waitGone("still running")
}
