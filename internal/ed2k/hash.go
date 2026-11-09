// Package ed2k contains eD2K-level primitives: file hashing and ed2k links.
package ed2k

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"mymule/internal/aich"
	"mymule/internal/md4"
	"mymule/internal/netutil"
)

// PartSize is the size of an eD2K chunk ("part"): 9.28 MB.
const PartSize = 9728000

// StatusPartCount is the number of parts eMule and aMule put in, and
// expect in, a file status (OP_FILESTATUS): size/PartSize + 1. It is one
// more than the real number of parts when the size is an exact multiple
// of PartSize (the empty last part of the hash set); otherwise the same.
func StatusPartCount(size int64) int { return int(size/PartSize) + 1 }

// Hash is a 16-byte MD4 hash as used throughout the eD2K network.
type Hash [16]byte

func (h Hash) String() string { return strings.ToUpper(hex.EncodeToString(h[:])) }

// MarshalText encodes the hash as hex (used for JSON).
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText decodes a hex hash.
func (h *Hash) UnmarshalText(b []byte) error {
	v, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

// IsZero reports whether the hash is all zeros.
func (h Hash) IsZero() bool { return h == Hash{} }

// ParseHash parses a 32-character hex string.
func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return h, fmt.Errorf("invalid ed2k hash %q", s)
	}
	copy(h[:], b)
	return h, nil
}

// FileHashes is the result of hashing a file: the root hash plus the
// MD4 hash of every part (needed later to verify downloaded chunks).
type FileHashes struct {
	Root  Hash
	Parts []Hash
	Size  int64
}

// HashReader computes the eD2K hash of a stream.
//
// Rules (eMule compatible):
//   - size < PartSize: root = MD4(data), single part.
//   - otherwise: root = MD4(concat(MD4(part_i))). When size is an exact
//     multiple of PartSize, an extra MD4 of an empty part is appended,
//     as eMule does.
//
// progress, if non-nil, is called with the number of bytes processed so far.
func HashReader(r io.Reader, progress func(int64)) (*FileHashes, error) {
	res := &FileHashes{}
	buf := make([]byte, 1<<20)
	h := md4.New()
	var inPart int64

	for {
		n, err := r.Read(buf)
		data := buf[:n]
		for len(data) > 0 {
			take := int64(len(data))
			if room := PartSize - inPart; take > room {
				take = room
			}
			h.Write(data[:take])
			inPart += take
			res.Size += take
			data = data[take:]
			if inPart == PartSize {
				var p Hash
				copy(p[:], h.Sum(nil))
				res.Parts = append(res.Parts, p)
				h.Reset()
				inPart = 0
			}
		}
		if progress != nil && n > 0 {
			progress(res.Size)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	// Final (possibly empty) part. eMule always appends it, including the
	// empty one when the size is an exact multiple of PartSize.
	var last Hash
	copy(last[:], h.Sum(nil))
	res.Parts = append(res.Parts, last)

	if len(res.Parts) == 1 {
		res.Root = res.Parts[0]
		return res, nil
	}
	root := md4.New()
	for _, p := range res.Parts {
		root.Write(p[:])
	}
	copy(res.Root[:], root.Sum(nil))
	return res, nil
}

// HashFile computes the eD2K hash of a file on disk.
func HashFile(path string, progress func(int64)) (*FileHashes, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return HashReader(f, progress)
}

// Link is a parsed ed2k://|file|name|size|hash|/ link.
type Link struct {
	Name string
	Size int64
	Hash Hash
	// AICH is the file's AICH hash, from an h= field (zero: none).
	AICH aich.Hash
}

func (l Link) String() string {
	if !l.AICH.IsZero() {
		return fmt.Sprintf("ed2k://|file|%s|%d|%s|h=%s|/", url.PathEscape(l.Name), l.Size, l.Hash, l.AICH)
	}
	return fmt.Sprintf("ed2k://|file|%s|%d|%s|/", url.PathEscape(l.Name), l.Size, l.Hash)
}

// MaxFileSize is the largest file eD2K supports (eMule's limit, 256 GiB).
// Sizes come from links, servers and peers; anything outside 1..MaxFileSize
// is rejected before it is used, since a download allocates per part.
const MaxFileSize int64 = 256 << 30

// ValidSize reports whether n is a usable eD2K file size.
func ValidSize(n int64) bool { return n > 0 && n <= MaxFileSize }

// ParseLink parses an ed2k file link. Of the extra fields, the AICH hash
// (h=) is read; others (sources, part hashes) are ignored, and so is an
// AICH hash that cannot be read.
func ParseLink(s string) (Link, error) {
	var l Link
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "ed2k://|file|") {
		return l, fmt.Errorf("not an ed2k file link")
	}
	parts := strings.Split(s[len("ed2k://|file|"):], "|")
	if len(parts) < 3 {
		return l, fmt.Errorf("malformed ed2k link")
	}
	name, err := url.PathUnescape(parts[0])
	if err != nil {
		name = parts[0]
	}
	name = netutil.CleanText(name, 1024)
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !ValidSize(size) {
		return l, fmt.Errorf("invalid size in ed2k link")
	}
	h, err := ParseHash(parts[2])
	if err != nil {
		return l, err
	}
	l = Link{Name: name, Size: size, Hash: h}
	for _, f := range parts[3:] {
		if len(f) > 2 && strings.EqualFold(f[:2], "h=") {
			if a, err := aich.ParseHash(f[2:]); err == nil {
				l.AICH = a
			}
		}
	}
	return l, nil
}
