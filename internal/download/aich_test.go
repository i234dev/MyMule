package download

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"

	"mymule/internal/aich"
	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// aichSetup is a download of three parts whose link gives the AICH hash
// of data (aichOf: of another content, when set).
func aichSetup(t *testing.T, data, aichOf []byte) (*PartFile, *aich.Tree) {
	t.Helper()
	if aichOf == nil {
		aichOf = data
	}
	tree, err := aich.FromReader(bytes.NewReader(aichOf), uint64(len(aichOf)))
	if err != nil {
		t.Fatal(err)
	}
	link, fh := linkFor(t, "Example.Film.mkv", data)
	link.AICH = tree.Root()
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pf.Close() })
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	return pf, tree
}

// corruptBlocks returns data with the given blocks of part 0 changed.
func corruptBlocks(data []byte, blocks ...int) []byte {
	bad := bytes.Clone(data)
	for _, b := range blocks {
		bad[b*aich.BlockSize+100] ^= 0xFF
	}
	return bad
}

func recoveryOf(t *testing.T, tree *aich.Tree, data []byte, p int) []byte {
	t.Helper()
	blocks, err := aich.BlockHashes(tree.Size, p, func(b []byte, off int64) error {
		copy(b, data[off:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := tree.Recovery(p, blocks)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

var part0 = []bool{true, false, false}

// A part that fails its hash waits for recovery data instead of being
// downloaded again; with it, the good blocks are kept and only the bad
// ones downloaded again.
func TestAICHRepairsCorruptPart(t *testing.T) {
	data := testData(int(ed2k.PartSize)*2 + 5000)
	pf, tree := aichSetup(t, data, nil)
	if r, ok := pf.AICHRoot(); !ok || r != tree.Root() || pf.NeedsAICHRoot() {
		t.Fatal("the link's AICH hash is not trusted")
	}
	serve(t, pf, "peer:1", part0, corruptBlocks(data, 2, 40), 0)
	if pf.Corrupt() != 1 {
		t.Fatalf("%d corrupt parts", pf.Corrupt())
	}
	if b := pf.Reserve("peer:2", part0, 5); len(b) != 0 {
		t.Fatal("part waiting for recovery data downloaded again")
	}
	if _, _, ok := pf.AICHAsk("peer:2", []bool{false, true, true}); ok {
		t.Fatal("asked a peer without the part")
	}
	p, root, ok := pf.AICHAsk("peer:2", part0)
	if !ok || p != 0 || root != tree.Root() {
		t.Fatal("recovery data not asked for")
	}
	if _, _, ok := pf.AICHAsk("peer:3", part0); ok {
		t.Fatal("asked a second peer at once")
	}
	rec := recoveryOf(t, tree, data, 0)
	if _, _, err := pf.AICHRecovered("peer:3", 0, root, rec); err == nil {
		t.Fatal("recovery data taken from a peer not asked")
	}
	bad := bytes.Clone(rec)
	bad[10] ^= 1
	if _, _, err := pf.AICHRecovered("peer:2", 0, root, bad); err == nil {
		t.Fatal("bad recovery data taken")
	}
	// After bad data, that peer is not asked again; another one is, and
	// its good data is taken.
	if _, _, ok := pf.AICHAsk("peer:2", part0); ok {
		t.Fatal("a peer that sent bad recovery data asked again")
	}
	if p, _, ok := pf.AICHAsk("peer:5", part0); !ok || p != 0 {
		t.Fatal("no other peer asked after bad recovery data")
	}
	pf.AICHNone("peer:5") // had none: not asked again either
	if _, _, ok := pf.AICHAsk("peer:5", part0); ok {
		t.Fatal("a peer without recovery data asked again")
	}
	pf.AICHAsk("peer:6", part0)
	kept, total, err := pf.AICHRecovered("peer:6", 0, root, rec)
	if err != nil || kept != total-2 || total != aich.PartBlocks(tree.Size, 0) {
		t.Fatalf("kept %d of %d: %v", kept, total, err)
	}
	if n := serve(t, pf, "peer:4", part0, data, 0); n != 2 {
		t.Fatalf("%d blocks downloaded again, want the 2 bad ones", n)
	}
	if v := pf.VerifiedParts(); !v[0] || pf.Corrupt() != 1 {
		t.Fatalf("part 0 not verified after repair (corrupt %d)", pf.Corrupt())
	}
}

// A repaired part that fails again (the AICH hash was a lie, so bad
// blocks were kept) is not blamed on the peer that filled in the rest.
func TestAICHRepairFailureNotBlamedOnFiller(t *testing.T) {
	data := testData(int(ed2k.PartSize)*2 + 5000)
	bad := corruptBlocks(data, 2, 40)
	lie := corruptBlocks(data, 2, 40, 5) // calls blocks 2 and 40 good
	pf, tree := aichSetup(t, data, lie)
	serve(t, pf, "peer:1", part0, bad, 0)
	p, root, ok := pf.AICHAsk("peer:2", part0)
	if !ok {
		t.Fatal("not asked")
	}
	if _, _, err := pf.AICHRecovered("peer:2", p, root, recoveryOf(t, tree, lie, 0)); err != nil {
		t.Fatal(err)
	}
	if n := serve(t, pf, "peer:3", part0, data, 0); n != 1 {
		t.Fatalf("%d blocks downloaded again, want 1", n)
	}
	if pf.Corrupt() != 2 {
		t.Fatalf("%d corrupt parts, want 2", pf.Corrupt())
	}
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.strikes["peer:3"] != 0 || pf.badPart["peer:3"] != nil {
		t.Fatal("the peer that filled in the repaired part was blamed")
	}
	if !pf.isolate[0] {
		t.Fatal("the part is not downloaded again from a single peer")
	}
}

// Without an answer in time, the part is downloaded again whole.
func TestAICHWaitEnds(t *testing.T) {
	old := aichWaitMax
	aichWaitMax = 50 * time.Millisecond
	defer func() { aichWaitMax = old }()
	data := testData(int(ed2k.PartSize)*2 + 5000)
	pf, _ := aichSetup(t, data, nil)
	serve(t, pf, "peer:1", part0, corruptBlocks(data, 3), 0)
	if len(pf.Reserve("peer:2", part0, 1)) != 0 {
		t.Fatal("not waiting")
	}
	time.Sleep(60 * time.Millisecond)
	if len(pf.Reserve("peer:2", part0, 1)) != 1 {
		t.Fatal("still waiting after aichWaitMax")
	}
	if pf.aich.waiting.Load() != 0 {
		t.Fatal("wait not dropped")
	}
}

// An AICH hash that does not describe the same file as the eD2K hash
// (all blocks good, the part bad) is not used again; neither is one under
// which a repaired part fails again.
func TestAICHWrongHashTurnsOff(t *testing.T) {
	data := testData(int(ed2k.PartSize)*2 + 5000)
	other := corruptBlocks(data, 5)
	pf, tree := aichSetup(t, data, other)
	serve(t, pf, "peer:1", part0, other, 0) // matches the AICH hash, not the eD2K one
	p, root, ok := pf.AICHAsk("peer:2", part0)
	if !ok {
		t.Fatal("not asked")
	}
	if _, _, err := pf.AICHRecovered("peer:2", p, root, recoveryOf(t, tree, other, 0)); err == nil {
		t.Fatal("recovery data that calls a failed part good taken")
	}
	if _, ok := pf.AICHRoot(); ok || pf.NeedsAICHRoot() {
		t.Fatal("AICH still used")
	}
	if len(pf.Reserve("peer:3", part0, 1)) != 1 {
		t.Fatal("part not downloaded again")
	}

	// A repaired part failing again may be a peer sending bad blocks
	// again: repaired again. Failing aichMaxRepairs times: off.
	pf2, tree2 := aichSetup(t, data, other)
	serve(t, pf2, "peer:1", part0, corruptBlocks(other, 9), 0)
	for i := range aichMaxRepairs {
		owner := fmt.Sprintf("peer:%d", 10+i)
		if _, _, ok := pf2.AICHAsk(owner, part0); !ok {
			t.Fatalf("round %d: not asked", i)
		}
		if _, _, err := pf2.AICHRecovered(owner, 0, tree2.Root(), recoveryOf(t, tree2, other, 0)); err != nil {
			t.Fatal(err)
		}
		if _, ok := pf2.AICHRoot(); !ok {
			t.Fatalf("AICH off after %d failures of a repaired part", i)
		}
		serve(t, pf2, fmt.Sprintf("peer:%d", 20+i), part0, corruptBlocks(other, 9), 0) // block 9 bad again
	}
	if _, ok := pf2.AICHRoot(); ok {
		t.Fatal("AICH still used after a repaired part failed again and again")
	}
}

// A hash from peers is trusted once enough different networks sent it,
// and nearly all of those that sent one.
func TestAICHVotes(t *testing.T) {
	data := testData(int(ed2k.PartSize)*2 + 5000)
	link, _ := linkFor(t, "Example.Film.mkv", data)
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	a, b := aich.Hash{1}, aich.Hash{2}
	ip := func(i int) net.IP { return net.ParseIP(fmt.Sprintf("203.0.%d.1", i*16)) }
	for i := range aichTrustIPs - 1 {
		if pf.AICHVote(ip(i), a) {
			t.Fatal("trusted too soon")
		}
	}
	// Same /20: counts once; and cannot vote for another hash too.
	if pf.AICHVote(net.ParseIP("203.0.0.200"), a) || pf.AICHVote(net.ParseIP("203.0.1.1"), a) {
		t.Fatal("one network counted twice")
	}
	pf.AICHVote(net.ParseIP("203.0.2.1"), b)
	if len(pf.aich.votes[b]) != 0 {
		t.Fatal("one network voted for two hashes")
	}
	// A dissenting network: 10 of 11, 11 of 12 (91%) are under 92%; 12
	// of 13 is not.
	pf.AICHVote(ip(15), b)
	if pf.AICHVote(ip(aichTrustIPs), a) || pf.AICHVote(ip(aichTrustIPs+1), a) {
		t.Fatal("trusted under 92%")
	}
	if !pf.AICHVote(ip(aichTrustIPs+2), a) {
		t.Fatal("not trusted with 12 of 13")
	}
	if r, ok := pf.AICHRoot(); !ok || r != a || pf.NeedsAICHRoot() {
		t.Fatal("not trusted")
	}
	// Kept across a restart.
	pf.Save()
	pf2, err := Open(pf.metaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pf2.Close()
	if r, ok := pf2.AICHRoot(); !ok || r != a {
		t.Fatal("trusted hash not saved")
	}
}

// The session asks AICH peers for the hash, takes their answers as
// votes, asks for recovery data of a waiting part, and repairs with it.
func TestAICHSession(t *testing.T) {
	aichHello := peer.Hello{Tags: proto.TagList{proto.IntTag(peer.CTEMuleMiscOpts1, 1<<29)}}
	s, theirs, f := sxSession(t, aichHello)
	data := testData(int(f.pf.Size))

	if err := s.askAICHRoot(); err != nil {
		t.Fatal(err)
	}
	pk, err := theirs.Read(2 * time.Second)
	if err != nil || pk.Proto != proto.ProtoEMule || pk.Opcode != peer.OpAICHFileHashReq {
		t.Fatalf("hash request: %v %+v", err, pk)
	}
	root := aich.Hash{3}
	s.handle(&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpAICHFileHashAns, Payload: peer.EncodeAICHFileHash(f.pf.Hash, root)})
	if len(f.pf.aich.votes[root]) != 1 {
		t.Fatal("answer not counted")
	}

	// A peer without AICH is not asked.
	plain, plainTheirs, _ := sxSession(t, peer.Hello{})
	plain.pf = f.pf
	plain.askAICHRoot()
	if _, err := plainTheirs.Read(200 * time.Millisecond); err == nil {
		t.Fatal("peer without AICH asked")
	}

	tree, _ := aich.FromReader(bytes.NewReader(data), uint64(len(data)))
	f.pf.mu.Lock()
	f.pf.trustLinkAICH(tree.Root())
	f.pf.mu.Unlock()
	serve(t, f.pf, "peer:1", part0, corruptBlocks(data, 7), 0)
	if err := s.askRecovery(); err != nil {
		t.Fatal(err)
	}
	pk, err = theirs.Read(2 * time.Second)
	if err != nil || pk.Opcode != peer.OpAICHRequest {
		t.Fatalf("recovery request: %v", err)
	}
	file, part, r, _ := peer.DecodeAICHRequest(pk.Payload)
	if file != f.pf.Hash || part != 0 || r != tree.Root() {
		t.Fatal("wrong request")
	}
	s.handle(&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpAICHAnswer, Payload: peer.EncodeAICHAnswer(file, 0, r, recoveryOf(t, tree, data, 0))})
	if done, _ := f.pf.Progress(); done != int64(aich.PartSize-aich.BlockSize) {
		t.Fatalf("%d bytes kept after repair", done)
	}
}
