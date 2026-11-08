package server

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"mymule/internal/proto"
)

func TestServerMetRoundTrip(t *testing.T) {
	in := []Info{
		{IP: net.IPv4(1, 2, 3, 4).To4(), Port: 4661, Name: "Test One", Users: 100},
		{IP: net.IPv4(5, 6, 7, 8).To4(), Port: 4242, Name: "Test Two", Description: "desc"},
	}
	var buf bytes.Buffer
	if err := WriteServerMet(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ParseServerMet(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Addr() != "1.2.3.4:4661" || out[0].Name != "Test One" ||
		out[0].Users != 100 || out[1].Description != "desc" {
		t.Fatalf("got %+v", out)
	}
}

func TestQueryEncode(t *testing.T) {
	b, err := Query{Keywords: "foo bar"}.encode()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x00, 0x01, 3, 0, 'f', 'o', 'o', 0x01, 3, 0, 'b', 'a', 'r'}
	if !bytes.Equal(b, want) {
		t.Fatalf("got % x", b)
	}
}

// fakeServer accepts one connection, checks the login and answers a search.
func fakeServer(t *testing.T, ln net.Listener) {
	c, err := ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()

	p, err := proto.ReadPacket(c)
	if err != nil || p.Opcode != proto.OpLoginRequest {
		t.Errorf("expected login, got %+v %v", p, err)
		return
	}
	var w proto.Writer
	w.Str16("welcome")
	c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpServerMessage, Payload: w.B}).Encode())
	w = proto.Writer{}
	w.U32(0x02000000) // HighID
	c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())

	p, err = proto.ReadPacket(c)
	if err != nil || p.Opcode != proto.OpSearchRequest {
		t.Errorf("expected search, got %+v %v", p, err)
		return
	}
	w = proto.Writer{}
	w.U32(1)
	w.Bytes(bytes.Repeat([]byte{0xAB}, 16))
	w.U32(0)
	w.U16(0)
	w.U32(3)
	w.WriteTag(proto.StrTag(proto.FTFileName, "ubuntu.iso"))
	w.WriteTag(proto.IntTag(proto.FTFileSize, 123456))
	w.WriteTag(proto.IntTag(proto.FTSources, 7))
	c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpSearchResult, Payload: w.B}).Encode())
	time.Sleep(100 * time.Millisecond)
}

