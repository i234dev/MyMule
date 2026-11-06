// Package fsutil writes MyMule's own files safely.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFile replaces path with data, atomically: readers see the old file
// or the new one, never a half-written one, also after a crash or a power
// cut (the data is flushed to disk before the rename).
//
// The temporary file gets a random name and is created exclusively, so a
// file or symbolic link planted in the folder under a predictable name
// (path + ".tmp") cannot redirect the write elsewhere; and renaming over
// path replaces a link there instead of writing through it.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(perm); err != nil && !isUnsupported(err) {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
