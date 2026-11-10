package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"mymule/internal/config"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/ipfilter"
	"mymule/internal/kad"
	"mymule/internal/peer"
	"mymule/internal/server"
	"mymule/internal/share"
)

// Kad in the daemon: the node, wired to the download manager (sources,
// searches, firewall checks, callbacks, buddies) and to the shared
// library (publishing).

// kadEngine is the daemon's Kad node.
type kadEngine struct {
	node *kad.Node
	conn *net.UDPConn
	m    **download.Manager
	done chan struct{} // closed when the node has stopped (and saved)
}

func addrOf(ip net.IP) netip.Addr {
	a, _ := netip.AddrFromSlice(ip.To4())
	return a
}

func ipOf(a netip.Addr) net.IP { return net.IP(a.Unmap().AsSlice()) }

// sharedForKad lists the shared files for publishing, again only when the
// library changed (Kad asks every second).
func sharedForKad(lib *share.Library) func() (int, []kad.SharedFile) {
	var mu sync.Mutex
	version := -1
	var files []kad.SharedFile
	return func() (int, []kad.SharedFile) {
		v := lib.Version()
		mu.Lock()
		defer mu.Unlock()
		if v != version {
			files = files[:0:0]
			for _, f := range lib.List() {
				files = append(files, kad.SharedFile{Hash: f.FileHash(), Name: f.FileName(), Size: uint64(f.FileSize()), Complete: !f.Partial()})
			}
			version = v
		}
		return v, files
	}
}

// fwCheckPayload is OP_FWCHECKUDPREQ's payload: our local and external
// Kad ports and the key the client must send the test with.
func fwCheckPayload(intern, extern uint16, key uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[0:], intern)
	binary.LittleEndian.PutUint16(b[2:], extern)
	binary.LittleEndian.PutUint32(b[4:], key)
	return b
}

// callbackNode is what kadCallback needs of the node.
type callbackNode interface {
	RequestCallback(buddyIP netip.Addr, buddyPort uint16, buddyID kad.ID, file [16]byte) error
	DirectCallback(ip netip.Addr, udpPort uint16) error
}

// kadCallback asks a firewalled Kad source to connect to us: through its
// buddy (types 3 and 5) or directly over UDP (type 6).
func kadCallback(node callbackNode, s kad.Source, file ed2k.Hash) error {
	switch s.Type {
	case 3, 5:
		return node.RequestCallback(s.BuddyIP, s.BuddyPort, s.Buddy, file)
	case 6:
		return node.DirectCallback(s.IP, s.UDPPort)
	}
	return errors.New("not a callback source")
}

// kadQuery and kadResult convert between server and Kad searches.
func kadQuery(q server.Query) kad.Query {
	return kad.Query{Keywords: q.Keywords, Type: q.Type, MinSize: q.MinSize, MaxSize: q.MaxSize, Extension: q.Extension}
}

func kadResult(r kad.KeywordResult) server.Result {
	return server.Result{Hash: ed2k.Hash(r.Hash), Name: r.Name, Size: r.Size, Type: r.Type, Sources: r.Sources}
}

