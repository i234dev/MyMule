package download

import (
	"errors"
	"fmt"
	"os"
)

// errNotRegular is returned for download files that are links or other
// non-regular files.
var errNotRegular = errors.New("not a regular file (a link?); refusing to use it")

// openRegular opens a download's own file (state or data) only if it is a
// plain file, never through a symbolic or hard link. Someone who can write
// in the temporary folder could otherwise plant a link under a download's
// name and make MyMule write downloaded data into, or serve to the
// network, any file MyMule can reach (its settings, the user's files).
// With create, a missing file is created.
func openRegular(path string, flag int, create bool) (*os.File, error) {
	before, err := os.Lstat(path)
	switch {
	case err == nil:
		if !before.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: %w", path, errNotRegular)
		}
	case os.IsNotExist(err) && create:
		flag |= os.O_CREATE | os.O_EXCL
		before = nil
	default:
		return nil, err
	}
	f, err := os.OpenFile(path, flag|noFollow, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// The file opened must be the one checked (not swapped in between),
	// and have no other names.
	if !st.Mode().IsRegular() || (before != nil && !os.SameFile(before, st)) || extraLinks(st) {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	return f, nil
}

// readRegular reads a download's state file under the same rules.
func readRegular(path string) ([]byte, error) {
	f, err := openRegular(path, os.O_RDONLY, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > maxStateSize {
		return nil, fmt.Errorf("%s: state file too large", path)
	}
	data := make([]byte, st.Size())
	_, err = f.ReadAt(data, 0)
	return data, err
}

// maxStateSize bounds a .part.json (a 256 GB file's state is well below).
const maxStateSize = 16 << 20
