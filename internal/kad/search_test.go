package kad

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"testing"
	"time"

	"mymule/internal/proto"
)

func TestWordsAndKeywordID(t *testing.T) {
	got := Words("Quiet.Orchard (2019) [x] of-the BLUE harbor quiet", true)
	want := []string{"quiet", "orchard", "2019", "the", "blue", "harbor"}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q", got)
		}
	}
	if n := len(Words("quiet quiet", false)); n != 2 {
		t.Fatalf("without dedupe: %d", n)
	}
	// MD4("abc"): eMule hashes the lowercase UTF-8 keyword.
	id := KeywordID("ABC")
	if hex.EncodeToString(id[:]) != "a448017aaf21d8525fc10ae87aa6729d" {
		t.Fatalf("KeywordID = %s", id)
	}
}

func TestKeywordSearchTree(t *testing.T) {
	q := Query{Keywords: "blue orchard 2", MinSize: 5 << 30, MaxSize: 100, Type: "Video", Extension: ".mkv"}
	cands, err := keywordCandidates(q)
	if err != nil || cands[0] != "orchard" || len(cands) != 2 {
		t.Fatalf("%q %v", cands, err)
	}
	words, terms := keywordSearch(q, "orchard")
	if len(words) != 3 {
		t.Fatalf("words %q", words)
	}
	// Written out byte by byte, as eMule's CreateSearchExpressionTree
	// reads it: AND "blue 2" AND type AND extension AND size>= size<=.
	want := []byte{
		0x00, 0x00, 0x01, 6, 0, 'b', 'l', 'u', 'e', ' ', '2',
		0x00, 0x00, 0x02, 5, 0, 'V', 'i', 'd', 'e', 'o', 1, 0, 0x03,
		0x00, 0x00, 0x02, 3, 0, 'm', 'k', 'v', 1, 0, 0x04,
		0x00, 0x00, 0x08, 0, 0, 0, 0x40, 1, 0, 0, 0, 0x03, 1, 0, 0x02,
		0x03, 100, 0, 0, 0, 0x04, 1, 0, 0x02,
	}
	if !bytes.Equal(terms, want) {
		t.Fatalf("tree\n% X\nwant\n% X", terms, want)
	}
	// One word: nothing to filter on.
	if _, terms := keywordSearch(Query{Keywords: "harbor"}, "harbor"); len(terms) != 0 {
		t.Fatalf("% X", terms)
	}
	// Archives and CD images are published as programs.
	for _, typ := range []string{"Arc", "Iso"} {
		_, terms := keywordSearch(Query{Keywords: "harbor", Type: typ}, "harbor")
		if !bytes.Contains(terms, []byte("Pro")) {
			t.Fatalf("%s: % X", typ, terms)
		}
	}
	if _, err := keywordCandidates(Query{Keywords: "a b"}); err == nil {
		t.Fatal("search without a 3-letter word accepted")
	}
	if _, err := keywordCandidates(Query{Keywords: string(make([]byte, maxQueryBytes+1))}); err == nil {
		t.Fatal("huge search accepted")
	}
}

// setID gives a test node another Kad ID.
func setID(n *Node, id ID) {
	n.keys.KadID = id
	n.table = NewTable(id, t0)
	n.table.MyIP = n.publicIP
}

// valueSearch makes A search target through B, a node close to the
// target, until A sends B the search request.
func valueSearch(t *testing.T, target ID, start func(a *Node)) (*fakeNet, *Node, *Node) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	near := target
	near[15] ^= 1
	setID(b, near)
	a.table.Add(contactOf(b), false, false, t0)
	a.table.SetAlive(b.ID(), t0)
	start(a)
	fn.pump()
	for range 5 {
		fn.now = fn.now.Add(time.Second)
		a.mu.Lock()
		a.tickSearches(fn.now)
		a.mu.Unlock()
		fn.pump()
	}
	a.mu.Lock()
	_, asked := a.searches[target].asked[addrOf(b)]
	a.mu.Unlock()
	if !asked {
		t.Fatal("search request not sent to the close node")
	}
	return fn, a, b
}

