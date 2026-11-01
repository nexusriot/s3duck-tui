package controller

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
)

func TestResolveLaunchProfile(t *testing.T) {
	a, b := &cfg.Config{Name: "a"}, &cfg.Config{Name: "b"}
	two := []*cfg.Config{a, b}

	if p, err := resolveLaunchProfile(two, "b", false); err != nil || p != b {
		t.Errorf("named = %v, %v; want b", p, err)
	}
	if p, err := resolveLaunchProfile(two, "", false); err != nil || p != nil {
		t.Errorf("nothing asked = %v, %v; want nil, nil (the profile list)", p, err)
	}
	if p, err := resolveLaunchProfile([]*cfg.Config{a}, "", true); err != nil || p != a {
		t.Errorf("location, one profile = %v, %v; want a", p, err)
	}

	_, err := resolveLaunchProfile(two, "", true)
	if err == nil || !strings.Contains(err.Error(), "--profile") || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("location, two profiles: err = %v; want a hint naming --profile and the profiles", err)
	}
	_, err = resolveLaunchProfile(two, "prod", false)
	if err == nil || !strings.Contains(err.Error(), `"prod"`) || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("unknown profile: err = %v; want it named, with the configured ones", err)
	}
	// Names are matched exactly: "a " is a different profile from "a".
	if _, err := resolveLaunchProfile(two, "a ", false); err == nil {
		t.Error(`"a " resolved to "a"`)
	}
	if _, err := resolveLaunchProfile(nil, "", true); err == nil {
		t.Error("location with no profiles accepted")
	}
}

func TestLocationTarget(t *testing.T) {
	tests := []struct {
		key        string
		isFolder   bool
		wantPrefix string
		wantSelect string
	}{
		{"", false, "", ""},
		{"logs/", false, "logs/", ""},
		{"logs", true, "logs/", ""},
		{"logs/app.log", false, "logs/", "logs/app.log"},
		{"top.txt", false, "", "top.txt"},
		// A trailing slash is a folder whatever the probe said.
		{"a/b/", false, "a/b/", ""},
		// Empty segments survive: "a//x" lives in "a//".
		{"a//x", false, "a//", "a//x"},
	}
	for _, tt := range tests {
		p, s := locationTarget(tt.key, tt.isFolder)
		if p != tt.wantPrefix || s != tt.wantSelect {
			t.Errorf("locationTarget(%q, %v) = (%q, %q), want (%q, %q)", tt.key, tt.isFolder, p, s, tt.wantPrefix, tt.wantSelect)
		}
	}
}

func TestResolveGoto(t *testing.T) {
	tests := []struct {
		name, input, bucket, path string
		wantBucket, wantKey       string
	}{
		{"uri", "s3://other/x/y/", "b", "p/", "other", "x/y/"},
		{"uri keeps odd keys", "s3://b/a//./x", "b", "", "b", "a//./x"},
		{"absolute in bucket", "/docs/", "b", "p/q/", "b", "docs/"},
		{"relative folder", "sub/", "b", "p/", "b", "p/sub/"},
		{"relative object", "file.txt", "b", "p/", "b", "p/file.txt"},
		{"dot dot", "../x/", "b", "p/q/", "b", "p/x/"},
		{"back to root", "..", "b", "p/", "b", ""},
		{"from bucket list", "logs/2026/", "", "", "logs", "2026/"},
		{"bucket only from list", "logs", "", "", "logs", ""},
		{"slash from list", "/logs/x", "", "", "logs", "x"},
		{"spaces trimmed", "  sub/  ", "b", "", "b", "sub/"},
		// The current folder is kept as it is: its own empty segments are
		// part of real keys, and cleaning the joined path rewrote them.
		{"current empty segment kept", "report.csv", "b", "a//b/", "b", "a//b/report.csv"},
		{"current leading slash kept", "x", "b", "/etc/", "b", "/etc/x"},
		{"dot dot over an empty segment", "..", "b", "a//", "b", "a/"},
		// A URI keeps a trailing space (part of the key) but not a pasted
		// line break.
		{"uri trailing space", "s3://b/dir/report \n", "x", "", "b", "dir/report "},
	}
	for _, tt := range tests {
		gb, gk, err := resolveGoto(tt.input, tt.bucket, tt.path)
		if err != nil {
			t.Errorf("%s: resolveGoto(%q) error %v", tt.name, tt.input, err)
			continue
		}
		if gb != tt.wantBucket || gk != tt.wantKey {
			t.Errorf("%s: resolveGoto(%q, %q, %q) = (%q, %q), want (%q, %q)",
				tt.name, tt.input, tt.bucket, tt.path, gb, gk, tt.wantBucket, tt.wantKey)
		}
	}
	for _, bad := range []struct{ in, bucket, path string }{
		{"", "b", ""},
		{"   ", "b", ""},
		{"../..", "b", "p/"},
		{"..", "", ""},
		{"s3://", "b", ""},
	} {
		if _, _, err := resolveGoto(bad.in, bad.bucket, bad.path); err == nil {
			t.Errorf("resolveGoto(%q, %q, %q) accepted", bad.in, bad.bucket, bad.path)
		}
	}
}

