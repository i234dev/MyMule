//go:build !windows

package fsutil

func isUnsupported(error) bool { return false }
