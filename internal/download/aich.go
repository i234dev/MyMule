package download

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"mymule/internal/aich"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// AICH on the download side. With an AICH hash we trust for the file (from
// its link, or sent by many peers alike), a part that fails its eD2K hash
// is not downloaded again whole: a peer that has it is asked for the
// part's recovery data (the hashes of its 180 KB blocks, proved against
// the AICH hash), and only the blocks that do not match are.

// As eMule: a hash sent by peers is trusted once at least aichTrustIPs
// different networks (/20) sent it and at least aichTrustPct percent of
// all those that sent one.
const (
	aichTrustIPs = 10
	aichTrustPct = 92
	// aichMaxVoters bounds the addresses remembered per download.
	aichMaxVoters = 1000
)

// A part that failed waits aichWaitMax for recovery data (it is not
// downloaded again meanwhile); a peer asked has aichAskWait to answer
// before another one is.
var (
	aichWaitMax = 2 * time.Minute
	aichAskWait = 30 * time.Second
)

// aichWait is a part waiting for recovery data.
type aichWait struct {
	since   time.Time
	askedBy string // owner asked last
	askedAt time.Time
}

// aichState is the AICH part of a PartFile (guarded by its mutex).
type aichState struct {
	root    aich.Hash
	trusted bool
	votes   map[aich.Hash]map[uint32]bool // hashes peers sent, by the /20 that sent them
	voters  int
	wait    map[int]*aichWait
	waiting atomic.Int32 // len(wait), read without the lock
	// restored: parts restored with recovery data; if one fails its hash
	// again, the trusted AICH hash is wrong: off, AICH is not used again.
	restored map[int]bool
	off      bool
}

// net20 is eMule's "unique IP" for votes: the first 20 bits.
func net20(ip net.IP) (uint32, bool) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return uint32(v4[0])<<16 | uint32(v4[1])<<8 | uint32(v4[2]&0xF0), true
}

// AICHRoot returns the file's AICH hash, when trusted.
func (pf *PartFile) AICHRoot() (aich.Hash, bool) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.aich.root, pf.aich.trusted && !pf.aich.off
}

// NeedsAICHRoot reports whether peers should be asked for the AICH hash.
func (pf *PartFile) NeedsAICHRoot() bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return !pf.aich.trusted && !pf.aich.off && aich.CanRecover(uint64(pf.Size), 0)
}

// AICHVote records the AICH hash a peer at ip sent; it reports whether
// that made a hash trusted.
func (pf *PartFile) AICHVote(ip net.IP, root aich.Hash) bool {
	key, ok := net20(ip)
	if !ok {
		return false
	}
	pf.mu.Lock()
	defer pf.mu.Unlock()
	a := &pf.aich
	if a.trusted || a.off {
		return false
	}
	if a.votes == nil {
		a.votes = map[aich.Hash]map[uint32]bool{}
	}
	for h, ips := range a.votes {
		if ips[key] && h != root {
			return false // one network, one hash: it cannot vote for two
		}
	}
	if a.votes[root] == nil {
		a.votes[root] = map[uint32]bool{}
	}
	if !a.votes[root][key] {
		if a.voters >= aichMaxVoters {
			return false
		}
		a.votes[root][key] = true
		a.voters++
	}
	n := len(a.votes[root])
	if n < aichTrustIPs || n*100/a.voters < aichTrustPct {
		return false
	}
	a.root, a.trusted, a.votes = root, true, nil
	pf.dirty = true
	return true
}

// aichFailedLocked is called when part p fails its eD2K hash: with a
// trusted AICH hash, the part waits for recovery data.
func (pf *PartFile) aichFailedLocked(p int) {
	a := &pf.aich
	if a.restored[p] {
		// Restored blocks that the AICH hash called good, and the part
		// still fails: that hash is not the file's.
		a.off = true
		clear(a.wait)
		a.waiting.Store(0)
		return
	}
	if !a.trusted || a.off || !aich.CanRecover(uint64(pf.Size), p) {
		return
	}
	if a.wait == nil {
		a.wait = map[int]*aichWait{}
	}
	a.wait[p] = &aichWait{since: time.Now()}
	a.waiting.Store(int32(len(a.wait)))
}

// aichHeldLocked reports whether part p is waiting for recovery data (not
// to be downloaded meanwhile). A wait that took too long ends.
func (pf *PartFile) aichHeldLocked(p int, now time.Time) bool {
	w := pf.aich.wait[p]
	if w == nil {
		return false
	}
	if now.Sub(w.since) > aichWaitMax {
		pf.aichDropLocked(p)
		return false
	}
	return true
}

func (pf *PartFile) aichDropLocked(p int) {
	delete(pf.aich.wait, p)
	pf.aich.waiting.Store(int32(len(pf.aich.wait)))
}

// AICHAsk picks a part waiting for recovery data that the peer owner,
// which has the parts in avail (nil: all), can be asked for now.
func (pf *PartFile) AICHAsk(owner string, avail []bool) (int, aich.Hash, bool) {
	if pf.aich.waiting.Load() == 0 {
		return 0, aich.Hash{}, false
	}
	pf.mu.Lock()
	defer pf.mu.Unlock()
	now := time.Now()
	for p, w := range pf.aich.wait {
		if !pf.aichHeldLocked(p, now) || (avail != nil && (p >= len(avail) || !avail[p])) ||
			now.Sub(w.askedAt) < aichAskWait || pf.badPart[owner][p] > 0 || pf.banned[owner] {
			continue
		}
		w.askedBy, w.askedAt = owner, now
		return p, pf.aich.root, true
	}
	return 0, aich.Hash{}, false
}

