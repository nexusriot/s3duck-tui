//go:build integration

package controller

import (
	"context"
	"strings"
	"testing"
)

// The local diff against bodies a server really returned.
func TestIntegrationDiffLocalCornerCases(t *testing.T) {
	m := itModel(t)
	ctx := context.Background()
	b := itBucket(t, m, "s3duck-it-diff-corner")

	t.Run("line endings only", func(t *testing.T) {
		itPut(t, m, b, "crlf.txt", "a\r\nb\r\nc\r\n")
		remote, err := m.GetVersionContent(ctx, b, "crlf.txt", "", editMaxSize)
		if err != nil {
			t.Fatal(err)
		}
		out, err := compareLocal([]byte("a\nb\nc\n"), remote, "local", "object")
		if err != nil {
			t.Fatal(err)
		}
		if out.identical {
			t.Error("CRLF vs LF is a real difference and must not be called identical")
		}
		if !strings.Contains(out.message, "line endings") {
			t.Errorf("message = %q, want it to name the line endings", out.message)
		}
	})

	t.Run("identical bytes", func(t *testing.T) {
		itPut(t, m, b, "same.txt", "a\nb\n")
		remote, err := m.GetVersionContent(ctx, b, "same.txt", "", editMaxSize)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := compareLocal([]byte("a\nb\n"), remote, "local", "object")
		if !out.identical {
			t.Errorf("round-tripped bytes should be identical: %+v", out)
		}
	})

	t.Run("object over the cap is declined", func(t *testing.T) {
		itPut(t, m, b, "huge.txt", strings.Repeat("x", editMaxSize+1024))
		if _, err := m.GetVersionContent(ctx, b, "huge.txt", "", editMaxSize); err == nil {
			t.Error("an object over the diff cap must be refused, not loaded")
		}
	})
}
