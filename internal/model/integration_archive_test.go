//go:build integration

package model_test

import (
	"archive/zip"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
)

// yesReporter is the smallest ops.Reporter: it drops progress and answers
// every question with yes.
type yesReporter struct{}

func (yesReporter) Progress(ops.Progress)       {}
func (yesReporter) Ask(ops.Question) ops.Answer { return ops.AnswerYes }
func (yesReporter) Log(string, ...any)          {}

// TestIntegrationArchiveFromALiveBucket runs the real pipeline: a folder
// resolved the way the browser resolves it, streamed through OpenObject into a
// zip, and read back. The fake-store tests pin the decisions; this pins that
// OpenObject's body, size and time are what an archive entry needs.
func TestIntegrationArchiveFromALiveBucket(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-archive")
	putObject(t, m, bucket, "data/logs/a.log", "alpha\n")
	putObject(t, m, bucket, "data/logs/deep/b.json", `{"b":1}`)
	putObject(t, m, bucket, "data/other.txt", "not in the folder")

	targets, total, err := m.ResolveDownloadObjects(ctx, "data/logs/", true, nil, bucket)
	if err != nil {
		t.Fatalf("ResolveDownloadObjects: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "logs.zip")
	res := (&ops.Archive{
		Bucket: bucket, SrcPrefix: "data/", Targets: targets, SelectedCount: 1,
		TotalBytes: total, Format: ops.ArchiveZip, Dest: dest,
		Opener: m, FS: ops.OSFS{},
	}).Run(ctx, yesReporter{})
	if res.Failed() != 0 || res.Done != len(targets) {
		t.Fatalf("result = %+v", res)
	}

	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(body)
		if f.Modified.IsZero() || f.Modified.Year() < 2020 {
			t.Errorf("%s: entry time %v, want the object's LastModified", f.Name, f.Modified)
		}
	}
	if got["logs/a.log"] != "alpha\n" || got["logs/deep/b.json"] != `{"b":1}` || len(got) != 2 {
		t.Errorf("archive = %v", got)
	}
}

// TestIntegrationOpenObjectReportsWhatItStreams covers the port on its own: the
// body is the object, and the stat matches it.
func TestIntegrationOpenObjectReportsWhatItStreams(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	bucket := freshBucket(t, m, "s3duck-it-openobject")
	body := strings.Repeat("0123456789", 1000)
	putObject(t, m, bucket, "k.txt", body)

	rc, st, err := m.OpenObject(ctx, bucket, "k.txt")
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(data) != body {
		t.Fatalf("read %d bytes, err %v", len(data), err)
	}
	if st.Size != int64(len(body)) || st.LastModified == nil {
		t.Errorf("stat = %+v", st)
	}
	if _, _, err := m.OpenObject(ctx, bucket, "missing"); err == nil || !model.IsNotFound(err) {
		t.Errorf("a missing key should fail as not found, got %v", err)
	}
}