func TestObjectURIs(t *testing.T) {
	file := &model.Object{Key: strptr("a.txt"), FullPath: strptr("dir/a.txt"), Ot: model.File}
	folder := &model.Object{Key: strptr("sub"), FullPath: strptr("dir/sub/"), Ot: model.Folder}
	bucket := model.BucketObject("logs")

	if got := objectURIs("b", "dir/", nil); !reflect.DeepEqual(got, []string{"s3://b/dir/"}) {
		t.Errorf("no rows = %v, want the current folder", got)
	}
	if got := objectURIs("b", "", nil); !reflect.DeepEqual(got, []string{"s3://b/"}) {
		t.Errorf("bucket root = %v", got)
	}
	got := objectURIs("b", "dir/", []*model.Object{file, folder, nil})
	if want := []string{"s3://b/dir/a.txt", "s3://b/dir/sub/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	if got := objectURIs("", "", []*model.Object{bucket}); !reflect.DeepEqual(got, []string{"s3://logs/"}) {
		t.Errorf("bucket row = %v", got)
	}
	if got := copyURIsMessage([]string{"s3://b/x"}); got != "Copied s3://b/x" {
		t.Errorf("one = %q", got)
	}
	if got := copyURIsMessage([]string{"x", "y"}); !strings.Contains(got, "2 URIs") {
		t.Errorf("two = %q", got)
	}
}

func TestPresignUploadDefaultKey(t *testing.T) {
	file := &model.Object{Key: strptr("a.txt"), FullPath: strptr("in/a.txt"), Ot: model.File}
	folder := &model.Object{Key: strptr("sub"), FullPath: strptr("in/sub/"), Ot: model.Folder}
	if got := presignUploadDefaultKey("in/", file); got != "in/a.txt" {
		t.Errorf("on a file = %q", got)
	}
	if got := presignUploadDefaultKey("in/", folder); got != "in/" {
		t.Errorf("on a folder = %q, want the current folder", got)
	}
	if got := presignUploadDefaultKey("in/", nil); got != "in/" {
		t.Errorf("on .. = %q", got)
	}
}

func TestUploadLinkText(t *testing.T) {
	link := "https://s3.example/b/k?X-Amz-Signature=abc[1]"
	txt := uploadLinkText("s3://b/k", link, "1 hour", true, false, nil)
	for _, want := range []string{"copied", "s3://b/k", "1 hour", "replaces it", "curl -T", "X-Amz-Signature=abc"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text lacks %q:\n%s", want, txt)
		}
	}
	// The link is escaped for the colour-tag renderer: "[1]" would
	// otherwise be eaten as a tag and the displayed link would be wrong.
	if !strings.Contains(txt, "abc[1[]") {
		t.Errorf("link not escaped for display:\n%s", txt)
	}
	if strings.Contains(txt, "temporary credentials") {
		t.Error("a static-key link warned about temporary credentials")
	}
	if tmp := uploadLinkText("s3://b/k", link, "7 days", false, true, nil); !strings.Contains(tmp, "temporary credentials") {
		t.Error("a link signed with temporary credentials does not say it may expire early")
	}
	fresh := uploadLinkText("s3://b/k", link, "1 hour", false, false, errString("no display"))
	if strings.Contains(fresh, "replaces it") {
		t.Error("a new key is reported as replacing something")
	}
	if !strings.Contains(fresh, "clipboard is unavailable") || strings.Contains(fresh, "Upload link copied") {
		t.Errorf("a failed copy reads as a success:\n%s", fresh)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestValidateLaunch(t *testing.T) {
	two := []*cfg.Config{{Name: "a"}, {Name: "b"}}
	c := &Controller{params: &cfg.Params{Config: two}}
	if err := c.ValidateLaunch(); err != nil {
		t.Errorf("nothing asked: %v", err)
	}
	c.launch = cli.Options{Profile: "b"}
	if err := c.ValidateLaunch(); err != nil {
		t.Errorf("known profile: %v", err)
	}
	c.launch = cli.Options{Location: &cli.S3URI{Bucket: "x"}}
	if err := c.ValidateLaunch(); err == nil {
		t.Error("location with two profiles accepted")
	}
	// A config that failed to load is reported as such, not as "no profile
	// named …" against an empty list.
	c.params.LoadErr = errString("failed to parse config file")
	c.launch = cli.Options{Profile: "a"}
	if err := c.ValidateLaunch(); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("load error: err = %v", err)
	}
}

// The launch location is used once: Ctrl+P and reopening the profile must
// land on its remembered location again.
func TestPendingLocationIsTakenOnce(t *testing.T) {
	c := &Controller{pendingLocation: &cli.S3URI{Bucket: "b", Key: "k"}}
	if loc := c.takePendingLocation(); loc == nil || loc.Bucket != "b" {
		t.Fatalf("first take = %v", loc)
	}
	if loc := c.takePendingLocation(); loc != nil {
		t.Errorf("second take = %v, want nil", loc)
	}
}

func TestProfileNameListEmpty(t *testing.T) {
	if got := profileNameList(nil); got != "none" {
		t.Errorf("= %q", got)
	}
}

// A launch location belongs to the profile the command line named. When that
// profile fails to open, the location must not wait for the next profile the
// user picks — another account, where the same bucket name means something
// else.
func TestFailedLaunchProfileDropsTheLocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	broken := filepath.Join(t.TempDir(), "aws-config")
	if err := os.WriteFile(broken, []byte("[profile sso\nthis is not ini"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", broken)

	c := NewController(cli.Options{Notify: cli.NotifyOff})
	if _, err := model.NewModel(modelConfigFor(&cfg.Config{Name: "prod", AWSProfile: "sso"})); err == nil {
		t.Skip("this SDK build loads the malformed config; cannot make the profile fail to open")
	}
	c.pendingLocation = &cli.S3URI{Bucket: "logs", Key: "2026/"}
	c.activeConfig = &cfg.Config{Name: "prod", AWSProfile: "sso"}
	c.Duck()
	if c.activeConfig != nil {
		t.Error("a profile that failed to open stayed active")
	}
	if c.pendingLocation != nil {
		t.Errorf("the launch location %v is still pending for the next profile", c.pendingLocation)
	}
}