// lastSent returns the last packet with op sent to addr.
func lastSent(fn *fakeNet, from, addr netip.AddrPort, op byte, keys *Keys) (Packet, bool) {
	var p Packet
	ok := false
	for _, dg := range fn.all {
		if dg.from != from || dg.to != addr {
			continue
		}
		r, err := keys.Decrypt(dg.b, from.Addr())
		if err != nil {
			continue
		}
		if q, err := DecodePacket(r.Data); err == nil && q.Opcode == op {
			p, ok = q, true
		}
	}
	return p, ok
}

// fromNode obfuscates a datagram from node b to node a with the verify key
// a gave b, as nodes answer requests.
func fromNode(a, b *Node, d []byte) []byte {
	return b.keys.EncryptKad(d, addrOf(a).Addr(), ID{}, a.keys.VerifyKey(addrOf(b).Addr()))
}

func searchRes(sender, target ID, answers ...func(w *proto.Writer)) []byte {
	var w proto.Writer
	WriteID(&w, sender)
	WriteID(&w, target)
	w.U16(uint16(len(answers)))
	for _, a := range answers {
		a(&w)
	}
	return rawPacket(OpSearchRes, w.B)
}

func sourceAnswer(user ID, typ byte, ip netip.Addr, port uint16) func(*proto.Writer) {
	return func(w *proto.Writer) {
		WriteID(w, user)
		WriteTags(w, []proto.Tag{IntTag(TagSourceType, uint64(typ)), IntTag(TagSourceIP, uint64(ipToU32(ip))),
			IntTag(TagSourcePort, uint64(port)), IntTag(TagSourceUDPPort, 4672)})
	}
}

func TestFindSources(t *testing.T) {
	target := RandomID()
	var got []Source
	fn, a, b := valueSearch(t, target, func(a *Node) {
		if err := a.FindSources(target, 12345, func(s Source) { got = append(got, s) }); err != nil {
			t.Fatal(err)
		}
	})
	_ = fn
	u1, u2, u3 := RandomID(), RandomID(), RandomID()
	ip := pubIP(77, false)
	fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), target,
		sourceAnswer(u1, 1, ip, 4662),
		sourceAnswer(u2, 2, ip, 4662), // type 2: not used
		sourceAnswer(u3, 3, ip, 4662),
		sourceAnswer(u1, 1, ip, 4662), // again: reported once
	)))
	a.runDeferred()
	if len(got) != 2 || got[0].UserHash != u1 || !got[0].Direct() || got[0].IP != ip || got[0].TCPPort != 4662 || got[0].UDPPort != 4672 {
		t.Fatalf("sources %+v", got)
	}
	if got[1].Direct() {
		t.Fatal("firewalled source counted as direct")
	}
	// From a node we did not ask, or for another target: ignored.
	x := netip.AddrPortFrom(pubIP(90, false), 4672)
	fn.inject(x, a, searchRes(RandomID(), target, sourceAnswer(RandomID(), 1, ip, 1)))
	fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), RandomID(), sourceAnswer(RandomID(), 1, ip, 1))))
	// From the asked node's address but without our key: maybe forged.
	fn.inject(addrOf(b), a, searchRes(b.ID(), target, sourceAnswer(RandomID(), 1, ip, 2)))
	a.runDeferred()
	if len(got) != 2 {
		t.Fatalf("%d sources after unrequested answers", len(got))
	}
	if err := a.FindSources(target, 1, nil); err != ErrBusy {
		t.Fatalf("second search: %v", err)
	}
	empty := newFakeNet().node(t, 5, Config{})
	if err := empty.FindSources(target, 1, nil); err != ErrNotConnected {
		t.Fatalf("without contacts: %v", err)
	}
}

