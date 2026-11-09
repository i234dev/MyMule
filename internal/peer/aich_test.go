package peer

import (
	"testing"

	"mymule/internal/aich"
	"mymule/internal/ed2k"
)

func TestAICHPackets(t *testing.T) {
	file := ed2k.Hash{1, 2, 3}
	root := aich.Hash{9, 8, 7}
	if f, r, err := DecodeAICHFileHash(EncodeAICHFileHash(file, root)); err != nil || f != file || r != root {
		t.Fatal("file hash answer")
	}
	if f, p, r, err := DecodeAICHRequest(EncodeAICHRequest(file, 300, root)); err != nil || f != file || p != 300 || r != root {
		t.Fatal("request")
	}
	f, p, r, data, err := DecodeAICHAnswer(EncodeAICHAnswer(file, 7, root, []byte{1, 2}))
	if err != nil || f != file || p != 7 || r != root || len(data) != 2 {
		t.Fatal("answer")
	}
	f, _, _, data, err = DecodeAICHAnswer(EncodeAICHAnswer(file, 7, root, nil))
	if err != nil || f != file || data != nil {
		t.Fatal("empty answer")
	}
	for _, bad := range [][]byte{nil, make([]byte, 15), make([]byte, 20), make([]byte, 37)} {
		if _, _, _, _, err := DecodeAICHAnswer(bad); err == nil && len(bad) != 16 {
			t.Errorf("%d bytes read", len(bad))
		}
	}
	if _, _, err := DecodeAICHFileHash(make([]byte, 35)); err == nil {
		t.Fatal("short file hash answer")
	}
	if NewHello(ed2k.Hash{}, "x", 0, 0, nil, 0).AICH() != AICHVersion {
		t.Fatal("our hello does not offer AICH")
	}
}
