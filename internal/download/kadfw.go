package download

import (
	"context"
	"encoding/hex"
	"net"
	"net/netip"
	"strconv"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/safe"
)

// The eD2K (TCP) side of Kad firewall support: the UDP firewall test, the
// buddy connections and the sources reachable only by a callback.

// KadFirewall is what the Kad side says about our reachability, for our
// hello: our buddy when we are firewalled, or whether we take direct
// callbacks.
type KadFirewall struct {
	BuddyIP        net.IP
	BuddyPort      uint16
	DirectCallback bool
}

// Buddy timing (eMule's): a ping every 10 minutes keeps the NAT mapping;
// pings from the other side are answered at most every 3 minutes.
const (
	buddyPingEvery    = 10 * time.Minute
	buddyPongMinEvery = 3 * time.Minute
	buddyIdle         = 25 * time.Minute
)

// maxKadTasks is how many of our own Kad checks run at once.
const maxKadTasks = 4

func toAddr(ip net.IP) netip.Addr {
	a, _ := netip.AddrFromSlice(ip.To4())
	return a
}

// kadTask runs f in the background for one of our Kad checks, at most
// maxKadTasks at once; false when it will not run.
func (m *Manager) kadTask(name string, f func(ctx context.Context)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx := m.ctx
	if ctx == nil || ctx.Err() != nil || m.closing {
		return false
	}
	if m.kadTasks.Add(1) > maxKadTasks {
		m.kadTasks.Add(-1)
		return false
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.kadTasks.Add(-1)
		safe.Catch(m.cfg.Log, name, func() { f(ctx) }, m.locks()...)
	}()
	return true
}

// dialPeer connects to ip:port and exchanges hellos.
func (m *Manager) dialPeer(ctx context.Context, ip net.IP, port uint16) (*peer.Conn, peer.Hello, error) {
	nc, err := m.dial(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), ed2k.Hash{}, 0)
	if err != nil {
		return nil, peer.Hello{}, err
	}
	c := peer.NewConn(nc)
	h, err := m.handshake(c, true)
	if err != nil {
		c.Close()
		return nil, h, err
	}
	return c, h, nil
}

// KadUDPCheck asks the client at ip:port to send us the Kad UDP firewall
// test packet (payload is OP_FWCHECKUDPREQ's). It returns false when it
// will not try; onFail is called if the client could not be asked.
func (m *Manager) KadUDPCheck(ip net.IP, port uint16, payload []byte, onFail func()) bool {
	if port == 0 {
		return false
	}
	return m.kadTask("kad UDP check", func(ctx context.Context) {
		c, h, err := m.dialPeer(ctx, ip, port)
		if err != nil {
			m.cfg.Debug("kad UDP check by %s:%d: %v", ip, port, err)
			onFail()
			return
		}
		m.trackConn(c, true)
		defer m.trackConn(c, false)
		defer c.Close()
		if h.KadVersion() <= 5 || h.KadPort() == 0 {
			onFail() // does not do the test
			return
		}
		if c.Send(proto.ProtoEMule, kad.OpFWCheckUDPReq, payload) != nil {
			onFail()
			return
		}
		c.Read(10 * time.Second) // let it arrive
	})
}

// KadConnectBuddy connects to the node that agreed to be our buddy, and
// keeps the connection: pings, and the callbacks it passes on (a
// downloader that wants a file from us). False when it will not try.
func (m *Manager) KadConnectBuddy(ip net.IP, port uint16, userHash ed2k.Hash) bool {
	m.mu.Lock()
	ctx := m.ctx
	ok := ctx != nil && ctx.Err() == nil && !m.closing && !m.buddyBusy && port != 0
	if ok {
		m.buddyBusy = true
		m.wg.Add(1)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			m.buddyBusy = false
			m.mu.Unlock()
			if m.cfg.KadBuddyDown != nil {
				m.cfg.KadBuddyDown(ip)
			}
		}()
		safe.Catch(m.cfg.Log, "kad buddy", func() { m.ourBuddy(ctx, ip, port, userHash) }, m.locks()...)
	}()
	return true
}

func (m *Manager) ourBuddy(ctx context.Context, ip net.IP, port uint16, userHash ed2k.Hash) {
	c, h, err := m.dialPeer(ctx, ip, port)
	if err != nil {
		m.cfg.Debug("kad buddy %s:%d: %v", ip, port, err)
		return
	}
	m.trackConn(c, true)
	defer m.trackConn(c, false)
	defer c.Close()
	if h.UserHash != userHash {
		return // another client at that address
	}
	kadPort := h.KadPort()
	if kadPort == 0 {
		kadPort = uint16(h.Tags.Int(peer.CTEMuleUDPPorts))
	}
	if m.cfg.KadBuddyUp != nil {
		m.cfg.KadBuddyUp(ip, kadPort)
	}
	m.cfg.Log("Kad: connected to our buddy %s", ip)
	lastPing := time.Time{}
	heard := time.Now()
	for ctx.Err() == nil {
		if time.Since(lastPing) >= buddyPingEvery {
			if c.Send(proto.ProtoEMule, kad.OpBuddyPing, nil) != nil {
				return
			}
			lastPing = time.Now()
		}
		p, err := c.Read(time.Minute)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Since(heard) < buddyIdle {
				continue
			}
			return
		}
		heard = time.Now()
		if p.Proto == proto.ProtoEMule && p.Opcode == kad.OpCallback && m.cfg.KadOwnCallback != nil {
			m.onOwnCallback(p.Payload)
		}
	}
}