// The source request names the file and its size.
func TestFindSourcesRequest(t *testing.T) {
	target := RandomID()
	fn, a, b := valueSearch(t, target, func(a *Node) { a.FindSources(target, 1<<33, func(Source) {}) })
	_ = a
	p, ok := lastSent(fn, addrOf(a), addrOf(b), OpSearchSourceReq, &b.keys)
	if !ok {
		t.Fatal("no source request")
	}
	r := proto.NewReader(p.Payload)
	if ReadID(r) != target || r.U16() != 0 || r.U64() != 1<<33 || r.Len() != 0 {
		t.Fatalf("request % X", p.Payload)
	}
}

func keywordAnswer(h ID, name string, size uint64, bsob bool) func(*proto.Writer) {
	return func(w *proto.Writer) {
		WriteID(w, h)
		sz := IntTag(TagFileSize, size)
		if bsob {
			var b proto.Writer
			b.U64(size)
			sz = proto.Tag{ID: TagFileSize, Type: proto.TagBsob, Bytes: b.B}
		}
		WriteTags(w, []proto.Tag{StrTag(TagFileName, name), sz, StrTag(TagFileType, "Video"), IntTag(TagSources, 7)})
	}
}

func TestSearchKeyword(t *testing.T) {
	q := Query{Keywords: "quiet orchard"}
	target := KeywordID("orchard")
	results := make(chan KeywordResult, 10)
	errc := make(chan error, 1)
	fn, a, b := valueSearch(t, target, func(n *Node) {
		go func() {
			errc <- n.SearchKeyword(context.Background(), q, func(r KeywordResult) { results <- r })
		}()
		for i := 0; i < 2000; i++ {
			n.mu.Lock()
			s := n.searches[target]
			n.mu.Unlock()
			if s != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("search not started")
	})
	// The request is restrictive and carries the other word.
	p, ok := lastSent(fn, addrOf(a), addrOf(b), OpSearchKeyReq, &b.keys)
	if !ok {
		t.Fatal("no keyword request")
	}
	r := proto.NewReader(p.Payload)
	if ReadID(r) != target || r.U16() != 0x8000 || r.U8() != seString || r.Str16() != "quiet" || r.Len() != 0 {
		t.Fatalf("request % X", p.Payload)
	}

	h1, h2, h3, h4 := RandomID(), RandomID(), RandomID(), RandomID()
	fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), target,
		keywordAnswer(h1, "Quiet.Orchard.2019.mkv", 1000, false),
		keywordAnswer(h2, "Orchard of Bright Comets.avi", 2000, false), // lacks "quiet"
		keywordAnswer(h3, "QUIET ORCHARD.iso", 5<<32, true),            // size as a blob
		keywordAnswer(h4, "Quiet Orchard no size", 0, false),
	)))
	a.runDeferred()
	var got []KeywordResult
	for len(got) < 2 {
		select {
		case r := <-results:
			got = append(got, r)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d results", len(got))
		}
	}
	if got[0].Hash != h1 || got[0].Size != 1000 || got[0].Type != "Video" || got[0].Sources != 7 || got[1].Hash != h3 || got[1].Size != 5<<32 {
		t.Fatalf("%+v", got)
	}
	select {
	case r := <-results:
		t.Fatalf("unexpected result %+v", r)
	default:
	}
	// The search ends after its lifetime; SearchKeyword returns.
	fn.now = fn.now.Add(time.Minute)
	a.mu.Lock()
	a.tickSearches(fn.now)
	a.mu.Unlock()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SearchKeyword did not return")
	}
}

// Cancelling the caller's context ends the search; later answers are not
// reported.
func TestSearchKeywordCancel(t *testing.T) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	a.table.Add(contactOf(b), false, false, t0)
	a.table.SetAlive(b.ID(), t0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.SearchKeyword(ctx, Query{Keywords: "harbor"}, func(KeywordResult) {}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped")
	}
	a.mu.Lock()
	left := len(a.searches)
	a.mu.Unlock()
	if left != 0 {
		t.Fatal("search left running")
	}
}

