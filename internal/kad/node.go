package kad

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"mymule/internal/fsutil"
	"mymule/internal/proto"
	"mymule/internal/safe"
)

// Config sets up a Node.
type Config struct {
	// Conn is the UDP socket on the Kad port. The node reads all of it.
	Conn net.PacketConn
	// Dir holds kad.dat (our ID and key) and nodes.dat.
	Dir      string
	UserHash [16]byte
	// TCPPort returns our eD2K TCP port (0 = no incoming connections).
	TCPPort func() uint16
	// PublicIP returns our public IP if known some other way (from an
	// eD2K server); may be nil.
	PublicIP func() netip.Addr
	// ConnectBack asks the eD2K side to connect to a node's TCP port and
	// tell it we got through (OP_KAD_FWTCPCHECK_ACK): the node is checking
	// whether it is firewalled. It returns false if it will not try.
	ConnectBack func(ip netip.Addr, tcpPort uint16, userHash [16]byte) bool
	// Shared returns the files we share and a number that changes when
	// the list does (nil = publish nothing). It is called with the node
	// locked, every second: it must be quick and must not call the node.
	Shared func() (version int, files []SharedFile)
	// FileType returns the eD2K file type of a name ("Video"...); may be
	// nil.
	FileType func(name string) string
	Log      func(format string, args ...any)
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time
}

// Node is our Kad node.
type Node struct {
	cfg   Config
	conn  net.PacketConn
	keys  Keys
	clock func() time.Time

	mu       sync.Mutex
	table    *Table
	searches map[ID]*search
	index    *index // what others published to us
	pending  map[netip.Addr]*pendingPub
	pub      publishState // what we publish

	outTracked []tracked // requests we sent, to accept answers
	inTracked  map[netip.Addr]*floodEntry
	inCleanup  time.Time
	banned     map[netip.Addr]time.Time
	challenges []challenge

	// saves are the background saves running; saveMu orders them, so an
	// older snapshot never replaces a newer one.
	saves  sync.WaitGroup
	saveMu sync.Mutex

	// deferred are calls to make once n.mu is released (results handed to
	// other parts of MyMule, which take their own locks).
	deferred []func()

	bootstrapList []Contact
	lastBootstrap time.Time
	lastContact   time.Time

	// Our public address as other nodes see it.
	extIP, extIPLast netip.Addr
	extPortIPs       []netip.Addr
	extPorts         []uint16
	extPort          uint16

	// Whether other nodes can reach our Kad port unasked (the UDP firewall
	// test says so): only then do we store what others publish.
	udpOpen bool

	// TCP firewall check: we ask nodes to connect to our TCP port.
	recheckIP    int // answers to our firewall requests in this round
	firewalledN  int // nodes that got through
	lastFWState  bool
	fwAsked      map[netip.Addr]*fwRequest
	nextFWCheck  time.Time
	nextSelf     time.Time
	bigTimer     time.Time
	nextConsolid time.Time
	nextSave     time.Time
	started      time.Time
}

// Kad timing (eMule's).
const (
	trackWindow        = 3 * time.Minute
	disconnectDelay    = 20 * time.Minute
	firewallChecks     = 4
	externalPortAskIPs = 3
	banTime            = time.Hour
)

const stateFile = "kad.dat"

// New creates a node from its saved state (or a new identity).
func New(cfg Config) (*Node, error) {
	if cfg.Conn == nil {
		return nil, errors.New("kad: no socket")
	}
	n := &Node{
		cfg: cfg, conn: cfg.Conn, clock: cfg.Now,
		searches:  map[ID]*search{},
		index:     newIndex(),
		pending:   map[netip.Addr]*pendingPub{},
		inTracked: map[netip.Addr]*floodEntry{},
		banned:    map[netip.Addr]time.Time{},
		fwAsked:   map[netip.Addr]*fwRequest{},
	}
	if n.clock == nil {
		n.clock = time.Now
	}
	if n.cfg.Log == nil {
		n.cfg.Log = func(string, ...any) {}
	}
	if n.cfg.TCPPort == nil {
		n.cfg.TCPPort = func() uint16 { return 0 }
	}
	n.keys.UserHash = cfg.UserHash
	if err := n.loadState(); err != nil {
		return nil, err
	}
	now := n.clock()
	n.table = NewTable(n.keys.KadID, now)
	n.table.MyIP = n.publicIP
	n.started = now
	n.lastFWState = true
	n.nextFWCheck = now.Add(time.Hour)
	n.nextSelf = now.Add(3 * time.Minute)
	n.bigTimer = now
	n.nextConsolid = now.Add(45 * time.Minute)
	n.nextSave = now.Add(30 * time.Minute)
	n.loadNodes(filepath.Join(cfg.Dir, "nodes.dat"))
	if b, err := os.ReadFile(filepath.Join(cfg.Dir, indexFile)); err == nil {
		if err := n.index.decode(b, n.keys.KadID, now); err != nil {
			n.cfg.Log("kad: %s: %v", indexFile, err)
		}
	}
	return n, nil
}

