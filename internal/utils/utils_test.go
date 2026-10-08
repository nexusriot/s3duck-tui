package utils

import (
	"strings"
	"testing"
)

func TestSplitFunc(t *testing.T) {
	if !SplitFunc('/') {
		t.Errorf("SplitFunc('/') = false, want true")
	}
	if SplitFunc('a') {
		t.Errorf("SplitFunc('a') = true, want false")
	}
	if SplitFunc(' ') {
		t.Errorf("SplitFunc(' ') = true, want false")
	}
}

func TestRandStr(t *testing.T) {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

	if got := RandStr(0); got != "" {
		t.Errorf("RandStr(0) = %q, want empty string", got)
	}

	for _, n := range []int{1, 4, 16, 64} {
		s := RandStr(n)
		if len(s) != n {
			t.Errorf("len(RandStr(%d)) = %d, want %d", n, len(s), n)
		}
		for _, r := range s {
			if !strings.ContainsRune(letters, r) {
				t.Errorf("RandStr(%d) produced out-of-charset rune %q", n, r)
			}
		}
	}
}
