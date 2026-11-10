package upload

import (
	"net"
	"sync"
	"time"

	"mymule/internal/aich"
	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/share"
)

// AICHRooter is a shared file (a download) that may know its AICH hash
// for sure: from its link, or agreed by many peers.
type AICHRooter interface {
	AICHRoot() (aich.Hash, bool)
}

// maxRecoveryPerMinute bounds the recovery data we build: each one reads
// a whole part (9.28 MB) from disk, and peers only ask after a corrupt
// part.
const maxRecoveryPerMinute = 20

// maxRecoveryPerPeer bounds what one network (a /24, or a /48 for IPv6)
// may take of that, so that a few addresses of one owner cannot use it
// all up and leave the others without.
const maxRecoveryPerPeer = 3

// recoveryBudget counts the recovery data built in the current minute.
type recoveryBudget struct {
	mu     sync.Mutex
	start  time.Time
	n      int
	byPeer map[string]int // by network
}

func (b *recoveryBudget) take(now time.Time, ip net.IP) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.start) >= time.Minute {
		b.start, b.n, b.byPeer = now, 0, nil
	}
	who := peerNet(ip)
	if b.n >= maxRecoveryPerMinute || b.byPeer[who] >= maxRecoveryPerPeer {
		return false
	}
	if b.byPeer == nil {
		b.byPeer = map[string]int{}
	}
	b.n++
	b.byPeer[who]++
	return true
}

// peerNet is the network an address is in (see maxRecoveryPerPeer).
func peerNet(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return ip.Mask(net.CIDRMask(48, 128)).String()
}

// aichRoot is the AICH hash we can give for a shared file.
func (m *Manager) aichRoot(h ed2k.Hash) (aich.Hash, bool) {
	lib := m.cfg.Library
	if lib == nil {
		return aich.Hash{}, false
	}
	if t := lib.AICH(h); t != nil {
		return t.Root(), true
	}
	if r, ok := lib.Get(h).(AICHRooter); ok {
		return r.AICHRoot()
	}
	return aich.Hash{}, false
}

// handleAICH answers AICH requests: a file's AICH hash, and the recovery
// data of a part of a complete file.
func (m *Manager) handleAICH(c *peer.Conn, ip net.IP, p *proto.Packet) error {
	switch p.Opcode {
	case peer.OpAICHFileHashReq:
		if len(p.Payload) < 16 {
			return nil
		}
		var h ed2k.Hash
		copy(h[:], p.Payload)
		root, ok := m.aichRoot(h)
		if !ok {
			return nil // as eMule: no answer
		}
		return c.Send(proto.ProtoEMule, peer.OpAICHFileHashAns, peer.EncodeAICHFileHash(h, root))

	case peer.OpAICHRequest:
		h, part, root, err := peer.DecodeAICHRequest(p.Payload)
		if err != nil {
			return nil
		}
		data := m.recoveryData(h, part, root, ip)
		return c.Send(proto.ProtoEMule, peer.OpAICHAnswer, peer.EncodeAICHAnswer(h, part, root, data))
	}
	return nil
}

// recoveryData builds the recovery data of a part of a complete shared
// file whose AICH hash is root (nil: none), for the peer at ip.
func (m *Manager) recoveryData(h ed2k.Hash, part int, root aich.Hash, ip net.IP) []byte {
	lib := m.cfg.Library
	if lib == nil {
		return nil
	}
	t := lib.AICH(h)
	f := lib.Get(h)
	if t == nil || f == nil || f.Partial() || t.Root() != root || !aich.CanRecover(t.Size, part) || !m.recovery.take(time.Now(), ip) {
		return nil
	}
	blocks, err := aich.BlockHashes(t.Size, part, func(b []byte, off int64) error { return share.ReadFull(f, b, off) })
	if err != nil {
		return nil
	}
	data, err := t.Recovery(part, blocks)
	if err != nil {
		// The data no longer matches the tree: the file changed.
		m.cfg.Debug("AICH: %s: %v", f.FileName(), err)
		return nil
	}
	return data
}
