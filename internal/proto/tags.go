package proto

import (
	"fmt"
	"math"
)

// Tag value types.
const (
	TagHash16    = 0x01
	TagString    = 0x02
	TagUint32    = 0x03
	TagFloat32   = 0x04
	TagBool      = 0x05
	TagBoolArray = 0x06
	TagBlob      = 0x07
	TagUint16    = 0x08
	TagUint8     = 0x09
	TagBsob      = 0x0A
	TagUint64    = 0x0B
	TagStr1      = 0x11 // TagStr1..TagStr16: short strings, length = type-0x10
	TagStr16     = 0x20
)

// Tag is a typed key/value. A tag is identified either by a one-byte ID
// (most common) or by a string name (older/extension tags).
type Tag struct {
	ID    byte
	Name  string // non-empty for string-named tags
	Type  byte   // normalised type (short strings become TagString)
	Str   string
	Int   uint64
	Float float32
	Bytes []byte
}

func (t Tag) Key() string {
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("0x%02X", t.ID)
}

func (t Tag) String() string {
	switch t.Type {
	case TagString:
		return fmt.Sprintf("%s=%q", t.Key(), t.Str)
	case TagFloat32:
		return fmt.Sprintf("%s=%g", t.Key(), t.Float)
	case TagHash16, TagBlob, TagBsob, TagBoolArray:
		return fmt.Sprintf("%s=%x", t.Key(), t.Bytes)
	default:
		return fmt.Sprintf("%s=%d", t.Key(), t.Int)
	}
}

// Constructors for tags we send.
func StrTag(id byte, s string) Tag { return Tag{ID: id, Type: TagString, Str: s} }
func IntTag(id byte, v uint32) Tag { return Tag{ID: id, Type: TagUint32, Int: uint64(v)} }

// WriteTag writes a tag in the classic (non-compact) format, which every
// server and client understands: [type][u16 namelen][name][value].
func (w *Writer) WriteTag(t Tag) {
	w.U8(t.Type)
	if t.Name != "" {
		w.Str16(t.Name)
	} else {
		w.U16(1)
		w.U8(t.ID)
	}
	switch t.Type {
	case TagString:
		w.Str16(t.Str)
	case TagUint32:
		w.U32(uint32(t.Int))
	case TagUint16:
		w.U16(uint16(t.Int))
	case TagUint8:
		w.U8(uint8(t.Int))
	case TagUint64:
		w.U64(t.Int)
	case TagFloat32:
		w.U32(math.Float32bits(t.Float))
	case TagHash16:
		w.Bytes(t.Bytes[:16])
	case TagBlob:
		w.U32(uint32(len(t.Bytes)))
		w.Bytes(t.Bytes)
	default:
		panic(fmt.Sprintf("proto: WriteTag: unsupported type 0x%02X", t.Type))
	}
}

// ReadTag reads a tag in either classic or compact (type|0x80) format.
func (r *Reader) ReadTag() (Tag, error) {
	var t Tag
	typ := r.U8()
	if typ&0x80 != 0 {
		typ &^= 0x80
		t.ID = r.U8()
	} else {
		name := r.Bytes(int(r.U16()))
		if len(name) == 1 {
			t.ID = name[0]
		} else {
			t.Name = string(name)
		}
	}
	if r.Err() != nil {
		return t, r.Err()
	}

	switch {
	case typ == TagString:
		t.Type, t.Str = TagString, r.Str16()
	case typ >= TagStr1 && typ <= TagStr16:
		t.Type, t.Str = TagString, string(r.Bytes(int(typ-0x10)))
	case typ == TagUint8:
		t.Type, t.Int = TagUint32, uint64(r.U8())
	case typ == TagUint16:
		t.Type, t.Int = TagUint32, uint64(r.U16())
	case typ == TagUint32:
		t.Type, t.Int = TagUint32, uint64(r.U32())
	case typ == TagUint64:
		t.Type, t.Int = TagUint64, r.U64()
	case typ == TagFloat32:
		t.Type, t.Float = TagFloat32, r.F32()
	case typ == TagBool:
		t.Type, t.Int = TagUint32, uint64(r.U8())
	case typ == TagHash16:
		t.Type, t.Bytes = TagHash16, r.Bytes(16)
	case typ == TagBlob:
		t.Type, t.Bytes = TagBlob, r.Bytes(int(r.U32()))
	case typ == TagBsob:
		t.Type, t.Bytes = TagBsob, r.Bytes(int(r.U8()))
	case typ == TagBoolArray:
		n := int(r.U16())
		t.Type, t.Bytes = TagBoolArray, r.Bytes((n+7)/8)
	default:
		return t, fmt.Errorf("proto: unknown tag type 0x%02X", typ)
	}
	return t, r.Err()
}

// ReadTags reads n tags.
func (r *Reader) ReadTags(n int) ([]Tag, error) {
	tags := make([]Tag, 0, min(n, 64))
	for i := 0; i < n; i++ {
		t, err := r.ReadTag()
		if err != nil {
			return tags, err
		}
		tags = append(tags, t)
	}
	return tags, nil
}

// TagList adds lookup helpers to a slice of tags.
type TagList []Tag

func (l TagList) Find(id byte) (Tag, bool) {
	for _, t := range l {
		if t.Name == "" && t.ID == id {
			return t, true
		}
	}
	return Tag{}, false
}

func (l TagList) FindName(name string) (Tag, bool) {
	for _, t := range l {
		if t.Name == name {
			return t, true
		}
	}
	return Tag{}, false
}

func (l TagList) Str(id byte) string {
	t, _ := l.Find(id)
	return t.Str
}

func (l TagList) Int(id byte) uint64 {
	t, _ := l.Find(id)
	return t.Int
}
