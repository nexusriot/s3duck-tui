package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	cfg "github.com/nexusriot/s3duck-tui/internal/config"
	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
	"github.com/nexusriot/s3duck-tui/internal/session"
)

func fileObj(full string) *model.Object {
	short := full[strings.LastIndex(full, "/")+1:]
	return &model.Object{Key: &short, FullPath: &full, Ot: model.File}
}

func folderObj(full string) *model.Object {
	return &model.Object{Key: &full, FullPath: &full, Ot: model.Folder}
}

func TestPurgeTargetsAndPlan(t *testing.T) {
	bucket := model.BucketObject("b")
	ts := purgeTargetsFor([]*model.Object{fileObj("x"), folderObj("logs/"), bucket, nil})
	if len(ts) != 2 || ts[0] != (purgeTarget{key: "x"}) || ts[1] != (purgeTarget{key: "logs/", folder: true}) {
		t.Fatalf("targets = %+v", ts)
	}
	if purgeWhat(ts) != "2 item(s)" || purgeWhat(ts[:1]) != "x" {
		t.Errorf("what = %q / %q", purgeWhat(ts), purgeWhat(ts[:1]))
	}
	refs := []model.VersionRef{{Key: "x", VersionID: "1"}, {Key: "x.bak", VersionID: "2"}}
	// A file's listing is prefix-based; the purge takes the exact key only.
	if got := planPurgeRefs(ts[0], refs, model.PurgeAll); len(got) != 1 || got[0].Key != "x" {
		t.Errorf("planned %+v; x.bak must not be purged with x", got)
	}
	if got := planPurgeRefs(purgeTarget{key: "", folder: true}, refs, model.PurgeAll); len(got) != 2 {
		t.Errorf("a folder purge planned %+v", got)
	}
}

func TestDeletedRowsAndUndeleteTargets(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	d := model.DeletedObject{Key: "logs/[old]/a.txt", MarkerID: "m", DeletedAt: &at, Size: 2048, Versions: 3}
	row := deletedRow(d, "logs/", true)
	for _, want := range []string{"[old[]/a.txt", "deleted ", "restores 2.0 KiB, 3 version(s) kept", "●"} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q: %s", want, row)
		}
	}
	bare := deletedRow(model.DeletedObject{Key: "x"}, "", false)
	if !strings.Contains(bare, "nothing to restore") {
		t.Errorf("a marker over nothing: %s", bare)
	}

	all := []model.DeletedObject{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	if got := undeleteTargets(all, map[string]bool{"c": true, "a": true}, 1); len(got) != 2 || got[0].Key != "a" || got[1].Key != "c" {
		t.Errorf("marked targets = %+v", got)
	}
	if got := undeleteTargets(all, map[string]bool{}, 1); len(got) != 1 || got[0].Key != "b" {
		t.Errorf("highlighted target = %+v", got)
	}
	if !strings.Contains(undeleteConfirmText(all[:1]), "Removes the delete marker") {
		t.Error("undelete confirmation does not say what it does")
	}
	if title := deletedTitle("b", "logs/", 5000, true); !strings.Contains(title, "first 5000") {
		t.Errorf("a capped view does not say so: %s", title)
	}
}

func TestBulkMetaEditMergesWithoutClobbering(t *testing.T) {
	e := bulkMetaEdit{
		cacheControl: "max-age=60",
		setMeta:      map[string]string{"Owner": "ops"},
		dropMeta:     []string{"stale"},
		setTags:      map[string]string{"env": "prod"},
		dropTags:     []string{"tmp"},
	}
	meta := model.ObjectMeta{ContentType: "text/csv", UserMetadata: map[string]string{"stale": "1", "keep": "yes"}}
	next, changed := e.applyMeta(meta)
	if !changed || next.ContentType != "text/csv" || next.CacheControl != "max-age=60" {
		t.Errorf("meta = %+v (changed %v); blank Content-Type must keep each object's own", next, changed)
	}
	if next.UserMetadata["owner"] != "ops" || next.UserMetadata["keep"] != "yes" || next.UserMetadata["stale"] != "" {
		t.Errorf("user metadata = %v", next.UserMetadata)
	}
	if meta.UserMetadata["stale"] != "1" {
		t.Error("the edit modified the object's own map")
	}
	// Applying it again changes nothing: the second pass is a skip.
	if _, again := e.applyMeta(next); again {
		t.Error("an object already in the asked-for state reads as changed")
	}

	tags, changed := e.applyTags(map[string]string{"tmp": "x", "team": "a"})
	if !changed || tags["env"] != "prod" || tags["team"] != "a" || tags["tmp"] != "" {
		t.Errorf("tags = %v", tags)
	}
	if (bulkMetaEdit{}).empty() != true || e.empty() {
		t.Error("empty()")
	}
	if !(bulkMetaEdit{setTags: map[string]string{"a": "b"}}).touchesTags() || (bulkMetaEdit{setTags: map[string]string{"a": "b"}}).touchesMeta() {
		t.Error("a tags-only edit must not touch metadata (that is a full copy)")
	}
	if got := splitKeys(" a, b\nc  "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("splitKeys = %v", got)
	}
}

type fakeMetaPort struct {
	meta         model.ObjectMeta
	tags         map[string]string
	metaPuts     int
	tagPuts      int
	headErr      error
	putTagsValue map[string]string
}

