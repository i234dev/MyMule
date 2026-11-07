//go:build unix

package share

import "syscall"

// noFollow makes opening a symbolic link fail instead of following it.
const noFollow = syscall.O_NOFOLLOW
