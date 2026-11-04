//go:build !unix && !windows

package config

import "os"

// lockFile is best effort on platforms without flock or Windows handles.
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
}
