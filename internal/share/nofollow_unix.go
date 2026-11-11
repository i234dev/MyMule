//go:build unix

package share

import "syscall"

// noFollow makes opening a symbolic link fail instead of following it.
const noFollow = syscall.O_NOFOLLOW

// nonBlock keeps opening a FIFO from waiting for a writer (regular files
// ignore it).
const nonBlock = syscall.O_NONBLOCK
