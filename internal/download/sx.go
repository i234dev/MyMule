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

// What we ask for, and take: a peer is asked once per sxAnswerEvery for a
// file, only while the file has fewer than sxWantBelow sources, and only
// on a connection where it has parts we need (the session then lasts long
// enough for the answer). From an answer we take at most sxMaxTake HighID
// sources, which go through the same checks as any other source.
const (
	sxWantBelow  = MaxSourcesPerFile * 4 / 5
	sxMaxTake    = 50
	sxAsksPerMin = 60
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

	asked        map[sxKey]time.Time // when we asked a peer for a file's sources
	askMinute    time.Time
	asksInMinute int
}

// remember notes key at now in m (made if nil), keeping it at most
// maxSXRemembered long: what is older than sxAnswerEvery goes first. It
// reports false when full.
func remember(m *map[sxKey]time.Time, key sxKey, now time.Time) bool {
	if *m == nil {
		*m = map[sxKey]time.Time{}
	}
	if len(*m) >= maxSXRemembered {
		for k, at := range *m {
			if now.Sub(at) >= sxAnswerEvery {
				delete(*m, k)
			}
		}
		if len(*m) >= maxSXRemembered {
			return false
		}
	}
	(*m)[key] = now
	return true
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
	// Counted before looking, whatever we find: a peer asking over and
	// over must not make us go through the sources each time.
	if !remember(&st.answered, key, now) {
		return nil
	}
	st.answersInMinute++
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
	return out
}

// sxShouldAsk reports whether to ask the peer at ip for more sources of
// f now, and notes that we do.
func (m *Manager) sxShouldAsk(f *File, ip net.IP, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ip == nil || f.finished || len(f.sources) >= sxWantBelow {
		return false
	}
	st := &m.sx
	key := sxKey{ip.String(), f.pf.Hash}
	if at, ok := st.asked[key]; ok && now.Sub(at) < sxAnswerEvery {
		return false
	}
	if now.Sub(st.askMinute) >= time.Minute {
		st.askMinute, st.asksInMinute = now, 0
	}
	if st.asksInMinute >= sxAsksPerMin || !remember(&st.asked, key, now) {
		return false
	}
	st.asksInMinute++
	return true
}

// askSources asks the session's peer for sources of its file, if it does
// source exchange and it is time to.
func (s *session) askSources() error {
	h := s.hello
	if !h.SourceExchange() || !s.m.sxShouldAsk(s.f, s.c.RemoteIP(), time.Now()) {
		return nil
	}
	// eMule compresses answers above 354 bytes whatever we announce:
	// taken while our request is pending, and no larger than an answer
	// can be.
	s.c.Packed = func(op byte) int {
		if s.sxVersion != 0 && (op == peer.OpAnswerSources || op == peer.OpAnswerSources2) {
			return peer.MaxSXAnswerBytes
		}
		return 0
	}
	if h.SourceExchange2() {
		s.sxVersion, s.sxSecond = peer.SXVersion, true
		return s.c.Send(proto.ProtoEMule, peer.OpRequestSources2, peer.EncodeRequestSources2(s.pf.Hash))
	}
	s.sxVersion, s.sxSecond = min(h.SourceExchange1(), peer.SXVersion), false
	return s.c.SendHash(proto.ProtoEMule, peer.OpRequestSources, s.pf.Hash)
}

// onSourcesAnswer takes the answer to our request on this session; one
// that we did not ask for (or a second one) is ignored.
func (s *session) onSourcesAnswer(p *proto.Packet) {
	if s.sxVersion == 0 || (p.Opcode == peer.OpAnswerSources2) != s.sxSecond {
		return
	}
	var file ed2k.Hash
	var srcs []peer.SXSource
	var err error
	if s.sxSecond {
		_, file, srcs, err = peer.DecodeAnswerSources2(p.Payload)
	} else {
		file, srcs, err = peer.DecodeAnswerSources(p.Payload, s.sxVersion)
	}
	if err != nil || file != s.pf.Hash {
		return
	}
	s.sxVersion = 0 // one answer per request
	var add []server.Source
	for _, x := range srcs {
		if len(add) == sxMaxTake {
			break
		}
		if x.ID < server.LowIDLimit || x.Port == 0 {
			continue // LowIDs: only reachable through their server
		}
		add = append(add, server.Source{ID: x.ID, Port: x.Port, Crypt: x.Crypt, UserHash: x.UserHash})
	}
	m := s.m
	m.mu.Lock()
	var our uint32
	if m.srv != nil {
		our = m.srv.Status().ClientID
	}
	m.mu.Unlock()
	if n := m.addSources(s.f, add, "source exchange", our, false); n > 0 {
		m.cfg.Log("%s: %d new sources from %s (source exchange)", s.pf.Name, n, s.src.key)
	}
}
