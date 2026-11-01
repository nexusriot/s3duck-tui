//go:build e2e

package e2e

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// , e on a folder writes the rows on screen — filter applied — as CSV in the
// download directory, a key with a comma intact.
func TestExportListingToCSV(t *testing.T) {
	s := newStore(t)
	s.put("rep/a,b.csv", "12345")
	s.put("rep/c.txt", "x")
	s.put("rep/sub/d.txt", "y")
	h := newHome(t)
	h.writeProfiles(h.profile(s.name, "rep/"))

	u := startTUIWith(t, h, noClipboard)
	u.openProfile(s.name, "rep/")
	u.waitFor("a,b.csv", "c.txt", "sub/")
	u.press("/")
	u.typeText("re:\\.(csv|txt)$")
	u.press("Enter")
	u.waitUntil("the filter to hide sub/", func(scr string) bool { return !strings.Contains(scr, "sub/") })
	u.typeText(",")
	u.typeText("e")
	u.waitFor("Export 2 row(s)", "CSV", "JSON")
	u.press("Enter") // CSV
	u.waitFor("Exported 2 row(s) to")

	matches, _ := filepath.Glob(filepath.Join(h.downloads, "*.csv"))
	if len(matches) != 1 {
		t.Fatalf("exports in the download dir: %v", matches)
	}
	data, _ := os.ReadFile(matches[0])
	recs, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("CSV %v: %q", err, data)
	}
	keys := recs[1][1] + "|" + recs[2][1]
	if !strings.Contains(keys, "rep/a,b.csv") || !strings.Contains(keys, "rep/c.txt") || recs[1][0] != s.name {
		t.Errorf("rows = %q", recs[1:])
	}
}