// Nodes too far from the target are not asked for the value.
func TestSearchTolerance(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	target := RandomID()
	s := newSearch(searchFile, target, fn.now)
	c := contactN(9)
	far := target
	far[0] ^= 0x80
	c.ID = far
	a.storePacket(s, far.Xor(target), c, fn.now)
	if len(s.asked) != 0 {
		t.Fatal("far node asked")
	}
	near := target
	near[3] ^= 0xFF
	c.ID = near
	a.storePacket(s, near.Xor(target), c, fn.now)
	if _, ok := s.asked[c.Addr()]; !ok {
		t.Fatal("near node not asked")
	}
}

// Random search answers for a running search never panic.
func TestSearchResHostile(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	for _, typ := range []searchType{searchFile, searchKeyword} {
		target := RandomID()
		s := newSearch(typ, target, fn.now)
		s.onSource = func(Source) {}
		s.onResult = func(KeywordResult) {}
		s.words = []string{"abc"}
		a.searches[target] = s
		from := netip.AddrPortFrom(pubIP(60, false), 4672)
		s.asked[from] = Contact{IP: from.Addr(), UDPPort: from.Port(), Version: 5}
		buf := make([]byte, 300)
		for i := range 5000 {
			for j := range buf {
				buf[j] = byte(i*31 + j*j*7 + j>>3)
			}
			cryptoRand(buf[:i%len(buf)])
			var w proto.Writer
			WriteID(&w, RandomID())
			WriteID(&w, target)
			w.Bytes(buf[:i%len(buf)])
			op := byte(OpSearchRes)
			if i%2 == 0 {
				op = OpSearchResV1
				w.B = w.B[16:]
			}
			fn.inject(from, a, rawPacket(op, w.B))
		}
		a.runDeferred()
	}
}

func cryptoRand(b []byte) { rand.Read(b) }

// A value search stops asking once it has enough answers, and 20 seconds
// before its end.
func TestValueSearchStops(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	s := newSearch(searchKeyword, RandomID(), fn.now)
	a.searches[s.target] = s
	s.answers = maxAnswers + 1
	a.tickSearches(fn.now.Add(time.Second))
	if !s.stopping {
		t.Fatal("not stopping with enough answers")
	}
	s2 := newSearch(searchFile, RandomID(), fn.now)
	a.searches[s2.target] = s2
	for i := range 5 { // nodes left to ask: it would not stop for lack of them
		c := contactN(30 + i)
		s2.possible[c.ID.Xor(s2.target)] = c
	}
	a.tickSearches(fn.now.Add(20 * time.Second))
	if s2.stopping {
		t.Fatal("stopping early")
	}
	a.tickSearches(fn.now.Add(26 * time.Second))
	if !s2.stopping {
		t.Fatal("not stopping 20 s before the end")
	}
}

// A search takes at most maxAnswers answers, however many come.
func TestSearchAnswersCapped(t *testing.T) {
	target := RandomID()
	got := 0
	fn, a, b := valueSearch(t, target, func(a *Node) {
		a.FindSources(target, 1, func(Source) { got++ })
	})
	for i := 0; i < 2*maxAnswers/50; i++ {
		var answers []func(*proto.Writer)
		for range 50 {
			answers = append(answers, sourceAnswer(RandomID(), 1, pubIP(100+i, false), 4662))
		}
		fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), target, answers...)))
	}
	a.runDeferred()
	if got != maxAnswers {
		t.Fatalf("%d answers taken", got)
	}
	a.mu.Lock()
	s := a.searches[target]
	a.mu.Unlock()
	if len(s.seen) != maxAnswers {
		t.Fatalf("%d answers kept", len(s.seen))
	}
}

