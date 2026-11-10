package kad

import (
	"bytes"
	"fmt"
	"math"
	"unicode/utf8"

	"mymule/internal/proto"
)

// Kad tag names (one-byte names, eMule's TAG_*).
const (
	TagFileName       = 0x01
	TagFileSize       = 0x02
	TagFileType       = 0x03
	TagFileFormat     = 0x04
	TagDescription    = 0x0B // a note's comment
	TagSources        = 0x15
	TagPublishInfo    = 0x33
	TagFileSizeHi     = 0x3A
	TagMediaArtist    = 0xD0
	TagMediaAlbum     = 0xD1
	TagMediaTitle     = 0xD2
	TagMediaLength    = 0xD3
	TagMediaBitrate   = 0xD4
	TagMediaCodec     = 0xD5
	TagKadMiscOptions = 0xF2
	TagEncryption     = 0xF3
	TagFileRating     = 0xF7
	TagBuddyHash      = 0xF8
	TagClientLowID    = 0xF9
	TagServerPort     = 0xFA
	TagServerIP       = 0xFB
	TagSourceUDPPort  = 0xFC
	TagSourcePort     = 0xFD
	TagSourceIP       = 0xFE
	TagSourceType     = 0xFF
)

// IntTag returns an integer tag of the smallest type that holds v, as
// eMule writes them in Kad.
func IntTag(id byte, v uint64) proto.Tag {
	t := proto.Tag{ID: id, Int: v}
	switch {
	case v <= math.MaxUint8:
		t.Type = proto.TagUint8
	case v <= math.MaxUint16:
		t.Type = proto.TagUint16
	case v <= math.MaxUint32:
		t.Type = proto.TagUint32
	default:
		t.Type = proto.TagUint64
	}
	return t
}

// StrTag returns a string tag.
func StrTag(id byte, s string) proto.Tag { return proto.Tag{ID: id, Type: proto.TagString, Str: s} }

// WriteTags writes a Kad tag list: a one-byte count, then the tags. At
// most 255 tags are written.
func WriteTags(w *proto.Writer, tags []proto.Tag) {
	n := min(len(tags), math.MaxUint8)
	w.U8(uint8(n))
	for _, t := range tags[:n] {
		writeTag(w, t)
	}
}

func writeTag(w *proto.Writer, t proto.Tag) {
	w.U8(t.Type)
	if t.Name != "" {
		w.Str16(t.Name)
	} else {
		w.U16(1)
		w.U8(t.ID)
	}
	switch t.Type {
	case proto.TagString:
		s := t.Str
		if len(s) > math.MaxUint16 {
			i := math.MaxUint16 // cut before a character, not inside one
			for i > 0 && !utf8.RuneStart(s[i]) {
				i--
			}
			s = s[:i]
		}
		w.Str16(s)
	case proto.TagUint8:
		w.U8(uint8(t.Int))
	case proto.TagUint16:
		w.U16(uint16(t.Int))
	case proto.TagUint32:
		w.U32(uint32(t.Int))
	case proto.TagUint64:
		w.U64(t.Int)
	case proto.TagFloat32:
		w.U32(math.Float32bits(t.Float))
	case proto.TagHash16:
		var h [16]byte
		copy(h[:], t.Bytes)
		w.Bytes(h[:])
	case proto.TagBsob:
		b := t.Bytes
		if len(b) > math.MaxUint8 {
			b = b[:math.MaxUint8]
		}
		w.U8(uint8(len(b)))
		w.Bytes(b)
	default:
		panic(fmt.Sprintf("kad: cannot write tag type 0x%02X", t.Type))
	}
}

// maxTagBytes is the longest string or blob accepted in a tag.
const maxTagBytes = 4096

// ReadTags reads a Kad tag list. Integers of every size come back as
// TagUint32 (or TagUint64), like proto.ReadTag, and strings that are not
// UTF-8 are read as Latin-1, as eMule does.
func ReadTags(r *proto.Reader) ([]proto.Tag, error) {
	n := int(r.U8())
	if r.Err() != nil {
		return nil, r.Err()
	}
	if n > r.Len()/4 { // the smallest tag is 4 bytes (type, name, 1-byte value)
		return nil, fmt.Errorf("kad: implausible tag count %d", n)
	}
	tags := make([]proto.Tag, 0, n)
	for range n {
		t, err := readTag(r)
		if err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, nil
}

func readTag(r *proto.Reader) (proto.Tag, error) {
	var t proto.Tag
	typ := r.U8()
	nameLen := int(r.U16())
	if nameLen > maxTagBytes {
		return t, fmt.Errorf("kad: tag name of %d bytes", nameLen)
	}
	name := r.Bytes(nameLen)
	if len(name) == 1 {
		t.ID = name[0]
	} else {
		t.Name = text(name)
	}
	if r.Err() != nil {
		return t, r.Err()
	}
	switch typ {
	case proto.TagString:
		n := int(r.U16())
		if n > maxTagBytes {
			return t, fmt.Errorf("kad: tag string of %d bytes", n)
		}
		t.Type, t.Str = proto.TagString, text(r.Bytes(n))
	case proto.TagUint8:
		t.Type, t.Int = proto.TagUint32, uint64(r.U8())
	case proto.TagUint16:
		t.Type, t.Int = proto.TagUint32, uint64(r.U16())
	case proto.TagUint32:
		t.Type, t.Int = proto.TagUint32, uint64(r.U32())
	case proto.TagUint64:
		t.Type, t.Int = proto.TagUint64, r.U64()
	case proto.TagBool:
		t.Type, t.Int = proto.TagUint32, uint64(r.U8())
	case proto.TagFloat32:
		t.Type, t.Float = proto.TagFloat32, r.F32()
	case proto.TagHash16:
		t.Type, t.Bytes = proto.TagHash16, r.Bytes(16)
	case proto.TagBsob:
		t.Type, t.Bytes = proto.TagBsob, r.Bytes(int(r.U8()))
	default:
		return t, fmt.Errorf("kad: unknown tag type 0x%02X", typ)
	}
	return t, r.Err()
}

// text returns b as a string, read as Latin-1 when it is not UTF-8. A
// UTF-8 byte order mark is dropped, as eMule does.
func text(b []byte) string {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// TagInt returns the integer value of tag id and whether it is there.
func TagInt(tags []proto.Tag, id byte) (uint64, bool) {
	for _, t := range tags {
		if t.Name == "" && t.ID == id && (t.Type == proto.TagUint32 || t.Type == proto.TagUint64) {
			return t.Int, true
		}
	}
	return 0, false
}

// TagStr returns the string value of tag id, or "".
func TagStr(tags []proto.Tag, id byte) string {
	for _, t := range tags {
		if t.Name == "" && t.ID == id && t.Type == proto.TagString {
			return t.Str
		}
	}
	return ""
}
