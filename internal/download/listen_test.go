package download

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"mymule/internal/server"
)

// With its port in use by another program, the manager says it is not
// listening once it tried: that port must not be opened on the router.
func TestListenedPortInUse(t *testing.T) {
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := uint16(busy.Addr().(*net.TCPAddr).Port)
	for _, tc := range []struct {
		port uint16
		want bool
	}{{port, false}, {0, false}} {
		m := NewManager(Config{Port: tc.port, ListenHost: "127.0.0.1", Nick: "test", Log: t.Logf,
			Connect: func(context.Context, server.Config) (*server.Conn, error) { return nil, errors.New("offline") }})
		ctx, cancel := context.WithCancel(context.Background())
		go m.Run(ctx)
		select {
		case <-m.Listened():
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("never tried to listen")
		}
		if got := m.ListenPort() != 0; got != tc.want {
			t.Errorf("port %d: listening %v", tc.port, got)
		}
		cancel()
	}
}
