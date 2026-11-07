package download

import (
	"context"
	"net"
	"testing"
	"time"

	"mymule/internal/peer"
	"mymule/internal/proto"
)

// Serving a peer after its download session ends is protected against
// panics like the session itself, and the source is released first.
func TestServeAfterSessionContained(t *testing.T) {
	m := NewManager(Config{Log: func(string, ...any) {}})
	f := &File{sources: map[string]*Source{}}
	src := &Source{key: "198.51.100.9:4662", file: f, busy: true}
	m.active = 1
	busyWhileServing := true
	testServeHook = func() {
		m.mu.Lock()
		busyWhileServing = src.busy
		m.mu.Unlock()
		panic("bad upload")
	}
	defer func() { testServeHook = nil }()
	a, b := net.Pipe()
	defer b.Close()
	m.afterSession(context.Background(), peer.NewConn(a), a, &sessionCleanup{src: src, serveAfter: true}, false)
	if busyWhileServing || m.active != 0 {
		t.Fatalf("source busy while serving uploads: %v (active %d)", busyWhileServing, m.active)
	}
}

// A peer we are not uploading to cannot keep its connection open for
// ever by sending a packet now and then.
func TestUnservedConnectionLimited(t *testing.T) {
	old := unservedLimit
	unservedLimit = 300 * time.Millisecond
	defer func() { unservedLimit = old }()
	m := NewManager(Config{})
	a, b := net.Pipe()
	defer b.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // a packet every 50 ms, answers discarded
		pc := peer.NewConn(b)
		go func() {
			for {
				if _, err := pc.Read(time.Second); err != nil {
					return
				}
			}
		}()
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if pc.Send(proto.ProtoEMule, peer.OpEMuleInfo, peer.EMuleInfoAnswer()) != nil {
					return
				}
			}
		}
	}()
	done := make(chan struct{})
	go func() { m.serveUploads(context.Background(), peer.NewConn(a), peer.Hello{}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection kept open by a chatty peer")
	}
}