// Once a value search stops, nobody else is asked.
func TestStoppedSearchAsksNobody(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	target := RandomID()
	s := newSearch(searchFile, target, fn.now)
	s.prepareToStop(fn.now)
	c := contactN(9)
	c.ID = target
	a.storePacket(s, ID{}, c, fn.now)
	if len(s.asked) != 0 {
		t.Fatal("asked after stopping")
	}
	// tickSearches does not move a stopping search on (a node lookup here:
	// value searches past their time are caught before).
	s2 := newSearch(searchNodeComplete, RandomID(), fn.now)
	s2.prepareToStop(fn.now)
	c2 := contactN(10)
	s2.possible[c2.ID.Xor(s2.target)] = c2
	a.searches[s2.target] = s2
	a.tickSearches(fn.now.Add(5 * time.Second))
	if len(s2.tried) != 0 {
		t.Fatal("stopping search asked another node")
	}
}

// found is never called after SearchKeyword returned, even for an answer
// being handed over at that moment.
func TestSearchKeywordNoLateResults(t *testing.T) {
	q := Query{Keywords: "orchard"}
	target := KeywordID("orchard")
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	var late bool
	fn, a, b := valueSearch(t, target, func(n *Node) {
		go func() {
			n.SearchKeyword(ctx, q, func(KeywordResult) {
				select {
				case <-returned:
					late = true
				default:
				}
			})
			close(returned)
		}()
		for i := 0; i < 2000; i++ {
			n.mu.Lock()
			s := n.searches[target]
			n.mu.Unlock()
			if s != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	// An answer is queued, then the caller gives up before it is handed
	// over.
	fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), target, keywordAnswer(RandomID(), "orchard.mkv", 5, false))))
	cancel()
	<-returned
	a.runDeferred()
	if late {
		t.Fatal("result reported after SearchKeyword returned")
	}
}

// Two searches whose best keyword is the same: the second uses its next
// best word instead of failing.
func TestSearchKeywordSameKeyword(t *testing.T) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	a.table.Add(contactOf(b), false, false, t0)
	a.table.SetAlive(b.ID(), t0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 3)
	go func() { done <- a.SearchKeyword(ctx, Query{Keywords: "orchard blue"}, func(KeywordResult) {}) }()
	waitSearches := func(n int) {
		for i := 0; i < 2000; i++ {
			a.mu.Lock()
			got := len(a.searches)
			a.mu.Unlock()
			if got == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("not %d searches", n)
	}
	waitSearches(1)
	go func() { done <- a.SearchKeyword(ctx, Query{Keywords: "orchard blue"}, func(KeywordResult) {}) }()
	waitSearches(2)
	a.mu.Lock()
	_, viaBlue := a.searches[KeywordID("blue")]
	a.mu.Unlock()
	if !viaBlue {
		t.Fatal("second search not on its other word")
	}
	// A third has no word left.
	if err := a.SearchKeyword(ctx, Query{Keywords: "orchard blue"}, func(KeywordResult) {}); err != ErrBusy {
		t.Fatalf("third search: %v", err)
	}
}

// Our own address, published by us, is not a source.
func TestFindSourcesSkipsUs(t *testing.T) {
	target := RandomID()
	var got []Source
	fn, a, b := valueSearch(t, target, func(a *Node) {
		a.cfg.TCPPort = func() uint16 { return 4662 }
		a.FindSources(target, 1, func(s Source) { got = append(got, s) })
	})
	a.mu.Lock()
	a.extIP, a.extIPLast = pubIP(5, false), pubIP(5, false)
	a.mu.Unlock()
	fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), target,
		sourceAnswer(RandomID(), 1, pubIP(5, false), 4662),
		sourceAnswer(RandomID(), 1, pubIP(5, false), 4663))))
	a.runDeferred()
	if len(got) != 1 || got[0].TCPPort != 4663 {
		t.Fatalf("%+v", got)
	}
}

