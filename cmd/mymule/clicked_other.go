//go:build !windows

package main

import "errors"

// startedByDoubleClick: only Windows starts console programs from a file
// manager in a window of their own.
func startedByDoubleClick() bool { return false }

func openBrowser(string) error { return errors.New("not supported") }
