package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalChecksumKnownValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "abc")
	if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	// Reference values for "abc": MD5 is the classic hex digest; the S3
	// additional checksums are base64. CRC32C must use Castagnoli — the
	// IEEE table would silently produce a wrong-but-plausible value.
	cases := map[string]string{
		"MD5":    "900150983cd24fb0d6963f7d28e17f72",
		"SHA256": "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=",
		"SHA1":   "qZk+NkcGgWq6PiVxeFDCbJzQ2J0=",
		"CRC32":  "NSRBwg==",
		"CRC32C": "Nks/tw==",
	}
	for algo, want := range cases {
		got, err := LocalChecksum(p, algo)
		if err != nil {
			t.Fatalf("%s: %v", algo, err)
		}
		if got != want {
			t.Errorf("LocalChecksum(%s) = %q, want %q", algo, got, want)
		}
	}
	if _, err := LocalChecksum(p, "SHA3"); err == nil {
		t.Error("an unknown algorithm should be refused, not silently hashed")
	}
	if _, err := LocalChecksum(filepath.Join(dir, "missing"), "MD5"); err == nil {
		t.Error("a missing file should error")
	}
}

func TestChecksumFromETag(t *testing.T) {
	single := checksumFromETag("\"900150983cd24fb0d6963f7d28e17f72\"", 3)
	if !single.SinglePartETag || single.ETag != "900150983cd24fb0d6963f7d28e17f72" {
		t.Errorf("single-part ETag misread: %+v", single)
	}
	// A multipart ETag is a hash of part hashes: not the body's MD5, so it
	// must never be offered for a content comparison.
	multi := checksumFromETag("\"d41d8cd98f00b204e9800998ecf8427e-7\"", 1)
	if multi.SinglePartETag {
		t.Error("a multipart ETag must not be treated as an MD5")
	}
	if checksumFromETag("", 0).SinglePartETag {
		t.Error("an absent ETag is not a checksum")
	}
}

func TestChecksumInfoVerifiable(t *testing.T) {
	cases := []struct {
		ci   ChecksumInfo
		want bool
	}{
		{ChecksumInfo{Algorithm: "CRC32C", Value: "x"}, true},
		{ChecksumInfo{Algorithm: "CRC32C", Value: "x-4", Composite: true}, false},
		{ChecksumInfo{SinglePartETag: true, ETag: "abc"}, true},
		{ChecksumInfo{}, false},
	}
	for _, tc := range cases {
		if got := tc.ci.Verifiable(); got != tc.want {
			t.Errorf("Verifiable(%+v) = %v, want %v", tc.ci, got, tc.want)
		}
	}
}

func TestVerifyResultString(t *testing.T) {
	if got := (VerifyResult{Verified: true, Algorithm: "CRC32C"}).String(); !strings.Contains(got, "verified (CRC32C)") {
		t.Errorf("verified string = %q", got)
	}
	got := VerifyResult{Mismatch: true, Algorithm: "MD5 (ETag)", Want: "a", Got: "b"}.String()
	if !strings.Contains(got, "MISMATCH") || !strings.Contains(got, "object a") || !strings.Contains(got, "local b") {
		t.Errorf("mismatch string = %q", got)
	}
	if got := (VerifyResult{Reason: "no checksum"}).String(); !strings.Contains(got, "not verified: no checksum") {
		t.Errorf("unverified string = %q", got)
	}
}

func TestNewHashCRC32CIsCastagnoli(t *testing.T) {
	c, err := newHash("CRC32C")
	if err != nil {
		t.Fatal(err)
	}
	ieee, err := newHash("CRC32")
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("abc"))
	ieee.Write([]byte("abc"))
	if string(c.Sum(nil)) == string(ieee.Sum(nil)) {
		t.Error("CRC32C and CRC32 must not agree — the polynomial differs")
	}
}