// AICHNone is called when owner had no recovery data for part p: another
// peer is asked.
func (pf *PartFile) AICHNone(owner string) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	for _, w := range pf.aich.wait {
		if w.askedBy == owner {
			w.askedAt = time.Time{}
		}
	}
}

// AICHRecovered takes the recovery data owner sent for part p: the
// blocks on disk that match their hashes are kept, the others downloaded
// again. It returns how many blocks were kept, of how many.
func (pf *PartFile) AICHRecovered(owner string, p int, root aich.Hash, data []byte) (kept, total int, err error) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	w := pf.aich.wait[p]
	if w == nil || w.askedBy != owner || root != pf.aich.root || pf.f == nil {
		return 0, 0, fmt.Errorf("recovery data not asked for")
	}
	blocks, err := aich.Verify(uint64(pf.Size), p, root, data)
	if err != nil {
		w.askedAt = time.Time{} // ask another peer
		return 0, 0, err
	}
	pf.aichDropLocked(p)
	f := pf.f
	ours, err := aich.BlockHashes(uint64(pf.Size), p, func(b []byte, off int64) error {
		_, err := f.ReadAt(b, off)
		return err
	})
	if err != nil {
		return 0, len(blocks), err
	}
	pr := pf.partRange(p)
	var good []Range
	for i := range blocks {
		if ours[i] == blocks[i] {
			start := pr.Start + int64(i)*aich.BlockSize
			good = append(good, Range{start, min(start+aich.BlockSize, pr.End)})
		}
	}
	if len(good) == len(blocks) {
		// Every block matches the AICH hash, and the part does not match
		// its eD2K hash: the two do not describe the same file. Download
		// the part again whole, and use AICH no more.
		pf.aich.off = true
		return 0, len(blocks), fmt.Errorf("the AICH hash does not fit the file's eD2K hash")
	}
	if pf.aich.restored == nil {
		pf.aich.restored = map[int]bool{}
	}
	pf.aich.restored[p] = true
	for _, r := range good {
		pf.done.Add(r)
	}
	pf.dirty = true
	return len(good), len(blocks), nil
}

// sharedPart serves a download's trusted AICH hash too.
func (s sharedPart) AICHRoot() (aich.Hash, bool) { return s.pf.AICHRoot() }

// ---- session side ----

// askAICHRoot asks the peer for the file's AICH hash, if we do not trust
// one yet and it speaks AICH.
func (s *session) askAICHRoot() error {
	if s.hello.AICH() == 0 || !s.pf.NeedsAICHRoot() {
		return nil
	}
	return s.c.SendHash(proto.ProtoEMule, peer.OpAICHFileHashReq, s.pf.Hash)
}

// askRecovery asks the peer for the recovery data of a part waiting for
// it, if there is one it can give.
func (s *session) askRecovery() error {
	if s.hello.AICH() == 0 || s.pf.aich.waiting.Load() == 0 {
		return nil
	}
	p, root, ok := s.pf.AICHAsk(s.owner, s.avail())
	if !ok {
		return nil
	}
	s.m.cfg.Debug("%s [%s]: asking for AICH recovery data of part %d", s.pf.Name, s.owner, p)
	return s.c.Send(proto.ProtoEMule, peer.OpAICHRequest, peer.EncodeAICHRequest(s.pf.Hash, p, root))
}

// aichPacket handles the AICH answers of the peer; it reports whether p
// was one.
func (s *session) aichPacket(p *proto.Packet) bool {
	if p.Proto != proto.ProtoEMule {
		return false
	}
	switch p.Opcode {
	case peer.OpAICHFileHashAns:
		f, root, err := peer.DecodeAICHFileHash(p.Payload)
		if err == nil && f == s.pf.Hash && s.pf.AICHVote(s.c.RemoteIP(), root) {
			s.m.cfg.Log("%s: AICH hash %s agreed by enough peers: corrupt parts will be repaired block by block", s.pf.Name, root)
		}
		return true
	case peer.OpAICHAnswer:
		f, part, root, data, err := peer.DecodeAICHAnswer(p.Payload)
		if err != nil || f != s.pf.Hash {
			return true
		}
		if data == nil {
			s.pf.AICHNone(s.owner)
			return true
		}
		kept, total, err := s.pf.AICHRecovered(s.owner, part, root, data)
		if err != nil {
			s.m.cfg.Debug("%s [%s]: AICH recovery data of part %d: %v", s.pf.Name, s.owner, part, err)
			return true
		}
		s.m.cfg.Log("%s: part %d repaired with AICH: %d of %d blocks were good; %d to download again",
			s.pf.Name, part, kept, total, total-kept)
		return true
	}
	return false
}

// trustLinkAICH sets the AICH hash from the link a download was added
// with: trusted, as the link names the file.
func (pf *PartFile) trustLinkAICH(h aich.Hash) {
	if h.IsZero() {
		return
	}
	pf.aich.root, pf.aich.trusted = h, true
}
