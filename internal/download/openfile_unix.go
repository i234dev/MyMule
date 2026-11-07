//go:build unix

package download

import (
	"os"
	"syscall"
)

// noFollow makes opening a symbolic link fail instead of following it.
const noFollow = syscall.O_NOFOLLOW

// extraLinks reports whether st has other hard links: another name for
// the same file, possibly one of MyMule's private files planted in the
// temporary folder under a download's name.
func extraLinks(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Nlink > 1
}
