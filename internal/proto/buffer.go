package proto

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrShort is returned when a buffer ends before a value could be read.
var ErrShort = errors.New("proto: short buffer")

// Writer builds little-endian payloads.
type Writer struct{ B []byte }

func (w *Writer) U8(v uint8)     { w.B = append(w.B, v) }
func (w *Writer) U16(v uint16)   { w.B = binary.LittleEndian.AppendUint16(w.B, v) }
func (w *Writer) U32(v uint32)   { w.B = binary.LittleEndian.AppendUint32(w.B, v) }
func (w *Writer) U64(v uint64)   { w.B = binary.LittleEndian.AppendUint64(w.B, v) }
func (w *Writer) Bytes(b []byte) { w.B = append(w.B, b...) }

// Str16 writes a string prefixed by its uint16 length.
func (w *Writer) Str16(s string) {
	w.U16(uint16(len(s)))
	w.B = append(w.B, s...)
}

// Reader consumes little-endian payloads. The first error sticks; check Err().
type Reader struct {
	B   []byte
	err error
}

func NewReader(b []byte) *Reader { return &Reader{B: b} }

func (r *Reader) Err() error { return r.err }
func (r *Reader) Len() int   { return len(r.B) }

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.B) < n {
		r.err = ErrShort
		r.B = nil
		return nil
	}
	b := r.B[:n]
	r.B = r.B[n:]
	return b
}

func (r *Reader) U8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *Reader) U16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *Reader) U32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *Reader) U64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (r *Reader) F32() float32 { return math.Float32frombits(r.U32()) }

// Bytes returns a copy of the next n bytes.
func (r *Reader) Bytes(n int) []byte {
	b := r.take(n)
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func (r *Reader) Hash() (h [16]byte) {
	copy(h[:], r.take(16))
	return
}

// Str16 reads a string prefixed by its uint16 length.
func (r *Reader) Str16() string { return string(r.take(int(r.U16()))) }
