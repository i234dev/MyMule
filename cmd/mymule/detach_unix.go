//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in its own session so it survives the terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
