package netutil

import (
	"testing"
	"time"
)

// Stop ends the waits at once.
func TestLimiterStop(t *testing.T) {
	l := NewLimiter(1000)
	l.Wait(1000) // the bucket empty
	done := make(chan struct{})
	go func() { l.Wait(1000); close(done) }()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	l.Stop()
	<-done
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("Wait went on after Stop")
	}
	if w := l.Wait(1 << 20); w != 0 {
		t.Fatalf("waited %v after Stop", w)
	}
}
