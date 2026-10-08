package controller

import (
	"time"

	"github.com/nexusriot/s3duck-tui/internal/jobs"
)

// runStatusTicker keeps the header's status field current. One long-lived
// goroutine for the app's lifetime: it compares against the text it last
// published and queues a redraw only on a change, so an idle app costs a
// string comparison twice a second and no draws at all.
//
// The text itself is the queue's business (jobs.Queue.Status decides between a
// live notice and the transfer badge); this is only the pump that gets it onto
// the frame.
func (c *Controller) runStatusTicker() {
	go func() {
		t := time.NewTicker(jobs.StatusTick)
		defer t.Stop()
		last := ""
		for range t.C {
			next := c.queue.Status()
			if next == last {
				continue
			}
			last = next
			c.shell.Do(func() { c.view.SetStatus(next) })
		}
	}()
}
