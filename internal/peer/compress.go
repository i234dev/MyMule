package peer

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// Compressed transfers, as eMule's: a requested block is compressed with
// zlib as a whole and sent in packets (OP_COMPRESSEDPART) that all carry
// the block's start and the compressed size; the receiver puts them
// together and inflates the block.

// DataCompVersion is the compression we support (misc options 1, bits
// 20-23).
const DataCompVersion = 1

// DataCompression returns the compression version a peer supports.
func (h Hello) DataCompression() byte { return byte(h.Tags.Int(CTEMuleMiscOpts1)>>20) & 0x0F }

// incompressible are file types that are compressed already: compressing
// their blocks again costs time and saves nothing.
var incompressible = map[string]bool{
	".avi": true, ".mkv": true, ".mp4": true, ".m4v": true, ".mpg": true, ".mpeg": true, ".mov": true,
	".wmv": true, ".webm": true, ".ts": true, ".m2ts": true, ".rmvb": true, ".ogm": true, ".flv": true,
	".mp3": true, ".ogg": true, ".opus": true, ".flac": true, ".aac": true, ".m4a": true, ".wma": true,
	".zip": true, ".rar": true, ".7z": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true,
	".zst": true, ".cab": true, ".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".epub": true, ".cbz": true, ".cbr": true, ".docx": true, ".xlsx": true, ".pptx": true, ".odt": true,
	".apk": true, ".jar": true, ".msi": true, ".dmg": true, ".heic": true, ".avif": true,
}

// Compressible reports whether blocks of a file with this name are worth
// compressing.
func Compressible(name string) bool { return !incompressible[strings.ToLower(filepath.Ext(name))] }

// CompressBlock compresses a block; ok is false when that saves less than
// a tenth (then it is sent as it is).
func CompressBlock(data []byte) (out []byte, ok bool) {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.DefaultCompression)
	w.Write(data)
	w.Close()
	if b.Len() >= len(data)-len(data)/10 {
		return nil, false
	}
	return b.Bytes(), true
}

// CompressedPacketSize is how much compressed data goes in each packet,
// as eMule's (about 10 KB).
const CompressedPacketSize = 10240

// EncodeCompressedPart builds one packet of a compressed block: start is
// the block's start, total the whole compressed size, chunk this packet's
// share. big chooses the 64-bit form.
func EncodeCompressedPart(hash ed2k.Hash, start int64, total uint32, chunk []byte, big bool) (byte, []byte) {
	var w proto.Writer
	w.Bytes(hash[:])
	op := byte(OpCompressedPart)
	if big {
		op = OpCompressedPartI64
		w.U64(uint64(start))
	} else {
		w.U32(uint32(start))
	}
	w.U32(total)
	w.Bytes(chunk)
	return op, w.B
}

// maxPacked bounds the compressed size of a block of n bytes: zlib adds a
// little to data that does not compress, and a peer claiming more is
// lying (it would make us keep that much in memory).
func maxPacked(n int64) int64 { return n + n/64 + 1024 }

// CompressedBlock gathers the packets of one compressed block.
type CompressedBlock struct {
	Start  int64 // the block's start
	Size   int64 // its length once inflated
	packed int64 // the compressed size the packets announce
	buf    []byte
}

// ErrBadCompressed is returned for compressed data that does not fit the
// block asked for.
var ErrBadCompressed = errors.New("bad compressed block")

// NewCompressedBlock starts gathering the block [start, start+size).
func NewCompressedBlock(start, size int64) *CompressedBlock {
	return &CompressedBlock{Start: start, Size: size}
}

// Add takes one packet of the block. It returns the block's data once all
// of it arrived (nil before).
func (b *CompressedBlock) Add(d DataPart) ([]byte, error) {
	if !d.Compressed || d.Start != b.Start {
		return nil, ErrBadCompressed
	}
	packed := int64(d.PackedSize)
	if b.packed == 0 {
		if packed == 0 || packed > maxPacked(b.Size) {
			return nil, fmt.Errorf("%w: %d bytes compressed for a %d byte block", ErrBadCompressed, packed, b.Size)
		}
		b.packed = packed
		b.buf = make([]byte, 0, packed)
	}
	if packed != b.packed || int64(len(b.buf)+len(d.Data)) > b.packed {
		return nil, fmt.Errorf("%w: more data than announced", ErrBadCompressed)
	}
	b.buf = append(b.buf, d.Data...)
	if int64(len(b.buf)) < b.packed {
		return nil, nil
	}
	return b.inflate()
}

// Received is how much of the compressed block arrived.
func (b *CompressedBlock) Received() int { return len(b.buf) }

func (b *CompressedBlock) inflate() ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b.buf))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCompressed, err)
	}
	out := make([]byte, b.Size)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCompressed, err)
	}
	// Exactly the block: nothing more may come out.
	var one [1]byte
	if n, _ := r.Read(one[:]); n != 0 {
		return nil, fmt.Errorf("%w: longer than the block", ErrBadCompressed)
	}
	return out, nil
}
