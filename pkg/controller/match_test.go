package controller

import (
	"testing"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

func TestNewMatcherKinds(t *testing.T) {
	cases := []struct {
		q    string
		kind matchKind
	}{
		{"", matchSubstring},
		{"report", matchSubstring},
		{"*.log", matchGlob},
		{"a?c", matchGlob},
		{"re:^2024", matchRegex},
		{"RE:^2024", matchRegex},
	}
	for _, tc := range cases {
		m, err := newMatcher(tc.q)
		if err != nil {
			t.Fatalf("newMatcher(%q) errored: %v", tc.q, err)
		}
		if m.kind != tc.kind {
			t.Errorf("newMatcher(%q).kind = %d, want %d", tc.q, m.kind, tc.kind)
		}
	}
}

func TestNewMatcherBadRegex(t *testing.T) {
	for _, q := range []string{"re:[", "re:("} {
		m, err := newMatcher(q)
		if err == nil {
			t.Errorf("newMatcher(%q) accepted a broken pattern", q)
		}
		if !m.broken() || m.match("anything") {
			t.Errorf("newMatcher(%q) must degrade to matching nothing", q)
		}
	}
	// The live-filter path must not propagate the error.
	if !mustMatcher("re:[").broken() {
		t.Error("mustMatcher should mark a broken pattern")
	}
}

func TestMatcherEmptyMatchesEverything(t *testing.T) {
	m := mustMatcher("   ")
	if !m.empty() {
		t.Fatal("a whitespace-only query should be empty")
	}
	if !m.match("anything at all") {
		t.Error("the empty matcher must accept everything")
	}
}

func TestMatcherSubstringIsCaseInsensitive(t *testing.T) {
	m := mustMatcher("Report")
	if !m.match("QUARTERLY-report.txt") {
		t.Error("substring match should ignore case")
	}
	if m.match("summary.txt") {
		t.Error("unrelated name matched")
	}
}

func TestMatcherGlobIsAnchored(t *testing.T) {
	m := mustMatcher("*.log")
	if !m.match("app.log") {
		t.Error("*.log should match app.log")
	}
	// "*" crosses "/" deliberately: recursive search matches full keys.
	if !m.match("logs/2024/app.LOG") {
		t.Error("*.log should match a nested key, case-insensitively")
	}
	if m.match("app.log.gz") {
		t.Error("a glob is a full match, so app.log.gz must not match *.log")
	}
	if !mustMatcher("report?.txt").match("report1.txt") {
		t.Error("? should match exactly one character")
	}
	if mustMatcher("report?.txt").match("report10.txt") {
		t.Error("? must not match two characters")
	}
}

func TestGlobMatchEdgeCases(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"**", "anything", true},
		{"", "", true},
		{"", "x", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"*a*a*a*a*b", "aaaaaaaaaaaaaaaaaaaa", false}, // the backtracking case
		{"?", "", false},
		{"?", "é", true},
		{"a*", "a", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestMatcherRegex(t *testing.T) {
	m := mustMatcher(`re:^20\d\d-`)
	if !m.match("2024-report") {
		t.Error("regex should match")
	}
	if m.match("v2024-report") {
		t.Error("^ should anchor")
	}
	// Case-insensitive by default, overridable with (?-i).
	if !mustMatcher("re:abc").match("ABC") {
		t.Error("regex should default to case-insensitive")
	}
	if mustMatcher("re:(?-i)abc").match("ABC") {
		t.Error("(?-i) should restore case sensitivity")
	}
}

func TestMatchKeySkipsFolderMarkers(t *testing.T) {
	m := mustMatcher("logs")
	if matchKey(m, "logs/") {
		t.Error("folder markers must never match")
	}
	if matchKey(m, "") {
		t.Error("empty key must never match")
	}
	if !matchKey(m, "logs/app.txt") {
		t.Error("a real key under the match should hit")
	}
}

func TestMatchLabel(t *testing.T) {
	cases := map[string]string{
		"x":     "text",
		"*.log": "glob",
		"re:x":  "regex",
		"re:[":  "bad pattern",
	}
	for q, want := range cases {
		if got := mustMatcher(q).matchLabel(); got != want {
			t.Errorf("matchLabel(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestFilterSortObjectsHonoursGlob(t *testing.T) {
	obj := func(name string) *model.Object {
		n := name
		return &model.Object{Key: &n, Ot: model.File}
	}
	objs := []*model.Object{obj("app.log"), obj("app.txt"), obj("other.log")}
	got := filterSortObjects(objs, "*.log", sortName, false)
	if len(got) != 2 {
		t.Fatalf("glob filter kept %d objects, want 2", len(got))
	}
	// A broken pattern must empty the listing, not pass everything through.
	if n := len(filterSortObjects(objs, "re:[", sortName, false)); n != 0 {
		t.Errorf("broken filter kept %d objects, want 0", n)
	}
}
