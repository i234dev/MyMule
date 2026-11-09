// Package portmap opens our ports on the router, with NAT-PMP or UPnP,
// so that peers can connect to us without forwarding them by hand.
package portmap

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Port is a port to open: "tcp" or "udp", and its number (the same
// outside and in).
type Port struct {
	Proto string
	Port  uint16
}

func (p Port) String() string { return fmt.Sprintf("%s %d", strings.ToUpper(p.Proto), p.Port) }

// Lease is how long a mapping is asked for; it is renewed at half.
const Lease = time.Hour

// RetryAfter is how long after finding no router it looks again.
const RetryAfter = 30 * time.Minute

// Config says what to open.
type Config struct {
	Ports       []Port
	Description string // shown in the router's list ("MyMule")
	Log         func(format string, args ...any)

	// For tests: the NAT-PMP gateway (default: found from the routing
	// table) and where to send the UPnP search (default: SSDPAddr).
	Gateway func() (netip.AddrPort, error)
	SSDP    string
}

// State is what was opened, for the status.
type State struct {
	Method string // "NAT-PMP", "UPnP", or "" when nothing is open
	Ports  []Port // the ports open
	Error  string // why not, when nothing could be opened
}

// Mapper keeps the ports open while it runs.
type Mapper struct {
	cfg Config

	mu    sync.Mutex
	state State
}

// New returns a mapper; Run starts it.
func New(cfg Config) *Mapper {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Gateway == nil {
		cfg.Gateway = defaultGateway
	}
	if cfg.SSDP == "" {
		cfg.SSDP = SSDPAddr
	}
	if cfg.Description == "" {
		cfg.Description = "MyMule"
	}
	return &Mapper{cfg: cfg}
}

// State returns what is open now.
func (m *Mapper) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.Ports = append([]Port(nil), s.Ports...)
	return s
}

func (m *Mapper) set(s State) {
	m.mu.Lock()
	old := m.state
	m.state = s
	m.mu.Unlock()
	switch {
	case s.Method != "" && (old.Method != s.Method || len(old.Ports) != len(s.Ports)):
		names := make([]string, len(s.Ports))
		for i, p := range s.Ports {
			names[i] = p.String()
		}
		m.cfg.Log("router: opened %s with %s", strings.Join(names, " and "), s.Method)
	case s.Method == "" && (old.Method != "" || old.Error == ""):
		m.cfg.Log("router: could not open the ports automatically (%s); forward them by hand for HighID", s.Error)
	}
}

// Run opens the ports and keeps them open until ctx is done, then closes
// them.
func (m *Mapper) Run(ctx context.Context) {
	if len(m.cfg.Ports) == 0 {
		return
	}
	var open func(ctx context.Context) (time.Duration, error)
	var closeAll func(ctx context.Context)
	defer func() {
		if closeAll != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			closeAll(cctx)
			cancel()
		}
	}()
	for {
		next := RetryAfter
		if open != nil {
			// Renew the way that worked; if it stops working, look again.
			if d, err := open(ctx); err == nil {
				next = d
			} else if ctx.Err() == nil {
				open, closeAll = nil, nil
				m.set(State{Error: err.Error()})
				next = time.Minute
			}
		} else if o, c, d, err := m.find(ctx); err == nil {
			open, closeAll, next = o, c, d
		} else if ctx.Err() == nil {
			m.set(State{Error: err.Error()})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// find tries NAT-PMP, then UPnP. It returns how to renew the mappings
// and how to remove them, and when to renew.
func (m *Mapper) find(ctx context.Context) (open func(context.Context) (time.Duration, error), closeAll func(context.Context), next time.Duration, err error) {
	if gw, gerr := m.cfg.Gateway(); gerr == nil {
		open = func(ctx context.Context) (time.Duration, error) { return m.natpmpOpen(ctx, gw) }
		if next, err = open(ctx); err == nil {
			return open, func(ctx context.Context) { m.natpmpClose(ctx, gw) }, next, nil
		}
	} else {
		err = gerr
	}
	natErr := err
	g, uerr := discover(ctx, m.cfg.SSDP)
	if uerr != nil {
		return nil, nil, 0, fmt.Errorf("NAT-PMP: %v; UPnP: %v", natErr, uerr)
	}
	open = func(ctx context.Context) (time.Duration, error) { return m.upnpOpen(ctx, g) }
	if next, err = open(ctx); err != nil {
		return nil, nil, 0, err
	}
	return open, func(ctx context.Context) { m.upnpClose(ctx, g) }, next, nil
}

func (m *Mapper) natpmpOpen(ctx context.Context, gw netip.AddrPort) (time.Duration, error) {
	life := uint32(Lease / time.Second)
	for _, p := range m.cfg.Ports {
		ext, got, err := natpmpMap(ctx, gw, p.Proto, p.Port, p.Port, life)
		if err != nil {
			return 0, err
		}
		if ext != p.Port {
			// Peers are told our port: another one outside is no use.
			natpmpMap(ctx, gw, p.Proto, p.Port, ext, 0)
			return 0, fmt.Errorf("NAT-PMP: the router gave %s port %d for %d", strings.ToUpper(p.Proto), ext, p.Port)
		}
		life = min(life, max(got, 120))
	}
	m.set(State{Method: "NAT-PMP", Ports: m.cfg.Ports})
	return time.Duration(life) * time.Second / 2, nil
}

func (m *Mapper) natpmpClose(ctx context.Context, gw netip.AddrPort) {
	for _, p := range m.cfg.Ports {
		natpmpMap(ctx, gw, p.Proto, p.Port, 0, 0)
	}
}

func (m *Mapper) upnpOpen(ctx context.Context, g *igd) (time.Duration, error) {
	for _, p := range m.cfg.Ports {
		if _, err := g.add(ctx, p.Proto, p.Port, uint32(Lease/time.Second), m.cfg.Description); err != nil {
			return 0, err
		}
	}
	m.set(State{Method: "UPnP", Ports: m.cfg.Ports})
	// Renewed at half the lease even when the router took only permanent
	// mappings: a router that restarted may have lost them.
	return Lease / 2, nil
}

func (m *Mapper) upnpClose(ctx context.Context, g *igd) {
	for _, p := range m.cfg.Ports {
		g.remove(ctx, p.Proto, p.Port)
	}
}

// defaultGateway finds the router for NAT-PMP: the default route on
// Linux; elsewhere, the .1 address of our private network (what most
// home routers use).
func defaultGateway() (netip.AddrPort, error) {
	if gw, ok := linuxGateway("/proc/net/route"); ok {
		return netip.AddrPortFrom(gw, NATPMPPort), nil
	}
	local, err := localAddrTo(netip.MustParseAddr("192.0.2.1")) // no packet is sent
	if err != nil {
		return netip.AddrPort{}, err
	}
	if !local.Is4() || !local.IsPrivate() {
		return netip.AddrPort{}, fmt.Errorf("no router: our address %s is not a private one", local)
	}
	a := local.As4()
	a[3] = 1
	return netip.AddrPortFrom(netip.AddrFrom4(a), NATPMPPort), nil
}

// linuxGateway reads the default route's gateway from /proc/net/route.
func linuxGateway(path string) (netip.Addr, bool) {
	f, err := os.Open(path)
	if err != nil {
		return netip.Addr{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Iface Destination Gateway Flags ...; flags 0x2: has a gateway
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		v := binary.LittleEndian.Uint32(b)
		var a [4]byte
		binary.BigEndian.PutUint32(a[:], v)
		gw := netip.AddrFrom4(a)
		if gw.IsPrivate() {
			return gw, true
		}
	}
	return netip.Addr{}, false
}
