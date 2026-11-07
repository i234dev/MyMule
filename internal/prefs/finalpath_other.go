//go:build !windows

package prefs

import "errors"

// finalPath is only needed on Windows (junctions); elsewhere
// filepath.EvalSymlinks already resolves every link.
func finalPath(string) (string, error) { return "", errors.ErrUnsupported }
