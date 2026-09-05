package model

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ChecksumInfo is what the server can tell us about an object's integrity.
type ChecksumInfo struct {
	// Algorithm is "CRC32C", "SHA256", … or "" when the object carries no
	// additional checksum.
	Algorithm string
	// Value is the base64 checksum as S3 reports it.
	Value string
	// Composite marks a checksum-of-checksums (a multipart object's, ending
	// in "-N"), which cannot be compared against a whole-file hash.
	Composite bool
	// ETag is the object's ETag with quotes stripped.
	ETag string
	// SinglePartETag reports that the ETag is a plain MD5 of the body — true
	// when it carries no "-N" part-count suffix.
	SinglePartETag bool
	// Size is the object's size as the server reports it.
	Size int64
}

// Verifiable reports whether anything here can be compared against a local
// file's contents.
func (ci ChecksumInfo) Verifiable() bool {
	return (ci.Algorithm != "" && !ci.Composite) || ci.SinglePartETag
}

// ObjectChecksum reads an object's checksum, ETag and size in one call.
//
// GetObjectAttributes is the only API that reports a stored checksum;
// HeadObject returns one only when asked with ChecksumMode and even then not
// on every backend. A backend that does not implement it at all yields a
// zero ChecksumInfo and no error, so callers treat it as "nothing to compare"
// rather than as a failure.
func (m *Model) ObjectChecksum(ctx context.Context, bucket *Object, key string) (ChecksumInfo, error) {
	if bucket == nil || bucket.Key == nil {
		return ChecksumInfo{}, fmt.Errorf("bucket is nil")
	}
	out, err := m.Client.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
		ObjectAttributes: []s3t.ObjectAttributes{
			s3t.ObjectAttributesChecksum,
			s3t.ObjectAttributesEtag,
			s3t.ObjectAttributesObjectSize,
		},
	})
	if err != nil {
		// Fall back to a HEAD: every backend implements that, and its ETag is
		// still usable for single-part objects.
		meta, herr := m.HeadObject(ctx, bucket, key)
		if herr != nil {
			return ChecksumInfo{}, herr
		}
		return checksumFromETag(meta.ETag, meta.Size), nil
	}

	ci := checksumFromETag(aws.ToString(out.ETag), out.ObjectSize)
	if out.Checksum != nil {
		ci.Algorithm, ci.Value = pickChecksum(out.Checksum)
		ci.Composite = strings.Contains(ci.Value, "-")
	}
	return ci, nil
}

// checksumFromETag builds the ETag-only half of a ChecksumInfo.
func checksumFromETag(etag string, size int64) ChecksumInfo {
	e := strings.Trim(etag, "\"")
	return ChecksumInfo{
		ETag: e,
		// A multipart ETag carries "-<partcount>"; anything else is the MD5
		// of the body.
		SinglePartETag: e != "" && !strings.Contains(e, "-"),
		Size:           size,
	}
}

// pickChecksum reads whichever algorithm the object actually carries.
func pickChecksum(c *s3t.Checksum) (algo, value string) {
	switch {
	case aws.ToString(c.ChecksumCRC32C) != "":
		return "CRC32C", aws.ToString(c.ChecksumCRC32C)
	case aws.ToString(c.ChecksumCRC32) != "":
		return "CRC32", aws.ToString(c.ChecksumCRC32)
	case aws.ToString(c.ChecksumSHA256) != "":
		return "SHA256", aws.ToString(c.ChecksumSHA256)
	case aws.ToString(c.ChecksumSHA1) != "":
		return "SHA1", aws.ToString(c.ChecksumSHA1)
	}
	return "", ""
}

// newHash builds the hasher for an S3 checksum algorithm name. CRC32C uses
// the Castagnoli polynomial, which is the whole difference from CRC32 and the
// easiest thing to get wrong.
func newHash(algo string) (hash.Hash, error) {
	switch strings.ToUpper(algo) {
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), nil
	case "CRC32":
		return crc32.NewIEEE(), nil
	case "SHA256":
		return sha256.New(), nil
	case "SHA1":
		return sha1.New(), nil
	case "MD5":
		return md5.New(), nil
	}
	return nil, fmt.Errorf("unsupported checksum algorithm %q", algo)
}