// onOwnCallback connects to a downloader our buddy says wants a file from
// us, if we share it.
func (m *Manager) onOwnCallback(payload []byte) {
	ip, port, file, ok := m.cfg.KadOwnCallback(payload)
	if !ok {
		return
	}
	h := ed2k.Hash(file)
	shared := m.cfg.Library != nil && m.cfg.Library.Get(h) != nil
	m.mu.Lock()
	_, downloading := m.files[h]
	m.mu.Unlock()
	if !shared && !downloading {
		return
	}
	m.KadConnectTo(net.IP(ip.AsSlice()), port)
}

// KadConnectTo connects to a downloader that asked, through Kad, that we
// connect to it (a callback through our buddy, or a direct one). Like the
// server's callbacks, these go through the callback gate: few at once, few
// a minute, each address at most once a minute, so that whoever makes the
// requests cannot use us to connect to hosts of its choice at will.
func (m *Manager) KadConnectTo(ip net.IP, port uint16) bool {
	if ok, why := m.callbacks.admit(ip, port, false, time.Now()); !ok {
		m.cfg.Debug("Kad callback to %s:%d refused: %s", ip, port, why)
		return false
	}
	m.mu.Lock()
	ctx := m.ctx
	if ctx == nil || ctx.Err() != nil || m.closing {
		m.mu.Unlock()
		m.callbacks.done()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
		nc, err := m.dial(ctx, "tcp4", addr, ed2k.Hash{}, 0)
		m.callbacks.done()
		if err != nil {
			return
		}
		m.runSession(ctx, nc, true, nil)
	}()
	return true
}

// serveBuddy keeps the connection of the firewalled node we are the buddy
// of: it answers its pings; the Kad side sends it callbacks.
func (m *Manager) serveBuddy(ctx context.Context, c *peer.Conn) {
	lastPong := time.Time{}
	heard := time.Now()
	for ctx.Err() == nil {
		p, err := c.Read(time.Minute)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Since(heard) < buddyIdle {
				continue
			}
			return
		}
		heard = time.Now()
		if p.Proto == proto.ProtoEMule && p.Opcode == kad.OpBuddyPing && time.Since(lastPong) >= buddyPongMinEvery {
			if c.Send(proto.ProtoEMule, kad.OpBuddyPong, nil) != nil {
				return
			}
			lastPong = time.Now()
		}
	}
}

// incomingBuddy reports whether a peer that connected to us is the
// firewalled node we agreed to be the buddy of, and if so serves it.
func (m *Manager) incomingBuddy(ctx context.Context, c *peer.Conn, h peer.Hello) bool {
	if m.cfg.KadIncomingBuddy == nil {
		return false
	}
	ip := c.RemoteIP()
	// The Kad side hands packets over without waiting: a queue, written by
	// a goroutine of its own, so that a node that stops reading cannot
	// stall Kad (packets are dropped when the queue is full).
	type out struct {
		op      byte
		payload []byte
	}
	queue := make(chan out, buddyQueue)
	stop := make(chan struct{})
	send := func(op byte, payload []byte) bool {
		select {
		case queue <- out{op, payload}:
			return true
		case <-stop:
			return false
		default:
			return false
		}
	}
	if !m.cfg.KadIncomingBuddy(ip, h.UserHash, send) {
		return false
	}
	m.cfg.Log("Kad: serving as the buddy of %s", ip)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case p := <-queue:
				if c.Send(proto.ProtoEMule, p.op, p.payload) != nil {
					c.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-writerDone
		if m.cfg.KadServedBuddyGone != nil {
			m.cfg.KadServedBuddyGone(ip)
		}
	}()
	m.serveBuddy(ctx, c)
	return true
}

// buddyQueue is how many packets for the node we serve may wait.
const buddyQueue = 16

// Kad callbacks: at most kadCallbacksPerSecond asked, each address at
// most every kadCallEvery, and at most maxKadFWPerFile such sources per
// file (apart from the LowID limit: they cost us a callback each, and a
// node answering a source search can name hundreds).
const (
	kadCallbacksPerSecond = 4
	kadCallEvery          = time.Minute
	maxKadFWPerFile       = 30
)

// kadSourceKey is the key of a source reachable only by a callback.
func kadSourceKey(user [16]byte) string { return "kad:" + hex.EncodeToString(user[:]) }

// addKadCallbackSource adds a firewalled Kad source of f: its buddy (or
// itself over UDP) is asked to have it connect to us.
func (m *Manager) addKadCallbackSource(f *File, s kad.Source) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := kadSourceKey(s.UserHash)
	if _, ok := f.sources[key]; ok {
		return false
	}
	lowIDs, kadFW := 0, 0
	for _, o := range f.sources {
		if o.LowID != 0 || o.kadFW != nil {
			lowIDs++
		}
		if o.kadFW != nil {
			kadFW++
		}
	}
	if lowIDs >= MaxLowIDPerFile || kadFW >= maxKadFWPerFile || len(f.sources) >= MaxSourcesPerFile {
		return false
	}
	ks := s
	f.sources[key] = &Source{key: key, UserHash: ed2k.Hash(s.UserHash), kadFW: &ks, file: f, NextAsk: time.Now()}
	return true
}