// loadState reads kad.dat: [u32 1][ID][u32 verify key secret], making a
// new identity when there is none.
func (n *Node) loadState() error {
	path := filepath.Join(n.cfg.Dir, stateFile)
	b, err := os.ReadFile(path)
	if err == nil {
		r := proto.NewReader(b)
		if r.U32() == 1 {
			id := ReadID(r)
			key := r.U32()
			if r.Err() == nil && !id.IsZero() && key != 0 {
				n.keys.KadID, n.keys.UDPKey = id, key
				return nil
			}
		}
		n.cfg.Log("kad: %s unreadable, starting with a new Kad ID", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	n.keys.KadID = RandomID()
	for n.keys.UDPKey == 0 {
		var k [4]byte
		rand.Read(k[:])
		n.keys.UDPKey = binary.LittleEndian.Uint32(k[:])
	}
	var w proto.Writer
	w.U32(1)
	WriteID(&w, n.keys.KadID)
	w.U32(n.keys.UDPKey)
	return fsutil.WriteFile(path, w.B, 0o600)
}

func (n *Node) loadNodes(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			n.cfg.Log("kad: %v", err)
		}
		return
	}
	f, err := ParseNodes(b)
	if err != nil {
		n.cfg.Log("kad: %s: %v", path, err)
		return
	}
	n.addNodes(f)
}

