package controller

import (
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ops"
)

func TestArchiveBaseName(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 4, 5, 0, time.UTC)
	name := "logs"
	one := []*model.Object{{Key: &name, Ot: model.Folder}}
	if got := archiveBaseName("b", "data/", one, now); got != "logs" {
		t.Errorf("one item: %q, want its own name", got)
	}
	two := append(one, one[0])
	if got := archiveBaseName("b", "data/2026/", two, now); got != "b-2026-20261022-150405" {
		t.Errorf("several items: %q", got)
	}
	if got := archiveBaseName("b", "", two, now); got != "b-20261022-150405" {
		t.Errorf("bucket root: %q", got)
	}
}

func TestArchiveFileNameSwapsTheExtension(t *testing.T) {
	cases := []struct {
		in   string
		f    ops.ArchiveFormat
		want string
	}{
		{"logs", ops.ArchiveZip, "logs.zip"},
		{"logs.tar.gz", ops.ArchiveZip, "logs.zip"},
		{"logs.zip", ops.ArchiveTarGz, "logs.tar.gz"},
		{"logs.ZIP", ops.ArchiveZip, "logs.zip"},
		{" logs.tar.gz ", ops.ArchiveTarGz, "logs.tar.gz"},
		{"report.v2", ops.ArchiveTarGz, "report.v2.tar.gz"},
	}
	for _, c := range cases {
		if got := archiveFileName(c.in, c.f); got != c.want {
			t.Errorf("archiveFileName(%q, %s) = %q, want %q", c.in, c.f, got, c.want)
		}
	}
}

func TestValidateArchiveFileName(t *testing.T) {
	for _, bad := range []string{"", "  ", "a/b.zip", `a\b.zip`, ".."} {
		if validateArchiveFileName(bad) == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if err := validateArchiveFileName("logs.tar.gz"); err != nil {
		t.Errorf("a plain name was refused: %v", err)
	}
}
