//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes cmd run without a console and survive the parent terminal.
func detach(cmd *exec.Cmd) {
	const detachedProcess = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
