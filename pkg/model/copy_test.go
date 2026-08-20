package model

import "testing"

func TestCopyPartSize(t *testing.T) {
	restoreCopyKnobs(t)

	MultipartCopyPartSize = 512 * 1024 * 1024

	// Ordinary sizes keep the configured part size.
	if got := copyPartSize(10 << 30); got != 512<<20 {
		t.Errorf("copyPartSize(10 GiB) = %d, want the configured 512 MiB", got)
	}

	// A source big enough to exceed 10,000 parts grows the part size instead
	// of producing a plan S3 would reject.
	huge := int64(9 << 40) // 9 TiB
	got := copyPartSize(huge)
	if parts := (huge + got - 1) / got; parts > copyMaxParts {
		t.Errorf("part size %d yields %d parts, over the %d limit", got, parts, copyMaxParts)
	}
	if got%(1<<20) != 0 {
		t.Errorf("grown part size %d is not a whole number of MiB", got)
	}

	// A configured size below the S3 minimum is raised to it: every part but
	// the last must be at least 5 MiB.
	MultipartCopyPartSize = 1024
	if got := copyPartSize(100 << 20); got != copyPartMinSize {
		t.Errorf("copyPartSize with a 1 KiB configured part = %d, want the 5 MiB minimum", got)
	}
}

func TestPlanCopyParts(t *testing.T) {
	restoreCopyKnobs(t)
	MultipartCopyPartSize = copyPartMinSize // 5 MiB

	t.Run("ranges are contiguous, numbered from one, and cover the source", func(t *testing.T) {
		const size = 5*copyPartMinSize + 123
		parts := planCopyParts(size)
		if len(parts) != 6 {
			t.Fatalf("got %d parts, want 6", len(parts))
		}
		var next int64
		for i, p := range parts {
			if p.Number != int32(i+1) {
				t.Errorf("part %d numbered %d", i, p.Number)
			}
			if p.Start != next {
				t.Errorf("part %d starts at %d, want %d (a gap or overlap)", p.Number, p.Start, next)
			}
			if p.End < p.Start {
				t.Errorf("part %d is empty: %d-%d", p.Number, p.Start, p.End)
			}
			next = p.End + 1
		}
		if next != size {
			t.Errorf("parts cover %d bytes, want %d", next, size)
		}
		// Only the last part may be under the S3 minimum.
		last := parts[len(parts)-1]
		if got := last.End - last.Start + 1; got != 123 {
			t.Errorf("last part is %d bytes, want the 123-byte remainder", got)
		}
		for _, p := range parts[:len(parts)-1] {
			if got := p.End - p.Start + 1; got < copyPartMinSize {
				t.Errorf("part %d is %d bytes, under the 5 MiB minimum", p.Number, got)
			}
		}
	})

	t.Run("an exact multiple leaves no trailing empty part", func(t *testing.T) {
		parts := planCopyParts(3 * copyPartMinSize)
		if len(parts) != 3 {
			t.Fatalf("got %d parts, want 3", len(parts))
		}
		if last := parts[2]; last.End != 3*copyPartMinSize-1 {
			t.Errorf("last part ends at %d, want %d", last.End, 3*copyPartMinSize-1)
		}
	})

	t.Run("nothing to plan", func(t *testing.T) {
		if parts := planCopyParts(0); parts != nil {
			t.Errorf("planCopyParts(0) = %v, want nil", parts)
		}
		if parts := planCopyParts(-1); parts != nil {
			t.Errorf("planCopyParts(-1) = %v, want nil", parts)
		}
	})
}

func TestDestinationClass(t *testing.T) {
	// An explicit class wins; otherwise a replace-copy's metadata supplies it;
	// otherwise nothing is set, which matches what a plain CopyObject does.
	if got := (copySpec{storageClass: "GLACIER"}).destinationClass(); got != "GLACIER" {
		t.Errorf("explicit class = %q", got)
	}
	if got := (copySpec{meta: &ObjectMeta{StorageClass: "STANDARD_IA"}}).destinationClass(); got != "STANDARD_IA" {
		t.Errorf("class from meta = %q", got)
	}
	if got := (copySpec{storageClass: "GLACIER", meta: &ObjectMeta{StorageClass: "STANDARD_IA"}}).destinationClass(); got != "GLACIER" {
		t.Errorf("explicit class must win, got %q", got)
	}
	if got := (copySpec{}).destinationClass(); got != "" {
		t.Errorf("plain copy class = %q, want empty", got)
	}
}

func TestCopySourceValue(t *testing.T) {
	plain := copySpec{srcBucket: "b", srcKey: "dir/a.txt"}
	if got, want := plain.copySourceValue(), copySource("b", "dir/a.txt"); got != want {
		t.Errorf("copySourceValue() = %q, want %q", got, want)
	}
	versioned := copySpec{srcBucket: "b", srcKey: "dir/a.txt", srcVersion: "v1"}
	if got, want := versioned.copySourceValue(), versionCopySource("b", "dir/a.txt", "v1"); got != want {
		t.Errorf("versioned copySourceValue() = %q, want %q", got, want)
	}
}

// restoreCopyKnobs puts the package-level copy tuning back when the test ends,
// so a test that moves it can't leak the setting into the next one. It
// registers a Cleanup rather than returning a closure: `defer restore(t)` on a
// closure-returning helper defers the wrong call and silently restores
// nothing.
func restoreCopyKnobs(t *testing.T) {
	t.Helper()
	threshold, part := MultipartCopyThreshold, MultipartCopyPartSize
	t.Cleanup(func() {
		MultipartCopyThreshold, MultipartCopyPartSize = threshold, part
	})
}