func (f *fakeMetaPort) HeadObject(context.Context, *model.Object, string) (model.ObjectMeta, error) {
	return f.meta, f.headErr
}
func (f *fakeMetaPort) PutObjectMeta(_ context.Context, _ *model.Object, _ string, m model.ObjectMeta) error {
	f.metaPuts++
	f.meta = m
	return nil
}
func (f *fakeMetaPort) ObjectTags(context.Context, *model.Object, string) (map[string]string, error) {
	return f.tags, nil
}
func (f *fakeMetaPort) PutObjectTags(_ context.Context, _ *model.Object, _ string, t map[string]string) error {
	f.tagPuts++
	f.putTagsValue = t
	return nil
}

func TestApplyBulkMeta(t *testing.T) {
	b := model.BucketObject("b")
	// Tags only: no metadata rewrite (a full object copy) is made.
	p := &fakeMetaPort{tags: map[string]string{}}
	if err := applyBulkMeta(context.Background(), p, b, "k", bulkMetaEdit{setTags: map[string]string{"env": "prod"}}); err != nil {
		t.Fatal(err)
	}
	if p.metaPuts != 0 || p.tagPuts != 1 {
		t.Errorf("metaPuts=%d tagPuts=%d", p.metaPuts, p.tagPuts)
	}
	// Already so: skipped, nothing written.
	p2 := &fakeMetaPort{tags: map[string]string{"env": "prod"}}
	if err := applyBulkMeta(context.Background(), p2, b, "k", bulkMetaEdit{setTags: map[string]string{"env": "prod"}}); err != ops.ErrUnchanged {
		t.Errorf("err = %v, want ErrUnchanged", err)
	}
	// A failed read is a failure, not a skip.
	p3 := &fakeMetaPort{headErr: errors.New("NoSuchKey")}
	if err := applyBulkMeta(context.Background(), p3, b, "k", bulkMetaEdit{contentType: "text/plain"}); err == nil || err == ops.ErrUnchanged {
		t.Errorf("err = %v", err)
	}
	// Merged tags past S3's limit of 10 are refused before anything is sent.
	many := map[string]string{}
	for i := 0; i < 10; i++ {
		many[string(rune('a'+i))] = "x"
	}
	p4 := &fakeMetaPort{tags: many}
	if err := applyBulkMeta(context.Background(), p4, b, "k", bulkMetaEdit{setTags: map[string]string{"z": "y"}}); err == nil || p4.tagPuts != 0 {
		t.Errorf("11 tags: err=%v puts=%d", err, p4.tagPuts)
	}
	text := bulkMetaConfirm(bulkMetaEdit{contentType: "text/plain", dropTags: []string{"tmp"}}, 7)
	for _, want := range []string{"Change 7 object(s)", "Content-Type → text/plain", "tag tmp removed", "rewrites each object"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, text)
		}
	}
	if storageClassOf(model.ObjectMeta{}) != "STANDARD" {
		t.Error("an omitted class is STANDARD")
	}
}

func TestKnownBuckets(t *testing.T) {
	p := &cfg.Config{
		LastBucket:    "reports",
		Bookmarks:     []cfg.Bookmark{{Bucket: "logs"}, {Bucket: "reports"}, {Bucket: " "}},
		PublicBuckets: []string{"open"},
	}
	if got := strings.Join(knownBuckets(p), ","); got != "reports,logs,open" {
		t.Errorf("known = %s", got)
	}
	if knownBuckets(nil) != nil || len(knownBuckets(&cfg.Config{})) != 0 {
		t.Error("no profile, no names")
	}
	rows := bucketRows([]string{"a"})
	if len(rows) != 1 || rows[0].Ot != model.Bucket || *rows[0].Key != "a" {
		t.Errorf("rows = %+v", rows)
	}
}

// A bucket delete purges every version; its confirmation says so.
func TestBucketDeleteConfirmMentionsVersions(t *testing.T) {
	text := deleteConfirmText([]deleteTarget{{name: "b", key: "b", isBucket: true, objects: 3, bytes: 10}})
	if !strings.Contains(text, "Every old version and delete marker") {
		t.Errorf("bucket confirmation:\n%s", text)
	}
	if strings.Contains(deleteConfirmText([]deleteTarget{{name: "k", key: "k", objects: 1}}), "old version") {
		t.Error("an object delete talks about versions")
	}
}

// Credentials scoped to some buckets are refused the bucket list; opening a
// bucket by name must still work, and the bucket screen shows the buckets the
// profile knows of instead of an error.
func TestBucketByNameWhenListingIsDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>no</Message></Error>`))
	}))
	defer srv.Close()
	region := "us-east-1"
	mdl, err := model.NewModel(model.NewConfig(srv.URL, &region, "k", "s", "", true, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := NewController(cliOptsOff())
	c.model = mdl
	c.activeConfig = &cfg.Config{Name: "scoped", LastBucket: "reports", Bookmarks: []cfg.Bookmark{{Bucket: "logs"}}}

	if b := c.findBucketByName(context.Background(), c.pane(), "private-team"); b == nil || *b.Key != "private-team" {
		t.Errorf("findBucketByName = %v, want the named bucket taken at its word", b)
	}
	objs, partial, err := c.makeObjectMap(context.Background(), mdl, sessionRoot(), nil)
	if err != nil || !partial || len(objs) != 2 || objs["reports"] == nil || objs["logs"] == nil {
		t.Errorf("bucket screen = %v (partial %v, err %v); want the two known buckets", objs, partial, err)
	}
	c.activeConfig = &cfg.Config{Name: "bare"}
	if _, _, err := c.makeObjectMap(context.Background(), mdl, sessionRoot(), nil); err == nil || !strings.Contains(err.Error(), "s3://bucket/") {
		t.Errorf("no known buckets: err = %v; want the hint to open one by name", err)
	}
}

func cliOptsOff() cli.Options { return cli.Options{Notify: cli.NotifyOff} }

func sessionRoot() session.Location { return session.Location{} }
