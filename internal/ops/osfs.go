package ops

import "os"

// OSFS is the real filesystem. The directory mode matches what the download
// path used before this moved: 0760, owner-and-group, since a download
// directory is not something to hand to the world.
type OSFS struct{}

func (OSFS) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (OSFS) MkdirAll(path string) error { return os.MkdirAll(path, 0760) }
