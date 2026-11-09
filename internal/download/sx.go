package download

import (
	"net"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/netutil"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/server"
)

// Source exchange (see package peer): a peer downloading a file we are
// downloading too asks us where else it is, and we tell it the sources we
// know work; and we ask the peers we download from.
//
// What we give is limited to what helps and costs little: HighID sources
// with a public address that answered us (queued us, uploaded to us, or
// have the file but no part we lack), at most sxMaxAnswer, each peer once
// per sxAnswerEvery for a file (eMule's 40 minutes), and sxAnswersPerMin
// answers a minute in all.
const (
	sxAnswerEvery   = 40 * time.Minute
	sxMaxAnswer     = 100
	sxAnswersPerMin = 60
	maxSXRemembered = 10000
)

type sxKey struct {
	ip   string
	file ed2k.Hash
}

// sxState is what source exchange remembers (under Manager.mu).
type sxState struct {
	answered        map[sxKey]time.Time // when we answered a peer for a file
	minute          time.Time
	answersInMinute int
}

// sxPacket handles a source exchange packet; it reports whether p was
// one.
func (m *Manager) sxPacket(c *peer.Conn, hello peer.Hello, p *proto.Packet) bool {
	if p.Proto != proto.ProtoEMule || (p.Opcode != peer.OpRequestSources && p.Opcode != peer.OpRequestSources2) {
		return false
	}
	file, version, ok := peer.DecodeRequestSources(p, hello.SourceExchange1())
	if !ok || (p.Opcode == peer.OpRequestSources && version < 2) {
		return true // eMule only asks with the first form above version 1
	}
	srcs := m.sxSourcesFor(file, c.RemoteIP(), time.Now())
	if len(srcs) == 0 {
		return true // nothing to say, as eMule
	}
	if p.Opcode == peer.OpRequestSources2 {
		c.Send(proto.ProtoEMule, peer.OpAnswerSources2, peer.EncodeAnswerSources2(version, file, srcs))
	} else {
		c.Send(proto.ProtoEMule, peer.OpAnswerSources, peer.EncodeAnswerSources(version, file, srcs))
	}
	return true
}

// sxSourcesFor returns the sources of file to give the peer at ip, or
// none when it asked too recently (or too many peers ask).
func (m *Manager) sxSourcesFor(file ed2k.Hash, ip net.IP, now time.Time) []peer.SXSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[file]
	if f == nil || f.finished || ip == nil {
		return nil
	}
	st := &m.sx
	key := sxKey{ip.String(), file}
	if at, ok := st.answered[key]; ok && now.Sub(at) < sxAnswerEvery {
		return nil
	}
	if now.Sub(st.minute) >= time.Minute {
		st.minute, st.answersInMinute = now, 0
	}
	if st.answersInMinute >= sxAnswersPerMin {
		return nil
	}
	var out []peer.SXSource
	for _, src := range f.sources {
		if len(out) == sxMaxAnswer {
			break
		}
		if src.IP == nil || src.LowID != 0 || src.kadFW != nil || src.Port == 0 || src.Fails > 0 ||
			src.IP.Equal(ip) || !netutil.IsPublicIPv4(src.IP) {
			continue
		}
		switch src.State {
		case StateQueued, StateDownloading, StateNoNeededParts:
		default:
			continue // not known to have the file
		}
		out = append(out, peer.SXSource{ID: server.IPToU32(src.IP), Port: src.Port, ServerIP: net.IPv4zero,
			UserHash: src.UserHash, Crypt: src.Crypt})
	}
	if len(out) == 0 {
		return nil
	}
	if st.answered == nil {
		st.answered = map[sxKey]time.Time{}
	}
	if len(st.answered) >= maxSXRemembered {
		for k, at := range st.answered {
			if now.Sub(at) >= sxAnswerEvery {
				delete(st.answered, k)
			}
		}
		if len(st.answered) >= maxSXRemembered {
			return nil
		}
	}
	st.answered[key] = now
	st.answersInMinute++
	return out
}