// AddNodes adds the contacts of a nodes.dat (one downloaded, say).
func (n *Node) AddNodes(b []byte) (int, error) {
	f, err := ParseNodes(b)
	if err != nil {
		return 0, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.addNodes(f), nil
}

func (n *Node) addNodes(f NodesFile) int {
	now := n.clock()
	added := 0
	if f.Bootstrap {
		return n.addBootstrap(f.Contacts)
	}
	for _, c := range f.Contacts {
		if c.Version <= 1 || !GoodAddr(c.IP, c.UDPPort) || (c.UDPPort == 53 && c.Version <= 5) {
			continue
		}
		if !f.HasVerified {
			c.Verified = true
		}
		if ok, _ := n.table.AddUnfiltered(c, false, false, now); ok {
			added++
		}
	}
	return added
}

// maxBootstrap is how many contacts of a bootstrap nodes.dat are kept:
// the ones closest to our ID, as eMule does, so that users of the same
// file do not all ask its first nodes.
const maxBootstrap = 50

func (n *Node) addBootstrap(cs []Contact) int {
	before := len(n.bootstrapList)
	have := map[ID]bool{}
	for _, c := range n.bootstrapList {
		have[c.ID] = true
	}
	for _, c := range cs {
		if c.Version <= 1 || !GoodAddr(c.IP, c.UDPPort) || (c.UDPPort == 53 && c.Version <= 5) || have[c.ID] {
			continue
		}
		have[c.ID] = true
		n.bootstrapList = append(n.bootstrapList, c)
	}
	me := n.keys.KadID
	sort.Slice(n.bootstrapList, func(i, j int) bool {
		return n.bootstrapList[i].ID.Xor(me).Cmp(n.bootstrapList[j].ID.Xor(me)) < 0
	})
	if len(n.bootstrapList) > maxBootstrap {
		n.bootstrapList = n.bootstrapList[:maxBootstrap]
	}
	return max(0, len(n.bootstrapList)-before)
}

// SaveNodes writes nodes.dat (at least 25 contacts, as eMule).
func (n *Node) SaveNodes() error {
	n.mu.Lock()
	cs := n.table.BootstrapContacts(200)
	n.mu.Unlock()
	if len(cs) < 25 {
		return nil
	}
	return fsutil.WriteFile(filepath.Join(n.cfg.Dir, "nodes.dat"), EncodeNodes(cs), 0o600)
}

// saveIndex writes kadindex.dat.
func (n *Node) saveIndex() error {
	n.mu.Lock()
	b := n.index.encode(n.keys.KadID, n.now())
	n.mu.Unlock()
	return fsutil.WriteFile(filepath.Join(n.cfg.Dir, indexFile), b, 0o600)
}

// Bootstrap asks the node at addr for contacts.
func (n *Node) Bootstrap(addr netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.send(addr, OpBootstrapReq, nil, ID{}, 0)
}

// ID returns our Kad ID.
func (n *Node) ID() ID { return n.keys.KadID }

func (n *Node) now() time.Time { return n.clock() }

// publicIP is our public IP: as Kad nodes see it, or else as the eD2K side
// knows it.
func (n *Node) publicIP() netip.Addr {
	if n.extIP.IsValid() {
		return n.extIP
	}
	if n.cfg.PublicIP != nil {
		if ip := n.cfg.PublicIP(); ip.Unmap().Is4() {
			return ip.Unmap()
		}
	}
	return netip.Addr{}
}

// Run reads the socket and runs the timers until ctx ends.
func (n *Node) Run(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				safe.Catch(n.cfg.Log, "kad tick", func() {
					n.mu.Lock()
					defer n.runDeferred()
					defer n.mu.Unlock()
					n.tick(n.now())
				}, &n.mu)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		n.conn.SetReadDeadline(time.Unix(1, 0))
	}()
	buf := make([]byte, 65536)
	for {
		k, from, err := n.conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// Windows reports ICMP "port unreachable" for an earlier send
			// as a read error; it is not fatal.
			continue
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		addr := ua.AddrPort()
		addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
		data := append([]byte(nil), buf[:k]...)
		safe.Catch(n.cfg.Log, "kad packet", func() {
			n.mu.Lock()
			defer n.runDeferred()
			defer n.mu.Unlock()
			n.handle(data, addr, n.now())
		}, &n.mu)
	}
	<-done
	n.saves.Wait() // the last saves are the final ones
	if err := n.SaveNodes(); err != nil {
		n.cfg.Log("kad: saving nodes.dat: %v", err)
	}
	if err := n.saveIndex(); err != nil {
		n.cfg.Log("kad: saving %s: %v", indexFile, err)
	}
	return nil
}

// later queues f to run once n.mu is released.
func (n *Node) later(f func()) { n.deferred = append(n.deferred, f) }

// runDeferred runs the queued calls; n.mu must not be held.
func (n *Node) runDeferred() {
	n.mu.Lock()
	fs := n.deferred
	n.deferred = nil
	n.mu.Unlock()
	for _, f := range fs {
		safe.Catch(n.cfg.Log, "kad result", f)
	}
}

