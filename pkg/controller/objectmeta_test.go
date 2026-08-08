package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/s3duck-tui/pkg/model"
)

func TestParseKVLines(t *testing.T) {
	t.Run("parses pairs, skipping blanks and comments", func(t *testing.T) {
		got, err := parseKVLines("a=1\n\n# a comment\n b = 2 \n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("only the first = separates", func(t *testing.T) {
		got, err := parseKVLines("url=https://x/y?a=b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["url"] != "https://x/y?a=b" {
			t.Errorf("got %q", got["url"])
		}
	})

	t.Run("an empty value is allowed", func(t *testing.T) {
		got, err := parseKVLines("k=")
		if err != nil || got["k"] != "" {
			t.Errorf("got %v, %v", got, err)
		}
	})

	t.Run("a line without = is an error, not silently dropped", func(t *testing.T) {
		if _, err := parseKVLines("a=1\noops\n"); err == nil {
			t.Error("want an error")
		} else if !strings.Contains(err.Error(), "line 2") {
			t.Errorf("error should name the line: %v", err)
		}
	})

	t.Run("empty key is an error", func(t *testing.T) {
		if _, err := parseKVLines("=v"); err == nil {
			t.Error("want an error")
		}
	})

	t.Run("duplicate keys are an error", func(t *testing.T) {
		if _, err := parseKVLines("a=1\na=2"); err == nil {
			t.Error("want an error for a duplicate key")
		}
	})

	t.Run("empty input yields an empty map", func(t *testing.T) {
		got, err := parseKVLines("")
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v", got, err)
		}
	})
}

func TestFormatKVLines(t *testing.T) {
	got := formatKVLines(map[string]string{"z": "26", "a": "1", "m": ""})
	if want := "a=1\nm=\nz=26\n"; got != want {
		t.Errorf("got %q, want %q (sorted)", got, want)
	}
	if got := formatKVLines(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestKVRoundTrip(t *testing.T) {
	in := map[string]string{"owner": "vlad", "purpose": "backup archive"}
	out, err := parseKVLines(formatKVLines(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %v, want %v", out, in)
	}
	for k, v := range in {
		if out[k] != v {
			t.Errorf("%q: got %q, want %q", k, out[k], v)
		}
	}
}

func TestValidateTags(t *testing.T) {
	t.Run("within the limits", func(t *testing.T) {
		if err := validateTags(map[string]string{"a": "1"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if err := validateTags(nil); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("too many tags", func(t *testing.T) {
		tags := map[string]string{}
		for i := 0; i < maxObjectTags+1; i++ {
			tags[string(rune('a'+i))] = "v"
		}
		if err := validateTags(tags); err == nil {
			t.Error("want an error over the tag-count limit")
		}
	})

	t.Run("key and value length limits", func(t *testing.T) {
		if err := validateTags(map[string]string{strings.Repeat("k", maxTagKeyLen+1): "v"}); err == nil {
			t.Error("want an error for an over-long key")
		}
		if err := validateTags(map[string]string{"k": strings.Repeat("v", maxTagValueLen+1)}); err == nil {
			t.Error("want an error for an over-long value")
		}
		// Exactly at the limit is fine.
		if err := validateTags(map[string]string{strings.Repeat("k", maxTagKeyLen): strings.Repeat("v", maxTagValueLen)}); err != nil {
			t.Errorf("boundary should be allowed: %v", err)
		}
	})
}

func TestSameMeta(t *testing.T) {
	base := model.ObjectMeta{
		ContentType:  "text/plain",
		CacheControl: "max-age=60",
		UserMetadata: map[string]string{"a": "1"},
	}

	same := base
	same.UserMetadata = map[string]string{"a": "1"}
	if !sameMeta(base, same) {
		t.Error("identical metadata should compare equal across distinct maps")
	}

	t.Run("each editable field is compared", func(t *testing.T) {
		for name, mutate := range map[string]func(m *model.ObjectMeta){
			"content-type":        func(m *model.ObjectMeta) { m.ContentType = "application/json" },
			"cache-control":       func(m *model.ObjectMeta) { m.CacheControl = "no-store" },
			"content-disposition": func(m *model.ObjectMeta) { m.ContentDisposition = "attachment" },
			"content-encoding":    func(m *model.ObjectMeta) { m.ContentEncoding = "gzip" },
			"user metadata":       func(m *model.ObjectMeta) { m.UserMetadata = map[string]string{"a": "2"} },
		} {
			other := base
			other.UserMetadata = map[string]string{"a": "1"}
			mutate(&other)
			if sameMeta(base, other) {
				t.Errorf("%s change was not detected", name)
			}
		}
	})

	t.Run("read-only fields are ignored", func(t *testing.T) {
		other := base
		other.UserMetadata = map[string]string{"a": "1"}
		other.Size = 999
		other.ETag = "different"
		if !sameMeta(base, other) {
			t.Error("size/etag must not count as an edit")
		}
	})
}

func TestMetaSummary(t *testing.T) {
	when := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	got := metaSummary(model.ObjectMeta{
		Size: 2048, ETag: "abc", LastModified: &when, StorageClass: "GLACIER",
	})
	for _, want := range []string{"2.0 KiB", "abc", "2026-07-30", "GLACIER", "archived (not restored)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}

	plain := metaSummary(model.ObjectMeta{Size: 1, ETag: "e", StorageClass: "STANDARD"})
	if strings.Contains(plain, "archived") {
		t.Errorf("a STANDARD object should carry no restore note: %s", plain)
	}
}
