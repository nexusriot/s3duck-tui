package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestMimeForName(t *testing.T) {
	cases := map[string]string{
		"index.html":        "text/html; charset=utf-8",
		"style.CSS":         "text/css; charset=utf-8",
		"data.json":         "application/json",
		"photo.JPEG":        "image/jpeg",
		"archive.tar.gz":    "application/gzip",
		"deep/path/app.js":  "text/javascript; charset=utf-8",
		"windows\\path.png": "image/png",
	}
	for name, want := range cases {
		if got := MimeForName(name); got != want {
			t.Errorf("MimeForName(%q) = %q, want %q", name, got, want)
		}
	}
	// No extension: nothing to go on by name alone.
	if got := MimeForName("README"); got != "" {
		t.Errorf("MimeForName(README) = %q, want empty", got)
	}
	// Unknown extension falls through to the system table, which may or may
	// not know it; either way it must not panic or invent a type for junk.
	if got := MimeForName("x.zzzznotatype"); got != "" {
		t.Errorf("MimeForName(unknown) = %q, want empty", got)
	}
}

func TestMimeForFileSniffsExtensionlessFiles(t *testing.T) {
	dir := t.TempDir()

	text := filepath.Join(dir, "README")
	if err := os.WriteFile(text, []byte("# Title\n\nplain prose\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := MimeForFile(text); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("MimeForFile(README) = %q, want text/plain...", got)
	}

	// An extension always wins over a sniff: a .csv sniffs as text/plain.
	csv := filepath.Join(dir, "rows.csv")
	if err := os.WriteFile(csv, []byte("a,b\n1,2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := MimeForFile(csv); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("MimeForFile(rows.csv) = %q, want text/csv...", got)
	}

	// Unrecognisable bytes with no extension: say nothing rather than claim
	// octet-stream, so the server's default applies and a re-upload of the
	// same file never *changes* the object's type.
	bin := filepath.Join(dir, "blob")
	if err := os.WriteFile(bin, []byte{0x00, 0x01, 0x02, 0xff, 0xfe}, 0600); err != nil {
		t.Fatal(err)
	}
	if got := MimeForFile(bin); got != "" {
		t.Errorf("MimeForFile(blob) = %q, want empty", got)
	}

	// An empty file has nothing to sniff.
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := MimeForFile(empty); got != "" {
		t.Errorf("MimeForFile(empty) = %q, want empty", got)
	}

	// A missing file must not panic.
	if got := MimeForFile(filepath.Join(dir, "nope")); got != "" {
		t.Errorf("MimeForFile(missing) = %q, want empty", got)
	}
}

func TestWriteOptionsApplyPut(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "index.html")
	if err := os.WriteFile(page, []byte("<html>"), 0600); err != nil {
		t.Fatal(err)
	}

	w := WriteOptions{DetectMime: true, SSE: "aws:kms", SSEKMSKeyID: "key-1", ChecksumAlgorithm: "CRC32C"}
	in := &s3.PutObjectInput{}
	w.applyPut(in, page)

	if got := aws.ToString(in.ContentType); !strings.HasPrefix(got, "text/html") {
		t.Errorf("ContentType = %q, want text/html...", got)
	}
	if string(in.ServerSideEncryption) != "aws:kms" {
		t.Errorf("ServerSideEncryption = %q", in.ServerSideEncryption)
	}
	if aws.ToString(in.SSEKMSKeyId) != "key-1" {
		t.Errorf("SSEKMSKeyId = %q", aws.ToString(in.SSEKMSKeyId))
	}
	if string(in.ChecksumAlgorithm) != "CRC32C" {
		t.Errorf("ChecksumAlgorithm = %q", in.ChecksumAlgorithm)
	}

	// An input that already carries a type keeps it: a copy or an edit has
	// the object's real type and must not have it re-guessed from the key.
	in = &s3.PutObjectInput{ContentType: aws.String("application/x-custom")}
	w.applyPut(in, page)
	if aws.ToString(in.ContentType) != "application/x-custom" {
		t.Errorf("existing ContentType was overwritten: %q", aws.ToString(in.ContentType))
	}

	// Detection off: no type is invented, but encryption still applies.
	in = &s3.PutObjectInput{}
	WriteOptions{SSE: "AES256"}.applyPut(in, page)
	if in.ContentType != nil {
		t.Errorf("ContentType set with detection off: %q", aws.ToString(in.ContentType))
	}
	if string(in.ServerSideEncryption) != "AES256" {
		t.Errorf("ServerSideEncryption = %q", in.ServerSideEncryption)
	}

	// A folder marker has no name to derive from; encryption must still land.
	in = &s3.PutObjectInput{}
	w.applyPut(in, "")
	if in.ContentType != nil {
		t.Errorf("ContentType invented for a marker: %q", aws.ToString(in.ContentType))
	}
	if string(in.ServerSideEncryption) != "aws:kms" {
		t.Error("marker object skipped encryption")
	}

	// A nil input must be tolerated rather than panic mid-transfer.
	w.applyPut(nil, page)
}

func TestWriteOptionsApplyCreateMultipart(t *testing.T) {
	in := &s3.CreateMultipartUploadInput{}
	WriteOptions{DetectMime: true, SSE: "AES256", ChecksumAlgorithm: "SHA256"}.applyCreateMultipart(in)
	if string(in.ServerSideEncryption) != "AES256" || string(in.ChecksumAlgorithm) != "SHA256" {
		t.Errorf("multipart create missed options: %+v", in)
	}
	WriteOptions{}.applyCreateMultipart(nil)
}

func TestWriteOptsDefaultsToDetectionOn(t *testing.T) {
	// A Model built without write options (tests, older code paths) must
	// still derive content types — that is the behaviour being fixed.
	var m Model
	if !m.writeOpts().DetectMime {
		t.Error("a Model with no config should default to mime detection")
	}
	m.Cf = &Config{}
	if m.writeOpts().DetectMime {
		t.Error("an explicit config with detection off must be honoured")
	}
}

func TestValidChecksumAndSummary(t *testing.T) {
	if !ValidChecksum("CRC32C") || ValidChecksum("MD5") {
		t.Error("ValidChecksum accepted the wrong set")
	}
	if got := DefaultWriteOptions().WriteSummary(); got != "mime:auto" {
		t.Errorf("default summary = %q", got)
	}
	if got := (WriteOptions{}).WriteSummary(); got != "defaults" {
		t.Errorf("empty summary = %q", got)
	}
	got := WriteOptions{DetectMime: true, SSE: "aws:kms", SSEKMSKeyID: "k", ChecksumAlgorithm: "CRC32"}.WriteSummary()
	for _, want := range []string{"mime:auto", "sse:aws:kms", "kms key set", "checksum:CRC32"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}