// tick runs every second.
func (n *Node) tick(now time.Time) {
	if !n.nextFWCheck.After(now) {
		n.recheckFirewalled(now)
	}
	if !n.nextSelf.After(now) {
		n.startSearch(searchNodeComplete, n.keys.KadID, now, nil)
		n.nextSelf = now.Add(4 * time.Hour)
	}
	timers, started := n.table.Tick(now, !n.bigTimer.After(now))
	if started {
		n.bigTimer = now.Add(10 * time.Second)
	}
	for _, c := range timers.Hello {
		n.sendHello(OpHelloReq, c.Addr(), c.Version, c.Key.For(n.publicIP()), c.ID, false)
	}
	for _, target := range timers.Lookup {
		n.startSearch(searchNode, target, now, nil)
	}
	n.tickSearches(now)
	n.tickPublish(now)
	n.index.clean(now)
	if !n.nextConsolid.After(now) {
		n.table.Consolidate(now)
		n.nextConsolid = now.Add(45 * time.Minute)
	}
	if !n.connected(now) && len(n.bootstrapList) > 0 &&
		(now.Sub(n.lastBootstrap) > 15*time.Second || (n.table.Len() == 0 && now.Sub(n.lastBootstrap) >= 2*time.Second)) {
		c := n.bootstrapList[0]
		n.bootstrapList = n.bootstrapList[1:]
		n.lastBootstrap = now
		target := ID{}
		if c.Version >= 6 {
			target = c.ID
		}
		n.send(c.Addr(), OpBootstrapReq, nil, target, 0)
	}
	if !n.nextSave.After(now) {
		n.nextSave = now.Add(30 * time.Minute)
		cs := n.table.BootstrapContacts(200)
		idx := n.index.encode(n.keys.KadID, now)
		dir := n.cfg.Dir
		n.saves.Add(1)
		go func() {
			defer n.saves.Done()
			n.saveMu.Lock()
			defer n.saveMu.Unlock()
			if len(cs) >= 25 {
				if err := fsutil.WriteFile(filepath.Join(dir, "nodes.dat"), EncodeNodes(cs), 0o600); err != nil {
					n.cfg.Log("kad: saving nodes.dat: %v", err)
				}
			}
			if err := fsutil.WriteFile(filepath.Join(dir, indexFile), idx, 0o600); err != nil {
				n.cfg.Log("kad: saving %s: %v", indexFile, err)
			}
		}()
	}
	if len(n.pending) > 0 && now.Second() == 0 {
		n.prunePending(now)
	}
	for ip, until := range n.banned {
		if !until.After(now) {
			delete(n.banned, ip)
		}
	}
	for ip, r := range n.fwAsked {
		if now.Sub(r.at) > trackWindow {
			delete(n.fwAsked, ip)
		}
	}
}

func (n *Node) connected(now time.Time) bool {
	return !n.lastContact.IsZero() && now.Sub(n.lastContact) < disconnectDelay
}

// send writes a Kad packet to addr, obfuscated with the node's ID when
// known (not zero), else with the verify key it gave us (receiverKey),
// else plain.
func (n *Node) send(addr netip.AddrPort, op byte, payload []byte, target ID, receiverKey uint32) {
	n.sendOpt(addr, op, payload, target, receiverKey, true)
}

// sendUntracked is send without recording a request.
func (n *Node) sendUntracked(addr netip.AddrPort, op byte, payload []byte, target ID, receiverKey uint32) {
	n.sendOpt(addr, op, payload, target, receiverKey, false)
}

func (n *Node) sendOpt(addr netip.AddrPort, op byte, payload []byte, target ID, receiverKey uint32, track bool) {
	if track && isTrackedRequest(op) {
		n.outTracked = append(n.outTracked, tracked{ip: addr.Addr(), op: op, at: n.now()})
		if len(n.outTracked) > 4096 {
			n.outTracked = n.outTracked[len(n.outTracked)-4096:]
		}
	}
	b := n.keys.EncryptKad(Packet{Opcode: op, Payload: payload}.Encode(), addr.Addr(), target, receiverKey)
	n.conn.WriteTo(b, net.UDPAddrFromAddrPort(addr))
}

// sendTo sends to a contact the way eMule does: obfuscated from version
// 6, plain before.
func (n *Node) sendTo(c Contact, op byte, payload []byte) {
	if c.Version >= 6 {
		n.send(c.Addr(), op, payload, c.ID, c.Key.For(n.publicIP()))
	} else {
		n.send(c.Addr(), op, payload, ID{}, 0)
	}
}

// Status is a snapshot for the web page and the API.
type Status struct {
	ID         ID
	Contacts   int
	Connected  bool
	Firewalled bool
	ExternalIP netip.Addr
	Searches   int
}

// Status returns the node's state.
func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	return Status{
		ID: n.keys.KadID, Contacts: n.table.Len(), Connected: n.connected(now),
		Firewalled: n.firewalled(), ExternalIP: n.extIP, Searches: len(n.searches),
	}
}

func (n *Node) String() string {
	s := n.Status()
	return fmt.Sprintf("kad %s: %d contacts, connected %v, firewalled %v", s.ID, s.Contacts, s.Connected, s.Firewalled)
}