// setupKad opens the Kad port and creates the node, adding its hooks to
// dc; *m is the manager once it exists (the hooks wait for it). When Kad
// cannot run, it returns nil and says why in dc.KadProblem.
func setupKad(dc *download.Config, m **download.Manager, lib *share.Library, uh ed2k.Hash) *kadEngine {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: int(settings.KadPort)})
	if err != nil {
		dc.KadProblem = fmt.Sprintf("cannot use UDP port %d: %v", settings.KadPort, err)
		logf("Kad disabled: %s", dc.KadProblem)
		return nil
	}
	dir, err := config.Dir()
	if err != nil {
		conn.Close()
		dc.KadProblem = err.Error()
		logf("Kad disabled: %v", err)
		return nil
	}
	k := &kadEngine{conn: conn, m: m, done: make(chan struct{})}
	// mgr is the manager, nil until it exists: the hooks do nothing then.
	mgr := func() *download.Manager { return *m }
	var node *kad.Node
	cfg := kad.Config{
		Conn:     ipfilter.PacketConn(conn, dc.IPFilter), // Kad and client UDP
		Dir:      dir,
		UserHash: uh,
		Crypt:    obfuscation(),
		TCPPort: func() uint16 {
			if mm := mgr(); mm != nil {
				return mm.ListenPort()
			}
			return 0
		},
		PublicIP: func() netip.Addr {
			if mm := mgr(); mm != nil {
				return mm.PublicIP()
			}
			return netip.Addr{}
		},
		ConnectBack: func(ip netip.Addr, port uint16, user [16]byte) bool {
			mm := mgr()
			return mm != nil && mm.KadConnectBack(ipOf(ip), port, ed2k.Hash(user))
		},
		FileType: server.FileTypeFor,
		CheckUDP: func(ip netip.Addr, tcp, intern, extern uint16, key uint32) bool {
			mm := mgr()
			return mm != nil && mm.KadUDPCheck(ipOf(ip), tcp, fwCheckPayload(intern, extern, key), func() { node.FailedUDPTest(ip) })
		},
		ConnectBuddy: func(ip netip.Addr, tcp uint16, user [16]byte) bool {
			mm := mgr()
			return mm != nil && mm.KadConnectBuddy(ipOf(ip), tcp, ed2k.Hash(user))
		},
		ConnectTo: func(ip netip.Addr, tcp uint16) {
			if mm := mgr(); mm != nil {
				mm.KadConnectTo(ipOf(ip), tcp)
			}
		},
		// The Kad port is our eD2K client UDP port too (as eMule's).
		ClientUDP: func(from netip.AddrPort, op byte, payload []byte) {
			if mm := mgr(); mm != nil {
				mm.OnClientUDP(from, op, payload)
			}
		},
		Log: logf,
	}
	if lib != nil {
		cfg.Shared = sharedForKad(lib)
	}
	node, err = kad.New(cfg)
	if err != nil {
		conn.Close()
		dc.KadProblem = err.Error()
		logf("Kad disabled: %v", err)
		return nil
	}
	k.node = node
	dc.SendClientUDP = func(to netip.AddrPort, op byte, payload []byte, user ed2k.Hash) {
		node.SendClientUDP(to, op, payload, [16]byte(user))
	}
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)

	dc.Kad = func() (uint16, byte) { return port, kad.Version }
	dc.KadFirewall = func() download.KadFirewall {
		fw := node.Firewall()
		var ip net.IP
		if fw.BuddyIP.IsValid() {
			ip = ipOf(fw.BuddyIP)
		}
		return download.KadFirewall{BuddyIP: ip, BuddyPort: fw.BuddyPort, DirectCallback: fw.DirectCallback}
	}
	dc.KadFirewallAck = func(ip net.IP) { node.TCPCheckAck(addrOf(ip)) }
	dc.KadLegacyAck = func(ip net.IP, port uint16) { node.SendFirewalledAck(addrOf(ip), port) }
	dc.KadSources = func(h ed2k.Hash, size uint64, found func(kad.Source)) error {
		return node.FindSources(h, size, found)
	}
	dc.KadCallback = func(s kad.Source, file ed2k.Hash) error { return kadCallback(node, s, file) }
	dc.KadSearch = func(ctx context.Context, q server.Query, found func(server.Result)) error {
		return node.SearchKeyword(ctx, kadQuery(q), func(r kad.KeywordResult) { found(kadResult(r)) })
	}
	dc.KadNotes = func(ctx context.Context, h ed2k.Hash, size uint64, found func(kad.Note)) error {
		return node.SearchNotes(ctx, h, size, found)
	}
	dc.KadUDPCheckRequest = func(ip net.IP, p []byte) { node.TestUDPFor(addrOf(ip), p) }
	dc.KadIncomingBuddy = func(ip net.IP, user ed2k.Hash, send func(byte, []byte) bool) bool {
		return node.IncomingBuddy(addrOf(ip), user, send)
	}
	dc.KadServedBuddyGone = func(ip net.IP) { node.ServedBuddyGone(addrOf(ip)) }
	dc.KadOwnCallback = node.Callback
	dc.KadBuddyUp = func(ip net.IP, port uint16) { node.BuddyUp(addrOf(ip), port) }
	dc.KadBuddyDown = func(ip net.IP) { node.BuddyDown(addrOf(ip)) }
	dc.KadBuddyOffer = func(ip net.IP, user ed2k.Hash) bool { return node.IsBuddyOffer(addrOf(ip), user) }
	dc.KadStatus = node.Status
	dc.OnPeerHello = func(ip net.IP, h peer.Hello) { kadPeerHello(node, ip, h) }
	logf("Kad: UDP port %d, ID %s", port, node.ID())
	return k
}

// kadPeerHello takes a client that runs Kad (its hello gives its Kad port)
// as a node to start from while we know few.
func kadPeerHello(node *kad.Node, ip net.IP, h peer.Hello) {
	if p := h.KadPort(); p != 0 && h.KadVersion() >= 2 {
		node.AddBootstrapContact(addrOf(ip), p)
	}
}

// run runs the node until ctx ends, and gets a nodes.dat when it knows no
// node at all.
func (k *kadEngine) run(ctx context.Context, tcpPort uint16) {
	go func() {
		defer close(k.done)
		defer k.conn.Close()
		// Our first packets tell our TCP port: wait (briefly) for the
		// manager to listen on it.
		for i := 0; tcpPort != 0 && i < 50 && ctx.Err() == nil; i++ {
			if mm := *k.m; mm != nil && mm.ListenPort() != 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		k.node.Run(ctx)
	}()
	go func() {
		var last time.Time
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			if settings.KadNodesURL != "" && k.node.NeedsNodes() && time.Since(last) > 30*time.Minute {
				last = time.Now()
				if n, err := fetchNodes(ctx, k.node, settings.KadNodesURL); err != nil {
					logf("Kad: could not get nodes from %s: %v", settings.KadNodesURL, err)
				} else {
					logf("Kad: %d nodes from %s", n, settings.KadNodesURL)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// wait waits for the node to stop (it saves nodes.dat and kadindex.dat).
func (k *kadEngine) wait() { <-k.done }

// close releases the port of a node that never ran.
func (k *kadEngine) close() { k.conn.Close() }

// nodesClient fetches nodes.dat: it follows at most 3 redirects, only to
// the same host and never from https to http (the file decides which
// nodes we trust first; nothing else should get to decide it, or make us
// fetch from elsewhere).
var nodesClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		first := via[0].URL
		switch {
		case len(via) > 3:
			return errors.New("too many redirects")
		case req.URL.Hostname() != first.Hostname():
			return fmt.Errorf("redirect to another host (%s)", req.URL.Hostname())
		case first.Scheme == "https" && req.URL.Scheme != "https":
			return errors.New("redirect from https to http")
		case req.URL.Scheme != "http" && req.URL.Scheme != "https":
			return errors.New("redirect to a non-HTTP address")
		}
		return nil
	},
}

// fetchNodes downloads a nodes.dat and adds its nodes.
func fetchNodes(ctx context.Context, node *kad.Node, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := nodesClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, kad.MaxNodesFile+1))
	if err != nil {
		return 0, err
	}
	if len(b) > kad.MaxNodesFile {
		return 0, errors.New("nodes.dat too big")
	}
	return node.AddNodes(b)
}
