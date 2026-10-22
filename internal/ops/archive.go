package ops

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// KindArchive is a download that lands as one archive file.
const KindArchive Kind = "archive"

// ArchiveFormat is the container an Archive writes.
type ArchiveFormat string

const (
	ArchiveTarGz ArchiveFormat = "tar.gz"
	ArchiveZip   ArchiveFormat = "zip"
)

// ArchiveFormats lists the formats in the order the form offers them.
func ArchiveFormats() []ArchiveFormat { return []ArchiveFormat{ArchiveTarGz, ArchiveZip} }

// Ext is the file extension the format's archives are named with.
func (f ArchiveFormat) Ext() string { return "." + string(f) }

// Opener streams one object's body. It is model.Model.OpenObject.
type Opener interface {
	OpenObject(ctx context.Context, bucket *model.Object, key string) (io.ReadCloser, model.ObjectStat, error)
}

// Archive downloads a resolved set of objects into a single .tar.gz or .zip,
// streamed: no object is staged on disk, so archiving a folder needs only the
// archive's own space, and the result is one file to move or send on.
//
// An archive is a single stream, so the objects go in one at a time — there is
// no worker pool to size. Failures split by when they happen. An object that
// cannot be opened (access denied, an archived Glacier object, gone since the
// listing) is left out and reported, and the archive is still written, minus
// it. An object that fails *mid-body* cannot be left out: its entry header is
// already written with the full size, so the archive would be corrupt from
// that point on, and the whole run is abandoned rather than leave a file that
// looks finished and is not.
type Archive struct {
	Bucket *model.Object
	// SrcPrefix is the listing prefix the targets were resolved under; entry
	// names are relative to it, as a download's local paths are.
	SrcPrefix     string
	Targets       []model.DownloadTarget
	SelectedCount int
	TotalBytes    int64
	Format        ArchiveFormat
	// Dest is the archive file's full path.
	Dest string

	Opener Opener
	FS     FS
	Gate   Gate
}

func (a *Archive) Describe() Descriptor {
	return Descriptor{
		Kind:         KindArchive,
		Title:        fmt.Sprintf("%d obj → %s", len(a.Targets), a.Dest),
		TotalBytes:   a.TotalBytes,
		TotalItems:   len(a.Targets),
		RefreshAfter: false,
		ConfirmText: fmt.Sprintf(
			"Download as one archive\n%s\nSelected: %d item(s)\nResolved: %d object(s)\nTotal size: %s (before compression)",
			a.Dest, a.SelectedCount, len(a.Targets), humanize.IBytes(uint64(a.TotalBytes))),
	}
}

// ArchiveEntryName maps a key onto its path inside the archive: relative to
// prefix, empty segments collapsed, and refused when it would climb out of the
// archive root on extraction ("..") — the zip-slip a download already refuses,
// moved to where the extracting tool would meet it. A folder marker keeps its
// trailing slash, which is how both formats spell a directory entry.
func ArchiveEntryName(prefix, key string) (string, error) {
	rel := key
	if p := model.NormalizePrefix(prefix); p != "" && strings.HasPrefix(key, p) {
		rel = strings.TrimPrefix(key, p)
	}
	dir := strings.HasSuffix(rel, "/")
	var segs []string
	for _, s := range strings.Split(rel, "/") {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("object key %q would extract outside the archive root", key)
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return "", fmt.Errorf("object key %q has no name inside the archive", key)
	}
	name := strings.Join(segs, "/")
	if dir {
		name += "/"
	}
	return name, nil
}

// storedExts are formats that are already compressed: deflating them again
// costs CPU for nothing (or a few bytes more), so a zip stores them as they
// are. Matched on the lower-cased extension.
var storedExts = map[string]bool{
	".gz": true, ".tgz": true, ".zst": true, ".bz2": true, ".xz": true, ".7z": true,
	".zip": true, ".rar": true, ".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".webp": true, ".heic": true, ".mp3": true, ".mp4": true,
	".mkv": true, ".mov": true, ".webm": true, ".ogg": true, ".pdf": true,
	".parquet": true, ".avro": true, ".jar": true, ".docx": true, ".xlsx": true,
}

// entryWriter is the part of tar and zip an Archive needs: start an entry
// with a known size and time, then take its body.
type entryWriter interface {
	dir(name string, mod time.Time) error
	file(name string, size int64, mod time.Time) (io.Writer, error)
	close() error
}

type tarGzEntries struct {
	gz *gzip.Writer
	tw *tar.Writer
}

func newTarGz(w io.Writer) *tarGzEntries {
	gz := gzip.NewWriter(w)
	return &tarGzEntries{gz: gz, tw: tar.NewWriter(gz)}
}

func (t *tarGzEntries) dir(name string, mod time.Time) error {
	return t.tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mod})
}

func (t *tarGzEntries) file(name string, size int64, mod time.Time) (io.Writer, error) {
	err := t.tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: size, ModTime: mod})
	return t.tw, err
}

func (t *tarGzEntries) close() error {
	if err := t.tw.Close(); err != nil {
		return err
	}
	return t.gz.Close()
}

type zipEntries struct{ zw *zip.Writer }

func (z *zipEntries) dir(name string, mod time.Time) error {
	h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: mod}
	h.SetMode(os.ModeDir | 0o755)
	_, err := z.zw.CreateHeader(h)
	return err
}

