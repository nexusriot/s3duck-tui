package model

import (
	"mime"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// mimeTable is consulted before Go's mime.TypeByExtension because that
// function reads the system's /etc/mime.types: absent in a scratch container
// and different between distributions, which would make the type an object
// gets depend on the machine that uploaded it.
var mimeTable = map[string]string{
	// Web
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".map":  "application/json",
	".wasm": "application/wasm",
	// Data / config
	".json":    "application/json",
	".jsonl":   "application/x-ndjson",
	".ndjson":  "application/x-ndjson",
	".xml":     "application/xml",
	".yaml":    "application/yaml",
	".yml":     "application/yaml",
	".toml":    "application/toml",
	".csv":     "text/csv; charset=utf-8",
	".tsv":     "text/tab-separated-values; charset=utf-8",
	".parquet": "application/vnd.apache.parquet",
	".sql":     "application/sql",
	// Text / docs
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".log":  "text/plain; charset=utf-8",
	".conf": "text/plain; charset=utf-8",
	".ini":  "text/plain; charset=utf-8",
	".sh":   "text/x-shellscript",
	".py":   "text/x-python",
	".go":   "text/x-go",
	".rs":   "text/x-rust",
	".c":    "text/x-c",
	".h":    "text/x-c",
	".pdf":  "application/pdf",
	// Images
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".ico":  "image/vnd.microsoft.icon",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".avif": "image/avif",
	".heic": "image/heic",
	// Fonts
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	// Audio / video
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	// Archives
	".zip": "application/zip",
	".gz":  "application/gzip",
	".tgz": "application/gzip",
	".bz2": "application/x-bzip2",
	".xz":  "application/x-xz",
	".zst": "application/zstd",
	".tar": "application/x-tar",
	".7z":  "application/x-7z-compressed",
	".rar": "application/vnd.rar",
	".deb": "application/vnd.debian.binary-package",
	".rpm": "application/x-rpm",
	// Certificates / keys
	".pem": "application/x-pem-file",
	".crt": "application/x-x509-ca-cert",
}

// MimeForName maps a file name (or S3 key) onto a Content-Type by extension.
// It returns "" when the extension is unknown, leaving the decision to the
// caller — S3's own default is octet-stream, which is the honest answer for a
// file we know nothing about.
func MimeForName(name string) string {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/")))
	if ext == "" {
		return ""
	}
	if t, ok := mimeTable[ext]; ok {
		return t
	}
	// Fall back to the system table for the long tail (.docx, .apk, …). Any
	// answer from here is still deterministic per machine and better than
	// octet-stream.
	return mime.TypeByExtension(ext)
}

// sniffLen is what http.DetectContentType actually reads.
const sniffLen = 512

// MimeForFile is MimeForName with a content sniff for extensionless files —
// README, Makefile, Dockerfile and friends, which are the common case in the
// buckets people browse with a TUI. Sniffing is only ever a fallback: an
// extension is a stronger signal than the first 512 bytes (a .csv sniffs as
// text/plain, a .svg as text/xml).
//
// A sniff that lands on octet-stream is reported as "" rather than being sent:
// saying nothing lets the server apply its own default, which keeps a
// re-upload of the same file from *changing* an object's type.
func MimeForFile(localPath string) string {
	if t := MimeForName(localPath); t != "" {
		return t
	}
	f, err := os.Open(localPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, sniffLen)
	n, _ := f.Read(buf) // a short read is fine; DetectContentType handles it
	if n == 0 {
		return ""
	}
	t := http.DetectContentType(buf[:n])
	if t == "application/octet-stream" {
		return ""
	}
	return t
}

// WriteOptions are the per-profile decisions applied to every object this app
// creates: what Content-Type to claim, whether to ask the server to encrypt at
// rest, and which checksum algorithm to have it verify. They live on the Model
// (built once from the profile) rather than being threaded through every call
// site, because "how this profile writes objects" is a property of the
// connection, not of the individual transfer.
type WriteOptions struct {
	// DetectMime turns Content-Type derivation on. Default true; a profile can
	// switch it off for a bucket whose types are managed elsewhere.
	DetectMime bool
	// SSE is the ServerSideEncryption header ("AES256" or "aws:kms"). Empty
	// leaves the bucket default in force.
	SSE string
	// SSEKMSKeyID names the CMK for SSE=aws:kms. Empty uses the account's
	// default S3 key.
	SSEKMSKeyID string
	// ChecksumAlgorithm asks S3 to verify an additional checksum on write
	// ("CRC32", "CRC32C", "SHA1", "SHA256"). Empty sends none.
	ChecksumAlgorithm string
}

// DefaultWriteOptions is what a profile that says nothing gets: derive the
// content type, encrypt as the bucket says, no extra checksum.
func DefaultWriteOptions() WriteOptions {
	return WriteOptions{DetectMime: true}
}

// SSEOptions lists the values the profile form offers.
func SSEOptions() []string { return []string{"", "AES256", "aws:kms"} }

// ChecksumOptions lists the values the profile form offers. CRC32C is the
// cheapest to compute and the one AWS itself defaults to.
func ChecksumOptions() []string { return []string{"", "CRC32C", "CRC32", "SHA256", "SHA1"} }

// ValidChecksum reports whether s is one of ChecksumOptions.
func ValidChecksum(s string) bool {
	for _, c := range ChecksumOptions() {
		if c == s {
			return true
		}
	}
	return false
}

// writeOpts returns the model's write options, tolerating a Model built
// before the field existed (tests, zero values) by defaulting mime detection
// on — the behaviour the app should have had from the start.
func (m *Model) writeOpts() WriteOptions {
	if m == nil || m.Cf == nil {
		return DefaultWriteOptions()
	}
	return m.Cf.Write
}

// applyPut stamps a PutObjectInput with the profile's write options.
// contentTypeFor, when non-empty, is the name to derive Content-Type from; an
// input that already carries a type (a copy that inherited one, an edit that
// preserves one) is never overwritten.
func (w WriteOptions) applyPut(in *s3.PutObjectInput, contentTypeFor string) {
	if in == nil {
		return
	}
	if w.DetectMime && aws.ToString(in.ContentType) == "" && contentTypeFor != "" {
		if t := MimeForFile(contentTypeFor); t != "" {
			in.ContentType = aws.String(t)
		}
	}
	if w.SSE != "" {
		in.ServerSideEncryption = s3t.ServerSideEncryption(w.SSE)
		if w.SSEKMSKeyID != "" {
			in.SSEKMSKeyId = aws.String(w.SSEKMSKeyID)
		}
	}
	if w.ChecksumAlgorithm != "" {
		in.ChecksumAlgorithm = s3t.ChecksumAlgorithm(w.ChecksumAlgorithm)
	}
}

// applyCreateMultipart is applyPut for the multipart-copy path, which starts a
// bare object and so needs the same encryption and checksum decisions.
func (w WriteOptions) applyCreateMultipart(in *s3.CreateMultipartUploadInput) {
	if in == nil {
		return
	}
	if w.SSE != "" {
		in.ServerSideEncryption = s3t.ServerSideEncryption(w.SSE)
		if w.SSEKMSKeyID != "" {
			in.SSEKMSKeyId = aws.String(w.SSEKMSKeyID)
		}
	}
	if w.ChecksumAlgorithm != "" {
		in.ChecksumAlgorithm = s3t.ChecksumAlgorithm(w.ChecksumAlgorithm)
	}
}

// summary renders the options for the profile details pane.
func (w WriteOptions) summary() string {
	var parts []string
	if w.DetectMime {
		parts = append(parts, "mime:auto")
	}
	if w.SSE != "" {
		s := "sse:" + w.SSE
		if w.SSEKMSKeyID != "" {
			s += "(kms key set)"
		}
		parts = append(parts, s)
	}
	if w.ChecksumAlgorithm != "" {
		parts = append(parts, "checksum:"+w.ChecksumAlgorithm)
	}
	if len(parts) == 0 {
		return "defaults"
	}
	return strings.Join(parts, ", ")
}

// WriteSummary exposes summary for the UI.
func (w WriteOptions) WriteSummary() string { return w.summary() }
