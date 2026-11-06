//go:build !linux && !darwin && !freebsd && !windows

package download

// freeSpace is unknown on this system: no check is made.
func freeSpace(string) (uint64, bool) { return 0, false }
