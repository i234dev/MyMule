package main

import (
	"net"
	"testing"

	"mymule/internal/server"
)

// A connection result is counted in the server list as it is on disk now,
// not in an older copy that would undo changes made since.
func TestServerStatsUseCurrentList(t *testing.T) {
	t.Setenv("MYMULE_HOME", t.TempDir())
	a := server.Info{IP: net.IPv4(192, 0, 2, 1).To4(), Port: 4661}
	b := server.Info{IP: net.IPv4(192, 0, 2, 2).To4(), Port: 4661}
	if err := saveServers([]server.Info{a}); err != nil {
		t.Fatal(err)
	}
	old, _ := loadServers()
	_ = old                          // the list a connection was chosen from
	saveServers([]server.Info{a, b}) // a "servers update" meanwhile
	updateServerStats(a, false)      // the connection to a failed
	list, _ := loadServers()
	if len(list) != 2 || list[0].Fails != 1 {
		t.Fatalf("list after the failure: %+v", list)
	}
}

// Dead servers leave the list, but only while some server still answers.
func TestPruneServers(t *testing.T) {
	ip := func(n byte) net.IP { return net.IPv4(192, 0, 2, n).To4() }
	dead := server.Info{IP: ip(1), Port: 1, Fails: maxServerFails}
	ok := server.Info{IP: ip(2), Port: 1}
	failing := server.Info{IP: ip(3), Port: 1, Fails: 3}
	if got := pruneServers([]server.Info{dead, ok, failing}); len(got) != 2 || got[0].Addr() != ok.Addr() {
		t.Fatalf("pruned to %+v", got)
	}
	offline := []server.Info{dead, failing}
	if got := pruneServers(offline); len(got) != 2 {
		t.Fatalf("pruned without a network: %+v", got)
	}
	// Back from an outage: one server answers, the others still carry
	// the outage's failures.
	back := []server.Info{ok}
	for i := byte(10); i < 20; i++ {
		back = append(back, server.Info{IP: ip(i), Port: 1, Fails: maxServerFails + 3})
	}
	if got := pruneServers(back); len(got) != len(back) {
		t.Fatalf("pruned to %d servers after an outage", len(got))
	}
}
