//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
		// The anchor's own text may wrap onto the lines below it (a long
		// clipboard error does); the link starts at the first http line.
		if b.Len() == 0 && !strings.HasPrefix(inner, "http") {
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
