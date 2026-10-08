package flows

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/ops"
)

func TestThroughput(t *testing.T) {
	tests := []struct {
		name     string
		done     int64
		total    int64
		elapsed  time.Duration
		wantRate float64
		wantETA  time.Duration
	}{
		{"zero elapsed -> unknown", 10, 100, 0, 0, -1},
		{"zero done -> unknown", 0, 100, time.Second, 0, -1},
		{"halfway", 50, 100, time.Second, 50, time.Second},
		{"unknown total -> rate only", 100, 0, time.Second, 100, -1},
		{"already complete -> no eta", 100, 100, time.Second, 100, -1},
		{"slow eta", 25, 100, 5 * time.Second, 5, 15 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rate, eta := Throughput(tt.done, tt.total, tt.elapsed)
			if rate != tt.wantRate {
				t.Errorf("rate = %v, want %v", rate, tt.wantRate)
			}
			if eta != tt.wantETA {
				t.Errorf("eta = %v, want %v", eta, tt.wantETA)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		-1 * time.Second:                      "--",
		0:                                     "0:00",
		5 * time.Second:                       "0:05",
		65 * time.Second:                      "1:05",
		time.Hour:                             "1:00:00",
		time.Hour + time.Minute + time.Second: "1:01:01",
	}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestByteRateETA(t *testing.T) {
	// no throughput yet -> both unknown
	if got := ByteRateETA(0, 100, time.Second); got != "-- · ETA --" {
		t.Errorf("ByteRateETA(idle) = %q, want %q", got, "-- · ETA --")
	}
	// 1 MiB of 2 MiB in 1s -> 1.0 MiB/s, ETA 0:01
	got := ByteRateETA(1<<20, 2<<20, time.Second)
	if !strings.Contains(got, "MiB/s") || !strings.Contains(got, "ETA 0:01") {
		t.Errorf("byteRateETA = %q, want a MiB/s rate and ETA 0:01", got)
	}
}

func TestObjRateETA(t *testing.T) {
	if got := ObjRateETA(0, 10, time.Second); got != "-- · ETA --" {
		t.Errorf("ObjRateETA(idle) = %q, want %q", got, "-- · ETA --")
	}
	// 5 of 10 objects in 1s -> 5 obj/s, ETA 0:01
	got := ObjRateETA(5, 10, time.Second)
	if !strings.Contains(got, "5 obj/s") || !strings.Contains(got, "ETA 0:01") {
		t.Errorf("objRateETA = %q, want '5 obj/s' and ETA 0:01", got)
	}
}

// The summary text used to live on the controller's downloadSummary. It moved
// here with the rest of the reporting, and it is pure, so the wording and the
// caps are testable without a terminal.

func TestSummaryTextStatusLine(t *testing.T) {
	d := ops.Descriptor{Kind: ops.KindDownload, TotalItems: 3}

	if got := SummaryText(d, ops.Result{Done: 3}); !strings.Contains(got, "Download complete.") {
		t.Errorf("clean run:\n%s", got)
	}
	if got := SummaryText(d, ops.Result{Canceled: true}); !strings.Contains(got, "Download canceled.") {
		t.Errorf("cancelled run:\n%s", got)
	}
	withErr := ops.Result{Failures: []ops.Failure{{Label: "k", Err: errors.New("boom")}}}
	got := SummaryText(d, withErr)
	if !strings.Contains(got, "Download finished with errors.") {
		t.Errorf("failed run:\n%s", got)
	}
	if !strings.Contains(got, "k: boom") {
		t.Errorf("the failure detail is missing:\n%s", got)
	}
}

func TestSummaryTextCountsAndCaps(t *testing.T) {
	d := ops.Descriptor{Kind: ops.KindDownload, TotalItems: 12}
	res := ops.Result{Done: 2, Skipped: 3, Overwritten: 1, Bytes: 2048}
	for i := range 10 {
		res.Failures = append(res.Failures, ops.Failure{
			Label: fmt.Sprintf("key%d", i), Err: errors.New("err"),
		})
	}

	got := SummaryText(d, res)
	for _, want := range []string{"Done: 2 of 12", "Skipped: 3", "Overwritten: 1", "Failed: 10", "2.0 KiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
	// Only the first eight failures are listed; the rest are summarised, or a
	// 5000-object run would scroll the modal into uselessness.
	if !strings.Contains(got, "...and 2 more") {
		t.Errorf("overflow note missing:\n%s", got)
	}
	if strings.Contains(got, "key9") {
		t.Errorf("listed past the cap:\n%s", got)
	}
}

// Verification counts only appear when a verify actually ran.
func TestSummaryTextVerificationLine(t *testing.T) {
	d := ops.Descriptor{Kind: ops.KindDownload, TotalItems: 2}

	if got := SummaryText(d, ops.Result{Done: 2}); strings.Contains(got, "Verified") {
		t.Errorf("a run with no verification mentioned it:\n%s", got)
	}
	got := SummaryText(d, ops.Result{Done: 2, Verified: 1, Unverified: 1})
	if !strings.Contains(got, "Verified: 1") || !strings.Contains(got, "1 not verifiable") {
		t.Errorf("verification line:\n%s", got)
	}
}

func TestProgressTextPhaseAndTransfer(t *testing.T) {
	d := ops.Descriptor{Kind: ops.KindDownload}

	phase := ProgressText(d, ops.Progress{Phase: "resolving", Items: 2, TotalItems: 9}, time.Second)
	if !strings.Contains(phase, "resolving") || !strings.Contains(phase, "2 / 9") {
		t.Errorf("phase line = %q", phase)
	}

	moving := ProgressText(d, ops.Progress{
		Bytes: 512, TotalBytes: 1024, Items: 1, TotalItems: 2, Active: []string{"a.bin"},
	}, time.Second)
	for _, want := range []string{"Download", "1 / 2 done", "50.0%", "a.bin"} {
		if !strings.Contains(moving, want) {
			t.Errorf("transfer line is missing %q: %q", want, moving)
		}
	}
}

// A long list of in-flight names must not push the gauge off the modal.
func TestProgressTextTruncatesTheActiveList(t *testing.T) {
	names := make([]string, 20)
	for i := range names {
		names[i] = fmt.Sprintf("file-with-a-long-name-%d.bin", i)
	}
	got := ProgressText(ops.Descriptor{Kind: ops.KindDownload},
		ops.Progress{Bytes: 1, TotalBytes: 2, Active: names}, time.Second)

	for _, line := range strings.Split(got, "\n") {
		if len([]rune(line)) > 60 {
			t.Errorf("line is %d runes, too long: %q", len([]rune(line)), line)
		}
	}
}

func TestProgressTextWithNothingInFlight(t *testing.T) {
	got := ProgressText(ops.Descriptor{Kind: ops.KindUpload}, ops.Progress{TotalBytes: 100}, 0)
	if !strings.Contains(got, "starting...") {
		t.Errorf("an idle sample should say so: %q", got)
	}
	if strings.Contains(got, "NaN") || strings.Contains(got, "+Inf") {
		t.Errorf("zero bytes produced a bad percentage: %q", got)
	}
}
