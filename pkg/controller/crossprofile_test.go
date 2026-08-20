package controller

import "testing"

func TestCrossDstKey(t *testing.T) {
	cases := []struct {
		srcPrefix, dstPrefix, key, want string
	}{
		// A file at the source root of the copy.
		{"backups/", "restore/", "backups/a.txt", "restore/a.txt"},
		// A folder's objects keep the folder name under the destination.
		{"backups/", "restore/", "backups/photos/2024/img.jpg", "restore/photos/2024/img.jpg"},
		// Copying from the bucket root.
		{"", "mirror/", "a/b.txt", "mirror/a/b.txt"},
		// Into the destination bucket root.
		{"deep/path/", "", "deep/path/x.bin", "x.bin"},
		// A key outside the source prefix passes through un-stripped — the
		// planner never produces this, but the function must not corrupt it.
		{"other/", "dst/", "elsewhere/f", "dst/elsewhere/f"},
	}
	for _, c := range cases {
		if got := crossDstKey(c.srcPrefix, c.dstPrefix, c.key); got != c.want {
			t.Errorf("crossDstKey(%q, %q, %q) = %q, want %q", c.srcPrefix, c.dstPrefix, c.key, got, c.want)
		}
	}
}
