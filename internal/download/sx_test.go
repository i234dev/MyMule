package download

import (
	"net"
	"strconv"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// sxManager returns a manager downloading one file, with the sources
// given (key "a.b.c.d:port" -> state).
func sxManager(t *testing.T, states map[string]SourceState) (*Manager, *File) {
	t.Helper()
	pf, _, _ := blameSetup(t)
	m := NewManager(Config{})
	f := &File{pf: pf, sources: map[string]*Source{}}
	m.files = map[ed2k.Hash]*File{pf.Hash: f}
	for key, st := range states {
		host, port, _ := net.SplitHostPort(key)
		p, _ := strconv.Atoi(port)
		f.sources[key] = &Source{key: key, IP: net.ParseIP(host).To4(), Port: uint16(p), State: st, file: f,
			UserHash: ed2k.Hash{byte(p)}, Crypt: 1}
	}
	return m, f
}

// We give the sources known to have the file, with a public address,
// HighID, not the asker itself; nothing for a file we do not download.
func TestSXAnswerPicksGoodSources(t *testing.T) {
	m, f := sxManager(t, map[string]SourceState{
		"1.0.0.1:4662":  StateQueued,
		"1.0.0.2:4662":  StateDownloading,
		"1.0.0.3:4662":  StateNoNeededParts,
		"1.0.0.4:4662":  StateNoFile,
		"1.0.0.5:4662":  StateUnreachable,
		"1.0.0.6:4662":  StateNew,
		"10.0.0.7:4662": StateQueued, // private
		"1.0.0.9:4662":  StateQueued, // the asker
	})
	f.sources["1.0.0.8:4662"] = &Source{key: "1.0.0.8:4662", IP: net.ParseIP("1.0.0.8").To4(), Port: 4662, State: StateQueued, Fails: 1, file: f}
	f.sources["lowid:5@srv"] = &Source{key: "lowid:5@srv", LowID: 5, State: StateQueued, file: f}
	got := m.sxSourcesFor(f.pf.Hash, net.ParseIP("1.0.0.9"), time.Now())
	want := map[string]bool{"1.0.0.1": true, "1.0.0.2": true, "1.0.0.3": true}
	if len(got) != len(want) {
		t.Fatalf("%d sources: %+v", len(got), got)
	}
	for _, s := range got {
		ip := net.IPv4(byte(s.ID), byte(s.ID>>8), byte(s.ID>>16), byte(s.ID>>24)).String()
		if !want[ip] || s.Port != 4662 || s.UserHash != (ed2k.Hash{byte(4662 & 0xFF)}) || s.Crypt != 1 {
			t.Fatalf("source %s %+v", ip, s)
		}
	}
	if got := m.sxSourcesFor(ed2k.Hash{1}, net.ParseIP("198.51.100.10"), time.Now()); got != nil {
		t.Fatal("sources for a file we do not download")
	}
	f.finished = true
	if got := m.sxSourcesFor(f.pf.Hash, net.ParseIP("198.51.100.11"), time.Now()); got != nil {
		t.Fatal("sources for a finished download")
	}
}

// Each peer once per sxAnswerEvery for a file, sxAnswersPerMin answers a
// minute in all, sxMaxAnswer sources at most.
func TestSXAnswerLimits(t *testing.T) {
	states := map[string]SourceState{}
	for i := range sxMaxAnswer + 20 {
		states["1.0."+strconv.Itoa(i/250)+"."+strconv.Itoa(1+i%250)+":4662"] = StateQueued
	}
	m, f := sxManager(t, states)
	now := time.Now()
	asker := net.ParseIP("198.51.100.9")
	if got := m.sxSourcesFor(f.pf.Hash, asker, now); len(got) != sxMaxAnswer {
		t.Fatalf("%d sources in one answer", len(got))
	}
	if got := m.sxSourcesFor(f.pf.Hash, asker, now.Add(sxAnswerEvery-time.Minute)); got != nil {
		t.Fatal("answered the same peer again too soon")
	}
	if got := m.sxSourcesFor(f.pf.Hash, asker, now.Add(sxAnswerEvery+time.Second)); got == nil {
		t.Fatal("not answered after sxAnswerEvery")
	}
	later := now.Add(2 * sxAnswerEvery)
	answered := 0
	for i := range sxAnswersPerMin + 10 {
		if m.sxSourcesFor(f.pf.Hash, net.IPv4(198, 51, 101, byte(i)), later) != nil {
			answered++
		}
	}
	if answered != sxAnswersPerMin {
		t.Fatalf("%d answers in a minute", answered)
	}
}

// Over a connection: a request of the second form gets an answer of the
// second form, in the version asked (up to ours); of the first form, in
// the peer's first-form version. A peer at version 1 of the first form
// is not answered (eMule asks only above 1).
func TestSXAnswerOverConnection(t *testing.T) {
	m, f := sxManager(t, map[string]SourceState{"1.0.0.1:4662": StateQueued})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, tc := range []struct {
		op      byte
		payload []byte
		v1      byte
		wantOp  byte // 0: no answer
		version byte
	}{
		{peer.OpRequestSources2, peer.EncodeRequestSources2(f.pf.Hash), 0, peer.OpAnswerSources2, peer.SXVersion},
		{peer.OpRequestSources, f.pf.Hash[:], 3, peer.OpAnswerSources, 3},
		{peer.OpRequestSources, f.pf.Hash[:], 1, 0, 0},
	} {
		m.sx.answered = nil
		got := make(chan *proto.Packet, 1)
		go func() {
			nc, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				got <- nil
				return
			}
			c := peer.NewConn(nc)
			defer c.Close()
			c.Send(proto.ProtoEMule, tc.op, tc.payload)
			p, _ := c.Read(time.Second)
			got <- p
		}()
		nc, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		c := peer.NewConn(nc)
		p, err := c.Read(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		hello := peer.Hello{Tags: proto.TagList{proto.IntTag(peer.CTEMuleMiscOpts1, uint32(tc.v1)<<12)}}
		if !m.sxPacket(c, hello, p) {
			t.Fatal("not handled")
		}
		ans := <-got
		c.Close()
		if tc.wantOp == 0 {
			if ans != nil {
				t.Fatalf("answered a version 1 peer: 0x%02X", ans.Opcode)
			}
			continue
		}
		if ans == nil || ans.Proto != proto.ProtoEMule || ans.Opcode != tc.wantOp {
			t.Fatalf("answer %+v", ans)
		}
		var srcs []peer.SXSource
		if ans.Opcode == peer.OpAnswerSources2 {
			var v byte
			v, _, srcs, err = peer.DecodeAnswerSources2(ans.Payload)
			if v != tc.version {
				t.Fatalf("version %d", v)
			}
		} else {
			_, srcs, err = peer.DecodeAnswerSources(ans.Payload, tc.version)
		}
		if err != nil || len(srcs) != 1 || srcs[0].Port != 4662 {
			t.Fatalf("%v %+v", err, srcs)
		}
	}
}
