package upload

import (
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

// recoveryBudget counts the recovery data built in the current minute.
type recoveryBudget struct {
	mu    sync.Mutex
	start time.Time
	n     int
}

func (b *recoveryBudget) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.start) >= time.Minute {
		b.start, b.n = now, 0
	}
	if b.n >= maxRecoveryPerMinute {
		return false
	}
	b.n++
	return true
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
func (m *Manager) handleAICH(c *peer.Conn, p *proto.Packet) error {
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
		data := m.recoveryData(h, part, root)
		return c.Send(proto.ProtoEMule, peer.OpAICHAnswer, peer.EncodeAICHAnswer(h, part, root, data))
	}
	return nil
}

// recoveryData builds the recovery data of a part of a complete shared
// file whose AICH hash is root (nil: none).
func (m *Manager) recoveryData(h ed2k.Hash, part int, root aich.Hash) []byte {
	lib := m.cfg.Library
	if lib == nil {
		return nil
	}
	t := lib.AICH(h)
	f := lib.Get(h)
	if t == nil || f == nil || f.Partial() || t.Root() != root || !aich.CanRecover(t.Size, part) || !m.recovery.take(time.Now()) {
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