func (z *zipEntries) file(name string, size int64, mod time.Time) (io.Writer, error) {
	method := zip.Deflate
	if storedExts[strings.ToLower(path.Ext(name))] {
		method = zip.Store
	}
	h := &zip.FileHeader{Name: name, Method: method, Modified: mod}
	h.SetMode(0o644)
	// Declaring the size up front lets the writer choose zip64 for an entry
	// over 4 GiB before it starts, instead of discovering it too late.
	if size >= 0 {
		h.UncompressedSize64 = uint64(size)
	}
	return z.zw.CreateHeader(h)
}

func (z *zipEntries) close() error { return z.zw.Close() }

// errArchiveAbandoned marks a failure that took the whole archive with it.
var errArchiveAbandoned = errors.New("archive abandoned")

func (a *Archive) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}

	if a.FS.Exists(a.Dest) {
		switch rep.Ask(Question{Kind: QOverwrite, Path: a.Dest}) {
		case AnswerYes, AnswerYesToAll:
			// Replaced only by the final rename, once the new one is whole.
		case AnswerNo, AnswerNoToAll:
			led.skip()
			return led.result()
		default:
			led.cancel()
			return led.result()
		}
	}

	started := Reserve(ctx, a.Gate, rep, func() { a.write(ctx, rep, led) })
	if !started {
		led.cancel()
	}
	res := led.result()
	rep.Log("Archive %s: %d object(s) in, %d failed", a.Dest, res.Done, res.Failed())
	return res
}

// countingWriter tallies what passes through, for the progress gauge.
type countingWriter struct {
	w io.Writer
	n *int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

// write builds the archive in a temp file beside Dest and renames it into
// place only once every entry is in and the file is synced: a cancelled or
// abandoned run leaves nothing behind, and an existing archive of that name
// survives until its replacement is complete.
func (a *Archive) write(ctx context.Context, rep Reporter, led *ledger) {
	dir := filepath.Dir(a.Dest)
	if err := a.FS.MkdirAll(dir); err != nil {
		led.fail(a.Dest, err)
		return
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(a.Dest)+".*.s3duck-part")
	if err != nil {
		led.fail(a.Dest, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	buf := bufio.NewWriterSize(tmp, 1<<20)
	var ew entryWriter
	if a.Format == ArchiveZip {
		ew = &zipEntries{zw: zip.NewWriter(buf)}
	} else {
		ew = newTarGz(buf)
	}

	// ok holds the byte count of every entry written, in order. Entries go in
	// one at a time, so none of this is shared between goroutines.
	var ok []int64
	var moved int64
	seen := map[string]string{}
	report := func(active string) {
		p := Progress{Bytes: moved, TotalBytes: a.TotalBytes, Items: len(ok), TotalItems: len(a.Targets)}
		if active != "" {
			p.Active = []string{active}
		}
		rep.Progress(p)
	}

	for _, t := range a.Targets {
		if ctx.Err() != nil {
			led.cancel()
			return
		}
		name, err := ArchiveEntryName(a.SrcPrefix, t.Key)
		if err != nil {
			led.fail(t.Key, err)
			continue
		}
		if prev, dup := seen[name]; dup {
			// "a//b" and "a/b" are one path inside the archive; extracting
			// both would keep only the last, silently.
			led.fail(t.Key, fmt.Errorf("lands on %q inside the archive, as %q already does", name, prev))
			continue
		}
		seen[name] = t.Key

		if strings.HasSuffix(name, "/") {
			if err := ew.dir(name, time.Now()); err != nil {
				led.fail(t.Key, fmt.Errorf("%w: %v", errArchiveAbandoned, err))
				return
			}
			ok = append(ok, 0)
			continue
		}

		report(path.Base(t.Key))
		body, st, err := a.Opener.OpenObject(ctx, a.Bucket, t.Key)
		if err != nil {
			if ctx.Err() != nil {
				led.cancel()
				return
			}
			// Nothing of this entry is written yet, so leaving it out keeps
			// the archive whole.
			led.fail(t.Key, err)
			continue
		}
		mod := time.Now()
		if st.LastModified != nil {
			mod = *st.LastModified
		}
		// Whole seconds: sub-second times push tar into PAX headers for no
		// benefit, and zip's DOS time cannot hold them anyway.
		mod = mod.Truncate(time.Second)

		w, err := ew.file(name, st.Size, mod)
		if err != nil {
			body.Close()
			led.fail(t.Key, fmt.Errorf("%w: %v", errArchiveAbandoned, err))
			return
		}
		var n int64
		_, err = io.Copy(countingWriter{w: w, n: &n}, body)
		body.Close()
		moved += n
		if err == nil && n != st.Size {
			err = fmt.Errorf("read %d of %d bytes", n, st.Size)
		}
		if err != nil {
			if ctx.Err() != nil {
				led.cancel()
				return
			}
			led.fail(t.Key, fmt.Errorf("%w mid-object: %v", errArchiveAbandoned, err))
			return
		}
		ok = append(ok, n)
		report("")
	}

	if len(ok) == 0 {
		// Every object failed to open: an empty archive would only look like
		// a result. The failures say why.
		return
	}
	if err := ew.close(); err != nil {
		led.fail(a.Dest, err)
		return
	}
	if err := buf.Flush(); err != nil {
		led.fail(a.Dest, err)
		return
	}
	if err := tmp.Sync(); err != nil {
		led.fail(a.Dest, err)
		return
	}
	if err := tmp.Close(); err != nil {
		led.fail(a.Dest, err)
		return
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		led.fail(a.Dest, err)
		return
	}
	if err := os.Rename(tmp.Name(), a.Dest); err != nil {
		led.fail(a.Dest, err)
		return
	}
	committed = true
	// Counted only now: until the rename, nothing the user can open holds them.
	for _, n := range ok {
		led.ok(n, false)
	}
}
