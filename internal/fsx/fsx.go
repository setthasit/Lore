package fsx

import (
	"io/fs"
	"os"
	"path/filepath"
)

// WriteAtomic stages beside the destination rather than in the temp directory:
// a rename across filesystems is not atomic.
func WriteAtomic(path string, body []byte, mode fs.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.partial")
	if err != nil {
		return err
	}
	name := staged.Name()

	_, err = staged.Write(body)
	if err == nil {
		err = staged.Chmod(mode)
	}
	if closed := staged.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// ModeOf reports the permissions of an existing file, so replacing it through
// WriteAtomic — which always creates a new inode — does not reset them.
func ModeOf(path string, fallback fs.FileMode) fs.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return fallback
	}
	return info.Mode().Perm()
}
