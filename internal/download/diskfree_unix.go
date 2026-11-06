//go:build linux || darwin || freebsd

package download

import "syscall"

// freeSpace returns the bytes available to us in the file system of dir.
func freeSpace(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
