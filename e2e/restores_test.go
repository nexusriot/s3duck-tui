//go:build e2e

package e2e

import (
	"testing"
)

// , r opens the tracker; MinIO has no archive tier, so it is empty and says
// how to fill it.
func TestRestoreTrackerOpens(t *testing.T) {
	s := newStore(t)
	s.put("k.txt", "x")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, ""))

	u := startTUI(t, h)
	u.openProfile(s.name, "")
	u.waitFor("k.txt")
	u.typeText(",")
	u.typeText("r")
	u.waitFor("Glacier restores", "no restores tracked")
	u.press("Escape")
}
