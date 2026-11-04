//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package main

func termSize() (w, h int) { return 100, 30 }

// termSetup: assume an ANSI terminal without raw key input.
func termSetup() (restore func(), ok bool) { return func() {}, true }
