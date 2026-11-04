//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleOutputCP         = kernel32.NewProc("GetConsoleOutputCP")
)

const (
	enableProcessedOutput           = 0x0001
	enableVirtualTerminalProcessing = 0x0004
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
)

func getMode(f *os.File) (uint32, bool) {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return mode, r != 0
}

func setMode(f *os.File, mode uint32) bool {
	r, _, _ := procSetConsoleMode.Call(f.Fd(), uintptr(mode))
	return r != 0
}

// termSetup prepares the console for a full-screen UI: ANSI escape
// sequences, UTF-8 output and unbuffered key reads. It returns a restore
// function, or ok=false if stdout is not a console.
func termSetup() (restore func(), ok bool) {
	outMode, ok := getMode(os.Stdout)
	if !ok {
		return nil, false
	}
	if !setMode(os.Stdout, outMode|enableProcessedOutput|enableVirtualTerminalProcessing) {
		return nil, false
	}
	cp, _, _ := procGetConsoleOutputCP.Call()
	procSetConsoleOutputCP.Call(65001)

	inMode, inOK := getMode(os.Stdin)
	if inOK {
		// Keep processed input so Ctrl+C still works.
		setMode(os.Stdin, inMode&^(enableLineInput|enableEchoInput))
	}
	return func() {
		setMode(os.Stdout, outMode)
		procSetConsoleOutputCP.Call(cp)
		if inOK {
			setMode(os.Stdin, inMode)
		}
	}, true
}

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// termSize returns the visible console size.
func termSize() (w, h int) {
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 100, 30
	}
	return int(info.Window.Right-info.Window.Left) + 1, int(info.Window.Bottom-info.Window.Top) + 1
}
