package screens

import (
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/jobs"
)

func TestTransferRow(t *testing.T) {
	jv := jobs.View{
		Kind: "download", Desc: "5 obj → /dl", Status: jobs.Running,
		Total: 200, Done: 100, Count: 5, DoneCount: 2,
	}
	primary, secondary := TransferRow(jv, time.Second)
	if !strings.Contains(primary, "running") || !strings.Contains(primary, "download") {
		t.Errorf("primary = %q", primary)
	}
	if !strings.Contains(secondary, "2/5 obj") || !strings.Contains(secondary, "50%") {
		t.Errorf("secondary = %q, want 2/5 obj and 50%%", secondary)
	}
	// A finished job shows "done" instead of a live rate/ETA.
	jv.Status = jobs.Done
	if _, sec := TransferRow(jv, time.Second); !strings.Contains(sec, "done") {
		t.Errorf("done-row secondary = %q, want 'done'", sec)
	}
}

// The row is rendered by a tview.List, which parses tags: "[failed]" is one,
// so an unescaped status vanished and a failed job read like a finished one.
func TestTransferRowStatusSurvivesTagParsing(t *testing.T) {
	for _, st := range []jobs.Status{jobs.Running, jobs.Queued, jobs.Done, jobs.Failed, jobs.Canceled} {
		jv := jobs.View{Kind: "sync", Desc: "photos[old]/ → b", Status: st, Total: 10, Count: 1}
		primary, secondary := TransferRow(jv, time.Second)
		shown := tview.TaggedStringWidth(primary)
		// The same text with the brackets swapped for parentheses has the
		// same width and no tags to swallow.
		want := tview.TaggedStringWidth("(" + st.String() + ") sync — photos(old)/ → b")
		if shown != want {
			t.Errorf("%s: row renders %d cells, want %d — part of %q was read as a tag", st, shown, want, primary)
		}
		if st != jobs.Running && !strings.Contains(secondary, st.String()) {
			t.Errorf("%s: secondary = %q, want it to name the state", st, secondary)
		}
	}
}
