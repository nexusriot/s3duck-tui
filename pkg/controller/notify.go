package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
)

// statusTick is how often the badge is recomputed. Slow enough to be free,
// fast enough that a percentage does not look stuck.
const statusTick = 500 * time.Millisecond

// noticeFor is how long a completion notice holds the status field before the
// badge (or nothing) takes it back.
const noticeFor = 6 * time.Second

// transferBadge renders the header's transfer indicator for a job snapshot:
// how many are active and their combined byte progress. Empty when nothing is
// running, so an idle app carries no chrome it does not need.
//
// Pure, so the arithmetic that decides "45%" is testable.
func transferBadge(jobs []jobView) string {
	active, done, total := 0, int64(0), int64(0)
	for _, j := range jobs {
		if j.status != jobRunning && j.status != jobQueued {
			continue
		}
		active++
		done += j.done
		total += j.total
	}
	if active == 0 {
		return ""
	}
	label := fmt.Sprintf(" ⇅ %d transfer", active)
	if active > 1 {
		label += "s"
	}
	if total > 0 {
		pct := float64(done) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
		label += fmt.Sprintf(" · %.0f%% (%s)", pct, humanize.IBytes(uint64(done)))
	}
	return label + " "
}

// noticeText renders a finished job as a one-line notice.
func noticeText(j jobView) string {
	mark := "✓"
	if j.status == jobFailed {
		mark = "✗"
	} else if j.status == jobCanceled {
		mark = "⃠"
	}
	desc := j.desc
	const maxDesc = 40
	if len([]rune(desc)) > maxDesc {
		desc = string([]rune(desc)[:maxDesc-1]) + "…"
	}
	out := fmt.Sprintf(" %s %s %s: %s ", mark, j.kind, desc, j.status)
	if j.failed > 0 {
		out = fmt.Sprintf(" %s %s %s: %s (%d failed) ", mark, j.kind, desc, j.status, j.failed)
	}
	return out
}

// setNotice publishes a transient status message. Safe from any goroutine.
func (c *Controller) setNotice(msg string) {
	c.jobsMu.Lock()
	c.notice = msg
	c.noticeUntil = time.Now().Add(noticeFor)
	c.jobsMu.Unlock()
}

// currentStatus is what the header's right field should show right now: a live
// notice if one is unexpired, otherwise the transfer badge.
func (c *Controller) currentStatus() string {
	c.jobsMu.Lock()
	notice, until := c.notice, c.noticeUntil
	c.jobsMu.Unlock()
	if notice != "" && time.Now().Before(until) {
		return notice
	}
	return transferBadge(c.jobSnapshot())
}

// runStatusTicker keeps the header's status field current. One long-lived
// goroutine for the app's lifetime: it compares against the text it last
// published and queues a redraw only on a change, so an idle app costs a
// string comparison twice a second and no draws at all.
func (c *Controller) runStatusTicker() {
	go func() {
		t := time.NewTicker(statusTick)
		defer t.Stop()
		last := ""
		for range t.C {
			next := c.currentStatus()
			if next == last {
				continue
			}
			last = next
			c.view.App.QueueUpdateDraw(func() { c.view.SetStatus(next) })
		}
	}()
}

// announceJob is called when a job reaches a terminal state. A foreground job
// already has the user's attention — its own modal is showing the result — so
// only a backgrounded one is announced.
func (c *Controller) announceJob(j jobView) {
	if !j.bg {
		return
	}
	c.setNotice(noticeText(j))
	// The bell only for outcomes worth interrupting for: a completed transfer
	// the user walked away from, or a failure. Not for their own cancel.
	if j.status != jobCanceled {
		c.view.App.QueueUpdateDraw(func() { c.view.Beep() })
	}
	c.logActivity("%s %s finished: %s", j.kind, j.desc, strings.TrimSpace(j.status.String()))
}
