package model

import (
	"context"
	"strings"
	"testing"
)

func TestParseRestoreStatus(t *testing.T) {
	t.Run("completed restore with an expiry", func(t *testing.T) {
		ongoing, expiry, ok := ParseRestoreStatus(`ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`)
		if !ok || ongoing {
			t.Errorf("ongoing=%v ok=%v, want false/true", ongoing, ok)
		}
		if expiry != "Fri, 21 Dec 2012 00:00:00 GMT" {
			t.Errorf("expiry = %q", expiry)
		}
	})

	t.Run("restore still running", func(t *testing.T) {
		ongoing, expiry, ok := ParseRestoreStatus(`ongoing-request="true"`)
		if !ok || !ongoing || expiry != "" {
			t.Errorf("ongoing=%v expiry=%q ok=%v", ongoing, expiry, ok)
		}
	})

	t.Run("absent or unparseable headers report not-ok", func(t *testing.T) {
		for _, in := range []string{"", "   ", "garbage", `ongoing-request="maybe"`} {
			if _, _, ok := ParseRestoreStatus(in); ok {
				t.Errorf("ParseRestoreStatus(%q) reported ok", in)
			}
		}
	})
}

func TestRestoreSummary(t *testing.T) {
	cases := []struct{ class, header, want string }{
		{"STANDARD", "", ""},
		{"GLACIER", "", "archived (not restored)"},
		{"DEEP_ARCHIVE", `ongoing-request="true"`, "restore in progress"},
		{"GLACIER", `ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`, "restored until Fri, 21 Dec 2012 00:00:00 GMT"},
		{"GLACIER", `ongoing-request="false"`, "restored"},
	}
	for _, c := range cases {
		if got := RestoreSummary(c.class, c.header); got != c.want {
			t.Errorf("RestoreSummary(%q, %q) = %q, want %q", c.class, c.header, got, c.want)
		}
	}
}

func TestIsArchived(t *testing.T) {
	for _, c := range []string{"GLACIER", "glacier", "DEEP_ARCHIVE"} {
		if !IsArchived(c) {
			t.Errorf("IsArchived(%q) = false", c)
		}
	}
	// GLACIER_IR is instant-retrieval: it needs no restore, so it must not be
	// treated as archived even though the name starts with GLACIER.
	for _, c := range []string{"STANDARD", "STANDARD_IA", "GLACIER_IR", ""} {
		if IsArchived(c) {
			t.Errorf("IsArchived(%q) = true", c)
		}
	}
}

func TestStorageClassesIncludeTheArchivedOnes(t *testing.T) {
	classes := StorageClasses()
	for _, a := range ArchivedStorageClasses() {
		found := false
		for _, c := range classes {
			if c == a {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is archived but not offered in StorageClasses()", a)
		}
	}
	if len(RestoreTiers()) == 0 {
		t.Error("RestoreTiers() is empty")
	}
}

func TestObjectWriteGuards(t *testing.T) {
	m := NewModel(NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	ctx := context.Background()
	b := &Object{Key: strPtr("bucket"), Ot: Bucket}

	// All of these must fail before any network call.
	if err := m.PutObjectMeta(ctx, nil, "k", ObjectMeta{}); err == nil {
		t.Error("nil bucket: want error")
	}
	if err := m.PutObjectMeta(ctx, b, "dir/", ObjectMeta{}); err == nil {
		t.Error("prefix key: want error")
	}
	if err := m.SetStorageClass(ctx, b, "k", ""); err == nil {
		t.Error("empty class: want error")
	}
	if err := m.SetStorageClass(ctx, b, "dir/", "GLACIER"); err == nil {
		t.Error("prefix key: want error")
	}
	if err := m.RestoreObject(ctx, b, "k", 0, ""); err == nil {
		t.Error("zero days: want error")
	}
	if err := m.RestoreObject(ctx, b, "k", -1, ""); err == nil {
		t.Error("negative days: want error")
	}
	if _, err := m.HeadObject(ctx, b, ""); err == nil {
		t.Error("empty key: want error")
	}
	if _, err := m.ObjectTags(ctx, nil, "k"); err == nil {
		t.Error("nil bucket: want error")
	}
}

func TestHeadObjectErrorMentionsBucket(t *testing.T) {
	m := NewModel(NewConfig("https://s3.example.com", nil, "ak", "sk", "", true, 0))
	_, err := m.HeadObject(context.Background(), nil, "k")
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Errorf("err = %v, want it to mention the bucket", err)
	}
}
