package arr

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"testing"

	"mymule/internal/ed2k"
)

var exampleLink = ed2k.Link{Name: "Example Distro 1.0 (demo).iso", Size: 734003200,
	Hash: ed2k.Hash{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF}}

func TestTorrentRoundTrip(t *testing.T) {
	data := Torrent(exampleLink)
	got, err := LinkFromTorrent(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != exampleLink {
		t.Fatalf("link: %+v", got)
	}

	// The info hash a BitTorrent client computes from the file (sha1 of
	// the raw info dictionary) must equal InfoHash.
	start := bytes.Index(data, []byte("4:infod")) + len("4:info")
	_, end, err := bdecodeAt(data, start)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(data[start:end])
	if hex.EncodeToString(sum[:]) != InfoHash(exampleLink) {
		t.Fatal("InfoHash differs from the hash of the torrent's info dictionary")
	}
	if len(InfoHash(exampleLink)) != 40 || InfoHash(exampleLink) == InfoHash(ed2k.Link{Name: "other", Size: 1}) {
		t.Fatal("info hash not unique")
	}
}

func TestForeignTorrentRejected(t *testing.T) {
	var buf bytes.Buffer
	bencode(&buf, map[string]any{"info": map[string]any{"name": "x", "length": 1}})
	if _, err := LinkFromTorrent(buf.Bytes()); err == nil {
		t.Fatal("a torrent without an ed2k link was accepted")
	}
	if _, err := LinkFromTorrent([]byte("garbage")); err == nil {
		t.Fatal("garbage accepted")
	}
}
