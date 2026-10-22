package controller

import (
	"reflect"
	"sort"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

func markFixture() []*model.Object {
	mk := func(name, full string, ot model.FType) *model.Object {
		return &model.Object{Key: &name, FullPath: &full, Ot: ot}
	}
	return []*model.Object{
		mk("app.log", "p/app.log", model.File),
		mk("app.log.1", "p/app.log.1", model.File),
		mk("db.log", "p/db.log", model.File),
		mk("notes.txt", "p/notes.txt", model.File),
		mk("logs", "p/logs/", model.Folder),
		mk("..", "", model.Bucket), // never markable
	}
}

func TestNamesMatchingUsesTheQuerySyntax(t *testing.T) {
	scope := markScope(markFixture(), "", sortName, false)
	cases := []struct {
		query   string
		folders bool
		want    []string
	}{
		{"*.log", false, []string{"p/app.log", "p/db.log"}},
		{"log", false, []string{"p/app.log", "p/app.log.1", "p/db.log"}},
		{"log", true, []string{"p/app.log", "p/app.log.1", "p/db.log", "p/logs/"}},
		{`re:^app\.log(\.\d+)?$`, false, []string{"p/app.log", "p/app.log.1"}},
		{"*", false, []string{"p/app.log", "p/app.log.1", "p/db.log", "p/notes.txt"}},
	}
	for _, c := range cases {
		m, err := newMatcher(c.query)
		if err != nil {
			t.Fatal(err)
		}
		got := namesMatching(scope, m, !c.folders)
		sort.Strings(got)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q (folders=%v) = %v, want %v", c.query, c.folders, got, c.want)
		}
	}
}

func TestMarkScopeHonoursTheFilter(t *testing.T) {
	scope := markScope(markFixture(), "app", sortName, false)
	if len(scope) != 2 {
		t.Fatalf("scope = %d rows, want only what the filter shows (2)", len(scope))
	}
	for _, o := range scope {
		if o.Ot != model.File && o.Ot != model.Folder {
			t.Errorf("non-markable row %q in the scope", *o.Key)
		}
	}
}
