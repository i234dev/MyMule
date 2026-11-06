// Package safe keeps one bad packet from taking MyMule down.
//
// Code that handles data from the network runs under Recover or Catch: a
// panic there (a bug that some input triggers) is logged with its stack
// and only ends that connection or packet. Recovering is only safe if no
// lock was left held, which would freeze MyMule instead; so after a panic
// the given locks are checked, and if one stays held MyMule exits, to be
// restarted by its service manager like after a crash.
package safe

import (
	"os"
	"runtime/debug"
	"time"
)

// TryLocker is a lock that can be tested without blocking (sync.Mutex).
type TryLocker interface {
	TryLock() bool
	Unlock()
}

// lockWait is how long a lock may stay held after a panic before MyMule
// gives up and exits. Normal holders release locks within milliseconds.
var lockWait = 5 * time.Second

// exit is os.Exit, replaced in tests.
var exit = os.Exit

// Recover is deferred at the top of a goroutine that handles network
// input. It must be the deferred call that runs first (deferred last).
func Recover(log func(string, ...any), where string, locks ...TryLocker) {
	if r := recover(); r != nil {
		report(log, where, r, locks)
	}
}

// Catch runs fn and reports whether it panicked (see Recover).
func Catch(log func(string, ...any), where string, fn func(), locks ...TryLocker) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			report(log, where, r, locks)
			panicked = true
		}
	}()
	fn()
	return false
}

func report(log func(string, ...any), where string, r any, locks []TryLocker) {
	if log != nil {
		log("BUG: %s: recovered from a panic: %v (please report it)\n%s", where, r, debug.Stack())
	}
	for _, l := range locks {
		if l == nil || freed(l) {
			continue
		}
		if log != nil {
			log("BUG: a lock stayed held after that panic; exiting so MyMule can be restarted cleanly")
		}
		exit(2)
	}
}

// freed waits up to lockWait for l to be free.
func freed(l TryLocker) bool {
	deadline := time.Now().Add(lockWait)
	for {
		if l.TryLock() {
			l.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
