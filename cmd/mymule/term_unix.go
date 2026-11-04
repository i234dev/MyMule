//go:build linux || darwin || freebsd || netbsd || openbsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

type winsize struct{ Row, Col, X, Y uint16 }

func ioctl(fd, req uintptr, arg unsafe.Pointer) bool {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	return e == 0
}

// termSize returns the terminal size.
func termSize() (w, h int) {
	var ws winsize
	if !ioctl(os.Stdout.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) || ws.Col == 0 {
		return 100, 30
	}
	return int(ws.Col), int(ws.Row)
}

// termSetup turns off line buffering and echo on stdin so single key
// presses (q) can be read; Ctrl+C keeps working. ok is false when stdout
// is not a terminal.
func termSetup() (restore func(), ok bool) {
	var ws winsize
	if !ioctl(os.Stdout.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) {
		return nil, false
	}
	var old syscall.Termios
	if !ioctl(os.Stdin.Fd(), ioctlGetTermios, unsafe.Pointer(&old)) {
		return func() {}, true
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	ioctl(os.Stdin.Fd(), ioctlSetTermios, unsafe.Pointer(&raw))
	return func() { ioctl(os.Stdin.Fd(), ioctlSetTermios, unsafe.Pointer(&old)) }, true
}
