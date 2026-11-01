package controller

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/session"
)

func TestEncodeRowsCSVAndJSON(t *testing.T) {
	size, n := int64(42), 3
	when := time.Date(2026, 11, 1, 10, 0, 0, 0, time.FixedZone("x", 3600))
	rows := []exportRow{
		{Bucket: "b", Key: "dir/a,\"b\"\nc.txt", Type: "object", Size: &size, LastModified: &when, StorageClass: "STANDARD", ETag: "e1"},
		{Bucket: "b", Key: "=HYPERLINK(\"x\")", Type: "object"},
		{Bucket: "b", Key: "dir/", Type: "folder", Size: &size, Objects: &n},
	}
	data, err := encodeRows(rows, formatCSVExport)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatalf("not valid CSV: %v\n%s", err, data)
	}
	if len(recs) != 4 || recs[0][1] != "key" {
		t.Fatalf("records = %v", recs)
	}
	// A key with a comma, quotes and a newline stays one field of one record.
	if recs[1][1] != "dir/a,\"b\"\nc.txt" || recs[1][3] != "42" || recs[1][4] != "2026-11-01T09:00:00Z" {
		t.Errorf("row 1 = %q", recs[1])
	}
	// A key a spreadsheet would run as a formula is defused in CSV.
	if recs[2][1] != "'=HYPERLINK(\"x\")" {
		t.Errorf("formula key = %q", recs[2][1])
	}
	if recs[3][7] != "3" {
		t.Errorf("objects column = %q", recs[3][7])
	}

	js, err := encodeRows(rows, formatJSONExport)
	if err != nil {
		t.Fatal(err)
	}
	var back []exportRow
	if err := json.Unmarshal(js, &back); err != nil || len(back) != 3 {
		t.Fatalf("JSON: %v %s", err, js)
	}
	// JSON keeps the key exactly — no formula guard, no HTML escaping.
	if back[1].Key != "=HYPERLINK(\"x\")" || strings.Contains(string(js), `<`) {
		t.Errorf("JSON altered a key: %q", back[1].Key)
	}
	if empty, _ := encodeRows(nil, formatJSONExport); strings.TrimSpace(string(empty)) != "[]" {
		t.Errorf("empty JSON = %q", empty)
	}
	if _, err := encodeRows(rows, "xml"); err == nil {
		t.Error("an unknown format was accepted")
	}
}

func TestExportRowsFromEachView(t *testing.T) {
	size := int64(7)
	class, etag := "GLACIER", `"abc"`
	key, full := "a.txt", "dir/a.txt"
	file := &model.Object{Key: &key, FullPath: &full, Ot: model.File, Size: &size, StorageClass: &class, Etag: &etag}
	r := exportRowFor("b", file)
	if r.Type != "object" || r.Key != "dir/a.txt" || r.ETag != "abc" || r.StorageClass != "GLACIER" || *r.Size != 7 {
		t.Errorf("object row = %+v", r)
	}
	if r := exportRowFor("", model.BucketObject("logs")); r.Type != "bucket" || r.Bucket != "logs" || r.Key != "" {
		t.Errorf("bucket row = %+v", r)
	}
	rows := searchExportRows([]searchHit{{key: "x", size: 1}, {bucket: "other", key: "y", size: 2}}, "cur")
	if rows[0].Bucket != "cur" || rows[1].Bucket != "other" || *rows[1].Size != 2 {
		t.Errorf("search rows = %+v", rows)
	}
	dir := newUsageNode("logs", "logs/", true)
	dir.bytes, dir.objects = 100, 4
	u := usageExportRows("b", []*usageNode{dir})
	if u[0].Type != "folder" || *u[0].Objects != 4 || *u[0].Size != 100 {
		t.Errorf("usage rows = %+v", u[0])
	}
}

