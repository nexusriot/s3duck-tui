package flows

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
)

// Rate and ETA arithmetic for progress lines. Pure, and here rather than in
// the controller because the only things that need it are the progress and
// transfer-panel renderers.

// Throughput returns the transfer rate (units/sec) and estimated time remaining
// for `done` of `total` units over `elapsed`. rate is 0 when it can't be
// computed yet; eta is -1 (unknown) when there is no measurable rate, total is
// unknown, or the transfer is already complete.
func Throughput(done, total int64, elapsed time.Duration) (rate float64, eta time.Duration) {
	secs := elapsed.Seconds()
	if secs <= 0 || done <= 0 {
		return 0, -1
	}
	rate = float64(done) / secs
	if rate <= 0 || total <= 0 || done >= total {
		return rate, -1
	}
	// Computed in float and capped: converting whole seconds to a Duration
	// and then multiplying overflowed int64 at very low rates (1 B of 5 TiB
	// after 100 s rendered as a wrapped, sometimes negative, ETA). Past the
	// cap an estimate is noise anyway, so it reads as unknown.
	left := float64(total-done) / rate
	if left > maxETA.Seconds() {
		return rate, -1
	}
	return rate, time.Duration(left * float64(time.Second))
}

// maxETA is the longest estimate worth showing.
const maxETA = 1000 * time.Hour

// FormatDuration renders a non-negative duration as M:SS (or H:MM:SS past an
// hour). A negative duration — an unknown ETA — renders as "--".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		return "--"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// ByteRateETA formats a one-line "1.2 MiB/s · ETA 3:45" suffix for a byte
// transfer; unknown values render as "--".
func ByteRateETA(done, total int64, elapsed time.Duration) string {
	rate, eta := Throughput(done, total, elapsed)
	rateStr := "--"
	if rate > 0 {
		rateStr = humanize.IBytes(uint64(rate)) + "/s"
	}
	return fmt.Sprintf("%s · ETA %s", rateStr, FormatDuration(eta))
}

// ObjRateETA is ByteRateETA for object-count transfers (server-side copy/move).
func ObjRateETA(done, total int, elapsed time.Duration) string {
	rate, eta := Throughput(int64(done), int64(total), elapsed)
	rateStr := "--"
	if rate > 0 {
		rateStr = fmt.Sprintf("%.0f obj/s", rate)
	}
	return fmt.Sprintf("%s · ETA %s", rateStr, FormatDuration(eta))
}
