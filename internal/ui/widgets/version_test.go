package widgets

import (
	"os"
	"regexp"
	"testing"
)

// The version is stated in three places that cannot import each other: the
// string the TUI shows, the Makefile's packaging default, and build-deb.sh.
// The Makefile says to bump all three by hand, which is exactly the kind of
// duplicated constant that drifts — a .deb labelled 0.10.2 carrying a binary
// that reports 0.10.1 is a support question nobody can answer.
var (
	// "S3Duck 🦆 TUI v.0.10.2" -> 0.10.2
	versionInText = regexp.MustCompile(`v\.(\d+\.\d+\.\d+)`)
	// "VERSION    ?= 0.10.2"
	versionInMake = regexp.MustCompile(`(?m)^VERSION\s*\?=\s*(\d+\.\d+\.\d+)\s*$`)
	// "version=0.10.2"
	versionInShell = regexp.MustCompile(`(?m)^version=(\d+\.\d+\.\d+)\s*$`)
)

func captureVersion(t *testing.T, re *regexp.Regexp, source, label string) string {
	t.Helper()
	m := re.FindStringSubmatch(source)
	if m == nil {
		t.Fatalf("no version found in %s; the format changed and this test can no longer check it", label)
	}
	return m[1]
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../../../" + name)
	if err != nil {
		t.Fatalf("cannot read %s: %v", name, err)
	}
	return string(data)
}

func TestVersionMatchesPackaging(t *testing.T) {
	app := captureVersion(t, versionInText, versionText, "widgets.versionText")
	make := captureVersion(t, versionInMake, readRepoFile(t, "Makefile"), "Makefile")
	deb := captureVersion(t, versionInShell, readRepoFile(t, "build-deb.sh"), "build-deb.sh")

	if app != make {
		t.Errorf("version drift: the app reports %s, the Makefile packages %s", app, make)
	}
	if app != deb {
		t.Errorf("version drift: the app reports %s, build-deb.sh packages %s", app, deb)
	}
}

// The .deb control file carries a placeholder that both packaging paths
// substitute. A literal version there would ship whatever was last committed,
// silently overriding the VERSION the build was asked for.
func TestDebianControlKeepsTheVersionPlaceholder(t *testing.T) {
	control := readRepoFile(t, "DEBIAN/control")
	if !regexp.MustCompile(`(?m)^Version: _version_\s*$`).MatchString(control) {
		t.Errorf("DEBIAN/control does not carry the _version_ placeholder that the Makefile and build-deb.sh substitute:\n%s", control)
	}
}
