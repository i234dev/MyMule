package download

import (
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
)

// A peer's hello (user hash, client ID, port) is only a claim. A stranger
// who copies a source's values must not be taken for that source.
func TestClaimSourceNeedsProof(t *testing.T) {
	m := NewManager(Config{})
	f := &File{sources: map[string]*Source{}}
	m.files = map[ed2k.Hash]*File{{1}: f}
	high := &Source{key: "11.0.0.1:4662", IP: pub(1), Port: 4662, UserHash: ed2k.Hash{0xAA}, file: f}
	low := &Source{key: "low:1234", LowID: 1234, UserHash: ed2k.Hash{0xBB}, file: f}
	f.sources[high.key] = high
	f.sources[low.key] = low

	// The same user hash or port from another address is someone else.
	for _, h := range []peer.Hello{
		{UserHash: ed2k.Hash{0xAA}, Port: 4662},
		{UserHash: ed2k.Hash{0xBB}, ClientID: 1234},
		{ClientID: 1234},
	} {
		if src := m.claimSource(h, pub(66)); src != nil {
			t.Fatalf("hello %+v from a stranger claimed %s", h, src.key)
		}
	}
	if m.active != 0 {
		t.Fatalf("active = %d after refused claims", m.active)
	}

	// The HighID source from its own address: by port, or by user hash.
	if src := m.claimSource(peer.Hello{Port: 4662}, pub(1)); src != high {
		t.Fatal("HighID source not recognised from its own address")
	}
	m.releaseSource(high)
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xAA}, Port: 1}, pub(1)); src != high {
		t.Fatal("HighID source with a new port not recognised by its user hash")
	}
	m.releaseSource(high)

	// The LowID source only while its requested callback is pending, once.
	low.callbackAt = time.Now()
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xCC}, ClientID: 1234}, pub(7)); src != nil {
		t.Fatal("LowID source claimed with another user hash")
	}
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xBB}, ClientID: 1234}, pub(7)); src != low {
		t.Fatal("LowID source not recognised after our callback request")
	}
	m.releaseSource(low)
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xBB}, ClientID: 1234}, pub(8)); src != nil {
		t.Fatal("one callback request was claimed twice")
	}
	low.callbackAt = time.Now().Add(-callbackWait - time.Second)
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xBB}, ClientID: 1234}, pub(7)); src != nil {
		t.Fatal("LowID source claimed after the callback window")
	}
}

// A LowID uploader that queued us connects to us by itself when our slot
// comes (no callback asked for at that moment): it is recognised by the
// user hash it gave us before, a stranger with its client ID is not.
func TestQueuedLowIDUploaderRecognised(t *testing.T) {
	m := NewManager(Config{})
	f := &File{sources: map[string]*Source{}}
	m.files = map[ed2k.Hash]*File{{1}: f}
	low := &Source{key: "low:1234", LowID: 1234, UserHash: ed2k.Hash{0xBB}, State: StateQueued, Rank: 3, file: f}
	f.sources[low.key] = low
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xCC}, ClientID: 1234}, pub(7)); src != nil {
		t.Fatal("a stranger with the source's client ID was taken for it")
	}
	if src := m.claimSource(peer.Hello{UserHash: ed2k.Hash{0xBB}, ClientID: 1234}, pub(7)); src != low {
		t.Fatal("the queued LowID uploader calling us for our slot was not recognised")
	}
}
