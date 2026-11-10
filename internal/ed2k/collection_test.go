package ed2k

import (
	"strings"
	"testing"

	"mymule/internal/proto"
)

// collectionFile builds an eMule collection in its binary format.
func collectionFile(name string, files ...[]proto.Tag) []byte {
	var w proto.Writer
	w.U32(2)
	w.U32(1)
	w.WriteTag(proto.StrTag(collName, "\xEF\xBB\xBF"+name))
	w.U32(uint32(len(files)))
	for _, tags := range files {
		w.U32(uint32(len(tags)))
		for _, t := range tags {
			w.WriteTag(t)
		}
	}
	return w.B
}

func fileTags(name string, size uint64, hash string, big bool) []proto.Tag {
	h, _ := ParseHash(hash)
	sz := proto.IntTag(collSize, uint32(size))
	if big {
		sz = proto.Tag{ID: collSize, Type: proto.TagUint64, Int: size}
	}
	return []proto.Tag{proto.StrTag(collName, name), sz, {ID: collHash, Type: proto.TagHash16, Bytes: h[:]}}
}

func TestParseCollection(t *testing.T) {
	b := collectionFile("Una Colección",
		fileTags("Example Film (2001).mkv", 734003200, "0123456789ABCDEF0123456789ABCDEF", false),
		fileTags("Example Show S01E01.mkv", 5<<30, "AAAA456789ABCDEF0123456789ABCDEF", true),
		[]proto.Tag{proto.StrTag(collName, "no hash.mkv"), proto.IntTag(collSize, 10)}, // left out
		fileTags("zero size.mkv", 0, "BBBB456789ABCDEF0123456789ABCDEF", false),        // left out
	)
	name, links, err := ParseCollection(b)
	if err != nil || name != "Una Colección" || len(links) != 2 {
		t.Fatalf("%q %+v %v", name, links, err)
	}
	if links[1].Size != 5<<30 || links[1].Name != "Example Show S01E01.mkv" || links[0].Hash.String() != "0123456789ABCDEF0123456789ABCDEF" {
		t.Fatalf("%+v", links)
	}

	text := "Mis enlaces\r\ned2k://|file|Example.Film.mkv|734003200|0123456789ABCDEF0123456789ABCDEF|/\r\n" +
		"ed2k://|file|Other.mkv|1000|AAAA456789ABCDEF0123456789ABCDEF|/ ed2k://|file|bad|x|y|/\n"
	if name, links, err := ParseCollection([]byte(text)); err != nil || name != "" || len(links) != 2 {
		t.Fatalf("text: %q %+v %v", name, links, err)
	}

	for what, bad := range map[string][]byte{
		"empty":       nil,
		"random text": []byte("hello"),
		"huge count":  append(collectionFile("x")[:len(collectionFile("x"))-4], 0xFF, 0xFF, 0xFF, 0x7F),
		"too big":     []byte(strings.Repeat("x", MaxCollectionSize+1)),
	} {
		if _, _, err := ParseCollection(bad); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}
