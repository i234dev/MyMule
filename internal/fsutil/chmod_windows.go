//go:build windows

package fsutil

// On Windows Chmod only toggles read-only; failures there are not fatal.
func isUnsupported(error) bool { return true }
