package download

import (
	"net"
	"testing"
	"time"

	"mymule/internal/peer"
)

// A connection that arrives once shutdown has closed the others is closed
// at once, so it cannot keep shutdown waiting for its time limits.
func TestConnectionDuringShutdownClosed(t *testing.T) {
	m := NewManager(Config{TempDir: t.TempDir(), IncomingDir: t.TempDir()})
	m.shutdown()
	a, b := net.Pipe()
	defer b.Close()
	c := peer.NewConn(a)
	m.trackConn(c, true)
	defer m.trackConn(c, false)
	if _, err := c.Read(time.Second); err == nil {
		t.Fatal("read on a connection that should be closed")
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("connection still open: %v", err)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