func noteAnswer(user ID, name string, rating int, comment string) func(*proto.Writer) {
	return func(w *proto.Writer) {
		WriteID(w, user)
		tags := []proto.Tag{StrTag(TagFileName, name)}
		if rating > 0 {
			tags = append(tags, IntTag(TagFileRating, uint64(rating)))
		}
		if comment != "" {
			tags = append(tags, StrTag(TagDescription, comment))
		}
		WriteTags(w, tags)
	}
}

// The notes of a file are asked with its size; each user's note is
// reported once, and empty notes not at all.
func TestSearchNotes(t *testing.T) {
	target := RandomID()
	notes := make(chan Note, 10)
	errc := make(chan error, 1)
	fn, a, b := valueSearch(t, target, func(n *Node) {
		go func() {
			errc <- n.SearchNotes(context.Background(), target, 1<<33, func(nt Note) { notes <- nt })
		}()
		for range 2000 {
			n.mu.Lock()
			s := n.searches[target]
			n.mu.Unlock()
			if s != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("search not started")
	})
	p, ok := lastSent(fn, addrOf(a), addrOf(b), OpSearchNotesReq, &b.keys)
	if !ok {
		t.Fatal("no notes request")
	}
	r := proto.NewReader(p.Payload)
	if ReadID(r) != target || r.U64() != 1<<33 || r.Len() != 0 {
		t.Fatalf("request % X", p.Payload)
	}
	u1, u2, u3 := RandomID(), RandomID(), RandomID()
	fn.inject(addrOf(b), a, fromNode(a, b, searchRes(b.ID(), target,
		noteAnswer(u1, "Example.Film.mkv", 1, "fake, it is another film"),
		noteAnswer(u2, "Example.Film.mkv", 0, ""), // says nothing
		noteAnswer(u3, "Example.Film.mkv", 9, "great"),
		noteAnswer(u1, "Example.Film.mkv", 5, "again"),
	)))
	a.runDeferred()
	var got []Note
	for len(got) < 2 {
		select {
		case nt := <-notes:
			got = append(got, nt)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d notes", len(got))
		}
	}
	if got[0].Rating != 1 || got[0].Comment != "fake, it is another film" || got[1].Rating != 0 || got[1].Comment != "great" {
		t.Fatalf("%+v", got)
	}
	fn.now = fn.now.Add(time.Minute)
	a.mu.Lock()
	a.tickSearches(fn.now)
	a.mu.Unlock()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SearchNotes did not return")
	}
}

// Our note on a file goes to the nodes close to it: the file, our ID, and
// its name, size, rating and comment.
func TestPublishNote(t *testing.T) {
	target := RandomID()
	fn, a, b := valueSearch(t, target, func(a *Node) {
		if err := a.PublishNote(target, 1<<33, "Example.Film.mkv", 1, "fake, another film"); err != nil {
			t.Fatal(err)
		}
	})
	p, ok := lastSent(fn, addrOf(a), addrOf(b), OpPublishNotesReq, &b.keys)
	if !ok {
		t.Fatal("no notes published")
	}
	r := proto.NewReader(p.Payload)
	if ReadID(r) != target || ReadID(r) != ID(a.keys.UserHash) {
		t.Fatalf("publish % X", p.Payload)
	}
	tags, err := ReadTags(r)
	if err != nil {
		t.Fatal(err)
	}
	got := map[byte]proto.Tag{}
	for _, tg := range tags {
		got[tg.ID] = tg
	}
	if got[TagFileName].Str != "Example.Film.mkv" || got[TagFileSize].Int != 1<<33 || got[TagFileRating].Int != 1 ||
		got[TagDescription].Str != "fake, another film" {
		t.Fatalf("tags %+v", tags)
	}
	if err := a.PublishNote(target, 1, "x", 9, ""); err == nil {
		t.Fatal("rating 9 accepted")
	}
}