func TestDialAndSearch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go fakeServer(t, ln)

	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Status().HighID {
		t.Fatalf("expected HighID, got %+v", c.Status())
	}
	res, _, err := c.Search(ctx, Query{Keywords: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Name != "ubuntu.iso" || res[0].Size != 123456 || res[0].Sources != 7 {
		t.Fatalf("results %+v", res)
	}
}

// Search results with an impossible size are dropped: downloading one
// used to allocate per part and kill the daemon (and again on restart).
func TestSearchResultsDropBadSizes(t *testing.T) {
	result := func(w *proto.Writer, id byte, lo, hi uint32) {
		w.Bytes([]byte{id, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
		w.U32(0)
		w.U16(0)
		w.U32(3)
		w.WriteTag(proto.StrTag(proto.FTFileName, "file.bin"))
		w.WriteTag(proto.IntTag(proto.FTFileSize, lo))
		w.WriteTag(proto.IntTag(proto.FTFileSizeHi, hi))
	}
	var w proto.Writer
	w.U32(4)
	result(&w, 1, 1000, 0)  // fine
	result(&w, 2, 0, 0)     // empty
	result(&w, 3, 0, 1<<28) // 2^60 bytes
	result(&w, 4, 1, 64)    // just over 256 GiB
	res, _, err := parseSearchResults(w.B)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Hash[0] != 1 {
		t.Fatalf("got %d results, want only the 1000-byte one: %+v", len(res), res)
	}
}

// Names and server texts come from other people: control and invisible
// characters must not reach the terminal or the log.
func TestNetworkTextIsCleaned(t *testing.T) {
	var w proto.Writer
	w.U32(1)
	w.Bytes(make([]byte, 16))
	w.U32(0)
	w.U16(0)
	w.U32(2)
	w.WriteTag(proto.StrTag(proto.FTFileName, "Film\x1b]52;c;cm0gLXJm\x07\u202evkm.exe"))
	w.WriteTag(proto.IntTag(proto.FTFileSize, 1000))
	res, _, err := parseSearchResults(w.B)
	if err != nil || len(res) != 1 {
		t.Fatalf("%v %v", res, err)
	}
	if res[0].Name != "Film]52;c;cm0gLXJmvkm.exe" {
		t.Errorf("search result name: %q", res[0].Name)
	}
	info := infoFromTags(nil, 4661, proto.TagList{proto.StrTag(proto.STServerName, "Srv\x1b[2J\nfake line")})
	if info.Name != "Srv[2J fake line" {
		t.Errorf("server name: %q", info.Name)
	}
}

// What a server supports survives a save of server.met.
func TestServerMetKeepsCapabilities(t *testing.T) {
	in := []Info{{IP: net.IPv4(192, 0, 2, 1).To4(), Port: 4661, UDPFlags: SrvUDPGetSources2 | 0x10,
		Version: 0x11, MaxUsers: 500, Preference: 1, ObfuTCPPort: 4663}}
	var buf bytes.Buffer
	if err := WriteServerMet(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ParseServerMet(buf.Bytes())
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
	o := out[0]
	if o.UDPFlags != in[0].UDPFlags || o.Version != 0x11 || o.MaxUsers != 500 || o.Preference != 1 || o.ObfuTCPPort != 4663 {
		t.Fatalf("got %+v", o)
	}
}

// Our client ID is set once per connection: later changes are ignored,
// except a LowID becoming a HighID.
func TestIDChangeOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []uint32
		want uint32
	}{
		{"high then other high", []uint32{0x02000000, 0x05060708}, 0x02000000},
		{"high then low", []uint32{0x02000000, 5}, 0x02000000},
		{"low then high", []uint32{5, 0x02000000}, 0x02000000},
		{"low then other low", []uint32{5, 6}, 5},
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		sent := make(chan struct{})
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			proto.ReadPacket(c)
			for _, id := range tc.ids {
				var w proto.Writer
				w.U32(id)
				c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())
			}
			var w proto.Writer
			w.Str16("done")
			c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpServerMessage, Payload: w.B}).Encode())
			close(sent)
			time.Sleep(time.Second)
		}()
		addr := ln.Addr().(*net.TCPAddr)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var logs []string
		var mu sync.Mutex
		c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test", Log: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			mu.Unlock()
		}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		<-sent
		deadline := time.Now().Add(3 * time.Second)
		for {
			mu.Lock()
			done := len(logs) > 0 && strings.Contains(logs[len(logs)-1], "done")
			mu.Unlock()
			if done || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got := c.Status().ClientID; got != tc.want {
			t.Errorf("%s: ID %d, want %d", tc.name, got, tc.want)
		}
		c.Close()
		ln.Close()
	}
}

// A server sending messages without end makes us log maxServerLogs lines a
// minute; the rest are counted and said once the minute is over.
func TestServerLogLimited(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	more := make(chan bool)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		proto.ReadPacket(c)
		send := func(op byte, w proto.Writer) {
			c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: op, Payload: w.B}).Encode())
		}
		var w proto.Writer
		w.U32(0x02000000)
		send(proto.OpIDChange, w)
		for i := range 200 {
			w = proto.Writer{}
			w.Str16(fmt.Sprintf("spam %d", i))
			send(proto.OpServerMessage, w)
			send(0x7E, proto.Writer{}) // unknown
		}
		<-more
		w = proto.Writer{}
		w.Str16("after a minute")
		send(proto.OpServerMessage, w)
		time.Sleep(time.Second)
	}()
	var mu sync.Mutex
	var logs []string
	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test", Log: func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	n := len(logs)
	mu.Unlock()
	if n > maxServerLogs+2 {
		t.Fatalf("%d lines logged", n)
	}
	c.mu.Lock()
	c.logWindow = c.logWindow.Add(-time.Minute)
	c.mu.Unlock()
	more <- true
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		last := strings.Join(logs, "\n")
		mu.Unlock()
		if strings.Contains(last, "after a minute") {
			if !strings.Contains(last, "more lines from it not logged") {
				t.Fatal("the lines left out are not said")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no line logged after the minute")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
