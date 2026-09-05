package model

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WalkDir lists one directory as Objects, directories first, each carrying its
// absolute path as FullPath — the identity objKey uses for selection, so a
// local pane's marks are scoped and survive navigation like a remote pane's.
//
// Unreadable entries are skipped rather than failing the listing: one
// permission-denied file in /etc should not make the directory unbrowsable.
// Symlinks are reported by what they point at (Stat, not Lstat) so a symlinked
// directory can be entered, matching what the sync walker does with its root.
func WalkDir(dir string) ([]*Object, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]*Object, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil {
			continue // vanished or unreadable: not worth failing the listing
		}
		n := name
		if info.IsDir() {
			p := full + string(filepath.Separator)
			out = append(out, &Object{Key: &n, Ot: Folder, FullPath: &p})
			continue
		}
		if !info.Mode().IsRegular() {
			continue // sockets, devices and fifos are not transferable
		}
		size := info.Size()
		mod := info.ModTime()
		p := full
		out = append(out, &Object{
			Key:          &n,
			Ot:           File,
			Size:         &size,
			LastModified: &mod,
			FullPath:     &p,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ot != out[j].Ot {
			return out[i].Ot > out[j].Ot // folders first, as in a remote listing
		}
		return *out[i].Key < *out[j].Key
	})
	return out, nil
}

// ParentDir returns dir's parent, or "" when dir is already the root. Used by
// the local pane's ".." row.
func ParentDir(dir string) string {
	clean := filepath.Clean(dir)
	parent := filepath.Dir(clean)
	if parent == clean {
		return "" // at the root: there is nowhere up to go
	}
	return parent
}

// LocalDirSize sums the regular files directly inside dir (not recursively)
// and counts them, for the pane's status line. Recursion is deliberately not
// done here: a pane listing walks one level, and a du of the whole tree is
// what the usage browser is for.
func LocalDirSize(dir string) (bytes int64, files int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		bytes += info.Size()
		files++
	}
	return bytes, files
}

// LocalTargets expands a selection of local paths into the concrete files to
// upload, paired with the key each should land on under dstPrefix. Directories
// are walked recursively and keep their own name at the destination, so
// uploading "reports/" produces "dstPrefix/reports/...".
//
// It is the local-pane counterpart of PrepareUpload, which derives keys from a
// single walked root and cannot express "these five things, from here".
func LocalTargets(paths []string, dstPrefix string) ([]UploadTarget, int64, error) {
	var out []UploadTarget
	var total int64

	for _, p := range paths {
		clean := filepath.Clean(p)
		info, err := os.Stat(clean)
		if err != nil {
			return nil, 0, err
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				continue
			}
			out = append(out, UploadTarget{
				LocalPath:  clean,
				RemotePath: dstPrefix + filepath.Base(clean),
				Size:       info.Size(),
			})
			total += info.Size()
			continue
		}

		base := filepath.Base(clean)
		err = walkFollowingRoot(clean, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(clean, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if fi.IsDir() {
				// An empty directory becomes a marker object, the same
				// convention the walk-based upload follows; a non-empty one
				// needs nothing of its own.
				if isEmptyDir(path) {
					key := dstPrefix + base + "/"
					if rel != "." {
						key = dstPrefix + base + "/" + rel + "/"
					}
					out = append(out, UploadTarget{LocalPath: path, RemotePath: key})
				}
				return nil
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
			key := dstPrefix + base + "/" + rel
			out = append(out, UploadTarget{LocalPath: path, RemotePath: key, Size: fi.Size()})
			total += fi.Size()
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("walking %s: %w", clean, err)
		}
	}
	return out, total, nil
}

// isEmptyDir reports whether a directory has no entries at all.
func isEmptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

// DisplayDir shortens a path for a pane title, replacing the home directory
// with "~" so a deep path still fits a narrow pane.
func DisplayDir(dir, home string) string {
	if home != "" && strings.HasPrefix(dir, home) {
		return "~" + strings.TrimPrefix(dir, home)
	}
	return dir
}
