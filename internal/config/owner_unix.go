//go:build unix

package config

import (
	"os"
	"syscall"
)

// ownedByMe reports whether this process's user owns the file.
func ownedByMe(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Getuid()
}