// LocalChecksum computes a file's checksum in the encoding S3 uses for that
// algorithm: base64 for the additional checksums, hex for MD5 (which is only
// ever compared against an ETag).
func LocalChecksum(path, algo string) (string, error) {
	h, err := newHash(algo)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := h.Sum(nil)
	if strings.EqualFold(algo, "MD5") {
		return hex.EncodeToString(sum), nil
	}
	return base64.StdEncoding.EncodeToString(sum), nil
}

// VerifyResult is the outcome of comparing a local file with an object.
type VerifyResult struct {
	// Verified is true only when a comparison actually happened and matched.
	Verified bool
	// Algorithm names what was compared ("CRC32C", "MD5 (ETag)", …).
	Algorithm string
	// Reason explains a result that is neither a match nor a mismatch — a
	// multipart object with no whole-object checksum, or a backend that
	// reports neither.
	Reason string
	// Mismatch is set when both sides produced a value and they differ. This
	// is the case worth shouting about: the transfer completed and the bytes
	// are wrong.
	Mismatch bool
	// Want / Got are the two values, for the report.
	Want, Got string
}

// String renders the result for a report line.
func (v VerifyResult) String() string {
	switch {
	case v.Mismatch:
		return fmt.Sprintf("MISMATCH (%s): object %s, local %s", v.Algorithm, v.Want, v.Got)
	case v.Verified:
		return fmt.Sprintf("verified (%s)", v.Algorithm)
	default:
		return "not verified: " + v.Reason
	}
}

// VerifyLocalFile compares a local file against an object's stored integrity
// data, preferring a whole-object additional checksum and falling back to the
// ETag for single-part objects.
//
// A multipart object with no additional checksum is reported honestly as
// unverifiable rather than being compared against a hash that could never
// match: that false alarm is worse than no answer.
func (m *Model) VerifyLocalFile(ctx context.Context, bucket *Object, key, localPath string) (VerifyResult, error) {
	ci, err := m.ObjectChecksum(ctx, bucket, key)
	if err != nil {
		return VerifyResult{}, err
	}

	st, err := os.Stat(localPath)
	if err != nil {
		return VerifyResult{}, err
	}
	// Size is the cheapest possible mismatch and worth reporting on its own:
	// the checksum would disagree anyway, but "1.2 GiB vs 900 MiB" says more.
	if ci.Size > 0 && st.Size() != ci.Size {
		return VerifyResult{
			Mismatch:  true,
			Algorithm: "size",
			Want:      fmt.Sprintf("%d bytes", ci.Size),
			Got:       fmt.Sprintf("%d bytes", st.Size()),
		}, nil
	}

	switch {
	case ci.Algorithm != "" && !ci.Composite:
		got, err := LocalChecksum(localPath, ci.Algorithm)
		if err != nil {
			return VerifyResult{}, err
		}
		return VerifyResult{
			Verified:  got == ci.Value,
			Mismatch:  got != ci.Value,
			Algorithm: ci.Algorithm,
			Want:      ci.Value,
			Got:       got,
		}, nil
	case ci.SinglePartETag:
		got, err := LocalChecksum(localPath, "MD5")
		if err != nil {
			return VerifyResult{}, err
		}
		return VerifyResult{
			Verified:  got == ci.ETag,
			Mismatch:  got != ci.ETag,
			Algorithm: "MD5 (ETag)",
			Want:      ci.ETag,
			Got:       got,
		}, nil
	case ci.Composite:
		return VerifyResult{Reason: fmt.Sprintf(
			"the object's %s checksum is a checksum-of-parts, which no whole-file hash can match; size matched",
			ci.Algorithm)}, nil
	default:
		return VerifyResult{Reason: "the object is multipart and carries no whole-object checksum (upload with a checksum algorithm to make it verifiable); size matched"}, nil
	}
}

// RemoteChecksums resolves checksums for a batch of keys, for the sync
// content-compare mode. Keys whose checksum cannot be established are absent
// from the result rather than present with an empty value, so the caller can
// tell "no answer" from "the empty answer".
func (m *Model) RemoteChecksums(ctx context.Context, bucket *Object, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		ci, err := m.ObjectChecksum(ctx, bucket, k)
		if err != nil {
			continue // a per-object failure degrades to size+mtime for that key
		}
		switch {
		case ci.Algorithm != "" && !ci.Composite:
			out[k] = ci.Algorithm + ":" + ci.Value
		case ci.SinglePartETag:
			out[k] = "MD5:" + ci.ETag
		}
	}
	return out, nil
}
