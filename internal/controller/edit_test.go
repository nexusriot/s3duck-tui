package controller

import (
	"bytes"
	"strings"
	"testing"
)

func TestIsProbablyBinary(t *testing.T) {
	t.Run("text is not binary", func(t *testing.T) {
		for _, in := range [][]byte{
			[]byte("plain text\nwith lines\n"),
			[]byte("unicode: привет 🦆"),
			{},
			nil,
		} {
			if isProbablyBinary(in) {
				t.Errorf("%q flagged binary", in)
			}
		}
	})

	t.Run("a NUL byte marks binary", func(t *testing.T) {
		if !isProbablyBinary([]byte("PK\x03\x04\x00rest")) {
			t.Error("zip header not flagged")
		}
	})

	t.Run("only the head is sniffed", func(t *testing.T) {
		// A NUL beyond the sniff window is invisible — the guard is a
		// heuristic, not a scan; this pins that trade-off.
		data := append(bytes.Repeat([]byte("a"), binarySniffLen), 0)
		if isProbablyBinary(data) {
			t.Error("NUL past the sniff window should not flag")
		}
	})
}

func TestEditorCommand(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("EDITOR wins and splits into args", func(t *testing.T) {
		name, args, err := editorCommand(env(map[string]string{"EDITOR": "emacs -nw", "VISUAL": "code"}))
		if err != nil || name != "emacs" || len(args) != 1 || args[0] != "-nw" {
			t.Errorf("got %q %v %v", name, args, err)
		}
	})

	t.Run("VISUAL is the fallback", func(t *testing.T) {
		name, _, err := editorCommand(env(map[string]string{"VISUAL": "nano"}))
		if err != nil || name != "nano" {
			t.Errorf("got %q %v", name, err)
		}
	})

	t.Run("vi is the last resort", func(t *testing.T) {
		name, _, err := editorCommand(env(map[string]string{}))
		// vi exists on this machine; if not, the error must say why.
		if err != nil {
			if !strings.Contains(err.Error(), "vi") {
				t.Errorf("err = %v", err)
			}
			return
		}
		if name != "vi" {
			t.Errorf("got %q", name)
		}
	})
}

func TestTempSuffix(t *testing.T) {
	cases := map[string]string{
		"dir/config.yaml":     ".yaml",
		"notes.txt":           ".txt",
		"no-extension":        "",
		"trailing.":           "",
		"weird.y a?ml":        "",
		"dir/x.verylongext11": "",
		"a.J2":                ".J2",
	}
	for in, want := range cases {
		if got := tempSuffix(in); got != want {
			t.Errorf("tempSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
