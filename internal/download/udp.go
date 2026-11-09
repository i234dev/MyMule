package download

import (
	"net"
	"net/netip"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/upload"
)

// Client UDP (see package peer): while we wait in a source's queue, we
// ask for our place with a datagram instead of a new TCP connection, as
// eMule does; and we answer the peers waiting in ours.
//
// A source is asked by UDP when it offers it, it queued us before, it is
// HighID and so are we (its answer must reach us). No answer within
// udpWait: TCP, as before; after udpMaxFails of those in a row, TCP only
// for that source. Answers are taken only from the address and port we
// asked, within udpWait.
const (
	udpWait     = 30 * time.Second
	udpMaxFails = 3
)

// udpReaskable reports whether src can be asked by UDP now (m.mu held;
// high: we have a HighID).
func (m *Manager) udpReaskable(src *Source, high bool) bool {
	return m.cfg.SendClientUDP != nil && high && src.udpPort != 0 && src.udpVer > 0 &&
		src.IP != nil && src.LowID == 0 && src.kadFW == nil && src.State == StateQueued &&
		src.udpFails < udpMaxFails
}

type udpAsk struct {
	to       netip.AddrPort
	payload  []byte
	userHash ed2k.Hash
}

// udpAskLocked prepares the UDP reask of src (m.mu held).
func (m *Manager) udpAskLocked(src *Source, now time.Time) udpAsk {
	f := src.file
	status := upload.StatusBits(f.pf.Size, f.pf.VerifiedParts())
	ip, _ := netip.AddrFromSlice(src.IP.To4())
	a := udpAsk{to: netip.AddrPortFrom(ip, src.udpPort),
		payload: peer.EncodeReaskFilePing(src.udpVer, f.pf.Hash, status, 0)}
	if src.Crypt&1 != 0 && m.cfg.Obfuscation&1 != 0 {
		a.userHash = src.UserHash
	}
	src.udpAskAt = now
	src.NextAsk = now.Add(udpWait) // then TCP, if it did not answer
	return a
}

// OnClientUDP handles an eD2K client UDP packet that came to our client
// UDP port.
func (m *Manager) OnClientUDP(from netip.AddrPort, op byte, payload []byte) {
	ip := net.IP(from.Addr().Unmap().AsSlice())
	if op == peer.OpReaskFilePing {
		up, send := m.cfg.Upload, m.cfg.SendClientUDP
		if up == nil || send == nil {
			return
		}
		if r, ok := up.ReaskPing(ip, from.Port(), payload); ok {
			send(from, r.Op, r.Payload, r.UserHash)
		}
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		for key, src := range f.sources {
			if src.udpAskAt.IsZero() || now.Sub(src.udpAskAt) > udpWait ||
				!src.IP.Equal(ip) || src.udpPort != from.Port() {
				continue
			}
			src.udpAskAt, src.udpFails = time.Time{}, 0
			switch op {
			case peer.OpReaskAck:
				avail, rank, err := peer.DecodeReaskAck(payload, src.udpVer)
				if err != nil {
					continue
				}
				if avail != nil {
					src.Avail = avail[:min(len(avail), NumParts(f.pf.Size))]
				}
				src.Rank = int(rank)
				src.NextAsk = now.Add(ReaskInterval)
			case peer.OpQueueFull:
				src.NextAsk = now.Add(ReaskInterval)
			case peer.OpFileNotFound:
				m.cfg.Debug("%s [%s]: no longer has the file (UDP)", f.pf.Name, key)
				src.State = StateNoFile
			}
		}
	}
}
