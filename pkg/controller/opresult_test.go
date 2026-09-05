package controller

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOpResultText(t *testing.T) {
	r := newOpResult("Sync")
	r.okBytes(100)
	r.okBytes(200)
	r.skip()
	r.fail(retryItem{label: "a/b.txt"}, fmt.Errorf("permission denied"))

	got := r.text(false)
	for _, want := range []string{"Sync finished with errors.", "Done: 2", "Skipped: 1", "Failed: 1", "a/b.txt: permission denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("report %q is missing %q", got, want)
		}
	}
	if !strings.Contains(got, "300 B") {
		t.Errorf("report should state the bytes moved: %q", got)
	}

	// A clean run and a cancelled run each say so.
	clean := newOpResult("Delete")
	clean.ok()
	if !strings.Contains(clean.text(false), "Delete complete.") {
		t.Error("a clean run should report completion")
	}
	if !strings.Contains(clean.text(true), "Delete canceled.") {
		t.Error("a cancelled run should say it was cancelled")
	}
}

func TestOpResultTextCapsTheInlineList(t *testing.T) {
	r := newOpResult("Copy")
	for i := 0; i < reportRows+5; i++ {
		r.fail(retryItem{label: fmt.Sprintf("item-%02d", i)}, fmt.Errorf("boom"))
	}
	got := r.text(false)
	if !strings.Contains(got, "...and 5 more (use Export list)") {
		t.Errorf("the report should disclose what it did not list: %q", got)
	}
	if strings.Contains(got, fmt.Sprintf("item-%02d", reportRows)) {
		t.Error("the inline list exceeded its cap")
	}
	// The ledger itself keeps everything — that is what makes the retry and
	// the export complete rather than a sample.
	if r.failed() != reportRows+5 || len(r.retryItems()) != reportRows+5 {
		t.Errorf("ledger holds %d failures, want %d", r.failed(), reportRows+5)
	}
	if len(r.exportLines()) != reportRows+5 {
		t.Errorf("export holds %d lines, want %d", len(r.exportLines()), reportRows+5)
	}
}

func TestOpResultNote(t *testing.T) {
	r := &opResult{name: "Copy", okCount: 2, note: "Objects written: 57"}
	if !strings.Contains(r.text(false), "Objects written: 57") {
		t.Error("the note is what explains items-vs-objects; it must be shown")
	}
}

func TestFailureFileName(t *testing.T) {
	when := time.Date(2026, 9, 2, 18, 15, 0, 0, time.UTC)
	if got := failureFileName("Copy to", when); got != "s3duck-copy-to-failures-20260902-181500.txt" {
		t.Errorf("failureFileName = %q", got)
	}
}

func TestWriteFailureReport(t *testing.T) {
	dir := t.TempDir()
	r := newOpResult("Sync")
	r.fail(retryItem{label: "one"}, fmt.Errorf("first"))
	r.fail(retryItem{label: "two"}, fmt.Errorf("second"))

	path, err := r.writeFailureReport(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "one\tfirst") || !strings.Contains(string(body), "two\tsecond") {
		t.Errorf("export = %q, want every failure, tab-separated", body)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("export mode = %v, want 0600 (it names object keys)", perm)
	}

	// Nothing to export is refused rather than writing an empty file the
	// user would then go looking through.
	if _, err := newOpResult("Sync").writeFailureReport(dir, time.Now()); err == nil {
		t.Error("exporting an empty ledger should error")
	}
}

func TestRetryItemsCarryTheirTarget(t *testing.T) {
	// The point of the ledger: a retry re-runs the same units, so the
	// descriptor has to survive the round trip.
	type unit struct{ id int }
	r := newOpResult("Delete")
	r.fail(retryItem{label: "x", target: unit{7}}, fmt.Errorf("nope"))
	items := r.retryItems()
	if len(items) != 1 {
		t.Fatalf("got %d retry items", len(items))
	}
	u, ok := items[0].target.(unit)
	if !ok || u.id != 7 {
		t.Errorf("target did not survive: %#v", items[0].target)
	}
}

func TestTransferBadge(t *testing.T) {
	if got := transferBadge(nil); got != "" {
		t.Errorf("an idle app should carry no badge, got %q", got)
	}
	done := []jobView{{status: jobDone}, {status: jobFailed}, {status: jobCanceled}}
	if got := transferBadge(done); got != "" {
		t.Errorf("finished jobs are not active, got %q", got)
	}
	one := []jobView{{status: jobRunning, done: 50, total: 100}}
	got := transferBadge(one)
	if !strings.Contains(got, "1 transfer") || !strings.Contains(got, "50%") {
		t.Errorf("badge = %q", got)
	}
	if strings.Contains(got, "transfers") {
		t.Errorf("one job should be singular: %q", got)
	}
	two := []jobView{{status: jobRunning, done: 1, total: 4}, {status: jobQueued, done: 0, total: 4}}
	// 1 of 8 bytes: %.0f rounds half to even, so 12.5% prints as 12%.
	if got := transferBadge(two); !strings.Contains(got, "2 transfers") || !strings.Contains(got, "12%") {
		t.Errorf("badge = %q, want the combined progress", got)
	}
	// A job whose total is unknown must not divide by zero or claim a share.
	if got := transferBadge([]jobView{{status: jobRunning, done: 10}}); !strings.Contains(got, "1 transfer") || strings.Contains(got, "%") {
		t.Errorf("unknown total = %q", got)
	}
	// Over-reported progress is clamped rather than printing 130%.
	if got := transferBadge([]jobView{{status: jobRunning, done: 13, total: 10}}); !strings.Contains(got, "100%") {
		t.Errorf("clamped badge = %q", got)
	}
}

func TestNoticeText(t *testing.T) {
	got := noticeText(jobView{kind: "download", desc: "photos/", status: jobDone})
	if !strings.Contains(got, "✓") || !strings.Contains(got, "download") || !strings.Contains(got, "photos/") {
		t.Errorf("notice = %q", got)
	}
	if got := noticeText(jobView{kind: "upload", desc: "x", status: jobFailed, failed: 3}); !strings.Contains(got, "3 failed") {
		t.Errorf("a failure count should be shown: %q", got)
	}
	// A long description is truncated so the notice cannot push the version
	// string off the header row.
	long := strings.Repeat("a", 100)
	if got := noticeText(jobView{kind: "sync", desc: long, status: jobDone}); len(got) > 80 {
		t.Errorf("notice is %d chars, too long: %q", len(got), got)
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1 000", 127000: "127 000", 1234567: "1 234 567"}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPartialNote(t *testing.T) {
	if got := partialNote(false, 10); got != "" {
		t.Errorf("a complete listing needs no note, got %q", got)
	}
	// Capped: the note has to point at the way to find the rest.
	if got := partialNote(true, listCap); !strings.Contains(got, "Ctrl+F") {
		t.Errorf("capped note = %q", got)
	}
	// Cancelled: retrying is the way out.
	if got := partialNote(true, 12); !strings.Contains(got, "retry") {
		t.Errorf("cancelled note = %q", got)
	}
}