func TestListingFileNameAndWrite(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 8, 7, 0, time.UTC)
	if got := listingFileName("logs/2026/../x y", formatCSVExport, now); got != "logs-2026-..-x-y-20261101-090807.csv" {
		t.Errorf("name = %q", got)
	}
	if got := listingFileName("///", formatJSONExport, now); got != "listing-20261101-090807.json" {
		t.Errorf("name = %q", got)
	}
	dir := t.TempDir()
	path, err := writeExport(filepath.Join(dir, "sub"), "out.csv", []byte("a,b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a,b\n" {
		t.Errorf("wrote %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "sub"))
	if len(entries) != 1 {
		t.Errorf("temp files left: %v", entries)
	}
}

// dualController builds a controller with two panes on two profiles.
func dualController(t *testing.T) (*Controller, session.Conn, session.Conn) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := NewController(cli.Options{Notify: cli.NotifyOff})
	region := "us-east-1"
	mk := func(name string) session.Conn {
		m, err := model.NewModel(model.NewConfig("http://127.0.0.1:1", &region, "k", "s", "", true, 0))
		if err != nil {
			t.Fatal(err)
		}
		return session.Conn{Model: m, Profile: &cfg.Config{Name: name}}
	}
	prod, backup := mk("prod"), mk("backup")
	c.browsing = true
	c.panes[0].SetConn(prod)
	c.panes[1].SetConn(backup)
	c.model, c.activeConfig = prod.Model, prod.Profile
	c.dual = true
	return c, prod, backup
}

// Tab makes the other pane's account the one every action uses.
func TestSwapPaneSwitchesTheAccount(t *testing.T) {
	c, prod, backup := dualController(t)
	if !c.panesDiffer() {
		t.Fatal("two profiles do not read as different")
	}
	c.swapPane()
	if c.model != backup.Model || c.activeConfig != backup.Profile {
		t.Errorf("after Tab the active account is %v", c.activeConfig.Name)
	}
	if c.clientFor(c.other()) != prod.Model {
		t.Error("the inactive pane lost its client")
	}
	c.swapPane()
	if c.model != prod.Model || c.activeConfig.Name != "prod" {
		t.Error("Tab back did not restore the first account")
	}
	if c.profileOf(backup.Model) != backup.Profile {
		t.Error("profileOf does not find the other pane's profile")
	}
}

// Changing a pane's account drops what it showed: a bucket name means a
// different bucket on the new one.
func TestSetConnRelocatesThePane(t *testing.T) {
	_, prod, backup := dualController(t)
	p := session.New()
	p.SetConn(prod)
	p.Enter(model.BucketObject("logs"), "2026/", "")
	p.SetConn(prod) // the same account: nothing moves
	if p.Bucket() == nil {
		t.Fatal("re-setting the same account moved the pane")
	}
	p.SetConn(backup)
	if p.Bucket() != nil || p.Path() != "" {
		t.Errorf("after switching accounts the pane still shows %v/%s", p.Bucket(), p.Path())
	}
	a, b := session.New(), session.New()
	a.SetConn(prod)
	b.SetConn(prod)
	if !session.SameAccount(a, b) {
		t.Error("one client on both sides is not the same account")
	}
}

func TestPaneTitleNamesTheProfileOnlyWhenTheyDiffer(t *testing.T) {
	_, prod, _ := dualController(t)
	if got := paneProfileLabel(prod, true); !strings.Contains(got, "prod") {
		t.Errorf("label = %q", got)
	}
	if got := paneProfileLabel(prod, false); got != "" {
		t.Errorf("same account labelled %q", got)
	}
	tagged := session.Conn{Model: prod.Model, Profile: &cfg.Config{Name: "[red]x"}}
	if got := paneProfileLabel(tagged, true); !strings.Contains(got, "[red[]x") {
		t.Errorf("profile name not escaped: %q", got)
	}
}

func TestCrossPaneDestination(t *testing.T) {
	_, _, backup := dualController(t)
	p := session.New()
	p.SetConn(backup)
	if _, err := crossPaneDestination(p); err == nil || !strings.Contains(err.Error(), "open a bucket") {
		t.Errorf("bucket list as destination: %v", err)
	}
	p.Enter(model.BucketObject("vault"), "2026", "")
	d, err := crossPaneDestination(p)
	if err != nil || d.bucket != "vault" || d.prefix != "2026/" || d.model != backup.Model {
		t.Errorf("destination = %+v, %v", d, err)
	}
	backup.Profile.ReadOnly = true
	if _, err := crossPaneDestination(p); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("read-only destination: %v", err)
	}
	local := session.New()
	local.SetConn(backup)
	local.SetLocalDir("/tmp")
	if _, err := crossPaneDestination(local); err == nil {
		t.Error("a local pane accepted as an account destination")
	}
	if _, err := crossPaneDestination(session.New()); err == nil {
		t.Error("a pane with no profile accepted")
	}
}

// The clipboard is tied to the account it was filled in; pasting into a pane
// on another profile is refused, not run against a same-named bucket there.
func TestPasteAcrossAccountsIsRefused(t *testing.T) {
	c, prod, _ := dualController(t)
	c.panes[1].Enter(model.BucketObject("logs"), "", "")
	c.clip = clipboard{op: "cut", bucket: model.BucketObject("logs"), model: prod.Model,
		items: []copyMoveItem{{shortName: "a", srcKey: "a"}}}
	c.swapPane()
	if err := pasteRefusal(c.clip.model, c.model); err == nil {
		t.Error("a paste into another account's pane was allowed")
	}
	if err := pasteRefusal(prod.Model, prod.Model); err != nil {
		t.Errorf("a paste on the same account refused: %v", err)
	}
	if err := pasteRefusal(nil, prod.Model); err != nil {
		t.Errorf("a clipboard with no recorded client refused: %v", err)
	}
}

// Each pane's profile remembers its own location; with one profile on both
// sides, the active pane's wins.
func TestRememberLocationPerPaneProfile(t *testing.T) {
	c, prod, backup := dualController(t)
	c.panes[0].Enter(model.BucketObject("logs"), "2026/", "")
	c.panes[1].Enter(model.BucketObject("vault"), "", "")
	c.rememberLocation()
	if prod.Profile.LastBucket != "logs" || prod.Profile.LastPrefix != "2026/" {
		t.Errorf("prod remembers %s/%s", prod.Profile.LastBucket, prod.Profile.LastPrefix)
	}
	if backup.Profile.LastBucket != "vault" {
		t.Errorf("backup remembers %q", backup.Profile.LastBucket)
	}

	c.panes[1].SetConn(prod)
	c.panes[1].Enter(model.BucketObject("other"), "", "")
	c.rememberLocation()
	if prod.Profile.LastBucket != "logs" {
		t.Errorf("same profile on both sides: remembered %q, want the active pane's", prod.Profile.LastBucket)
	}
}

// A pane comparison across profiles lists the right side through the right
// side's client. Two fake endpoints that each hold one key tell which client
// answered.
func TestCompareReadsEachSideThroughItsOwnClient(t *testing.T) {
	serve := func(key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>b</Name><KeyCount>1</KeyCount><IsTruncated>false</IsTruncated><Contents><Key>d/` + key + `</Key><Size>1</Size></Contents></ListBucketResult>`))
		}))
	}
	left, right := serve("left.txt"), serve("right.txt")
	defer left.Close()
	defer right.Close()
	region := "us-east-1"
	lm, _ := model.NewModel(model.NewConfig(left.URL, &region, "k", "s", "", true, 0))
	rm, _ := model.NewModel(model.NewConfig(right.URL, &region, "k", "s", "", true, 0))

	c := &Controller{}
	spec := syncSpec{dir: syncRemote,
		srcBucket: model.BucketObject("b"), srcPrefix: "d/",
		dstBucket: model.BucketObject("b"), dstPrefix: "d/", dstModel: rm}
	src, dst, err := c.collectSides(context.Background(), lm, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(src) != 1 || src[0].Rel != "left.txt" || len(dst) != 1 || dst[0].Rel != "right.txt" {
		t.Errorf("src %+v, dst %+v; want each side from its own endpoint", src, dst)
	}
}
