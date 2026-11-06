package safe

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCatchRecoversAndLogs(t *testing.T) {
	var logged []string
	log := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	var mu sync.Mutex
	if !Catch(log, "test packet", func() { panic("boom") }, &mu) {
		t.Fatal("panic not reported")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "test packet") || !strings.Contains(logged[0], "boom") {
		t.Fatalf("log: %q", logged)
	}
	if Catch(log, "fine", func() {}) {
		t.Fatal("no panic reported as one")
	}
}

func TestRecoverInGoroutine(t *testing.T) {
	done := make(chan bool)
	go func() {
		defer func() { done <- true }()
		defer Recover(nil, "goroutine")
		var m map[string]int
		m["x"] = 1 // nil map: panics
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not finish")
	}
}

// A lock left held by the panicking code would freeze MyMule: exit instead.
func TestStuckLockExits(t *testing.T) {
	oldExit, oldWait := exit, lockWait
	defer func() { exit, lockWait = oldExit, oldWait }()
	lockWait = 100 * time.Millisecond
	code := -1
	exit = func(c int) { code = c }

	var held, free sync.Mutex
	Catch(nil, "with a free lock", func() { panic("x") }, &free)
	if code != -1 {
		t.Fatal("exited although the lock was free")
	}
	held.Lock()
	Catch(nil, "with a held lock", func() { panic("x") }, &held)
	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
}
