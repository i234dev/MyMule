package ed2k

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"

	"mymule/internal/aich"
	"mymule/internal/proto"
)

// eMule collections (.emulecollection): a list of files to download
// together. They come in eMule's binary format or as text with one
// ed2k:// link per line.

// MaxCollectionSize bounds a collection file, and MaxCollectionFiles the
// files in it.
const (
	MaxCollectionSize  = 16 << 20
	MaxCollectionFiles = 10000
)

// Collection tags (eMule's FT_*).
const (
	collName     = 0x01 // FT_FILENAME
	collSize     = 0x02 // FT_FILESIZE
	collSizeHi   = 0x3A // FT_FILESIZE_HI
	collHash     = 0x28 // FT_FILEHASH
	collAICHHash = 0x27 // FT_AICH_HASH
)

// ErrNotCollection is returned for a file that is not a collection.
var ErrNotCollection = errors.New("not an eMule collection")

var linkRe = regexp.MustCompile(`(?i)ed2k://\|file\|[^\s]+?\|/`)

// ParseCollection reads a collection: its name (empty for a text one) and
// the files in it. Files that do not make a valid link are left out.
func ParseCollection(b []byte) (name string, links []Link, err error) {
	if len(b) > MaxCollectionSize {
		return "", nil, fmt.Errorf("collection too big (%d bytes)", len(b))
	}
	if name, links, err := parseBinaryCollection(b); err == nil {
		return name, links, nil
	}
	for _, s := range linkRe.FindAllString(string(b), MaxCollectionFiles) {
		if l, err := ParseLink(s); err == nil {
			links = append(links, l)
		}
	}
	if len(links) == 0 {
		return "", nil, ErrNotCollection
	}
	return "", links, nil
}

func parseBinaryCollection(b []byte) (string, []Link, error) {
	r := proto.NewReader(b)
	if v := r.U32(); v != 1 && v != 2 { // version 2: files over 4 GB
		return "", nil, ErrNotCollection
	}
	header, err := r.ReadTags(int(r.U32()))
	if err != nil {
		return "", nil, ErrNotCollection
	}
	name := ""
	if t, ok := proto.TagList(header).Find(collName); ok {
		name = cleanTagString(t.Str)
	}
	n := r.U32()
	if r.Err() != nil || n > MaxCollectionFiles {
		return "", nil, ErrNotCollection
	}
	var links []Link
	for range n {
		tags, err := r.ReadTags(int(r.U32()))
		if err != nil {
			return "", nil, ErrNotCollection
		}
		var l Link
		var hasHash bool
		var hi uint64
		for _, t := range tags {
			if t.Name != "" {
				continue
			}
			switch t.ID {
			case collName:
				l.Name = cleanTagString(t.Str)
			case collSize:
				l.Size = int64(t.Int)
			case collSizeHi:
				hi = t.Int
			case collHash:
				if t.Type == proto.TagHash16 && len(t.Bytes) == 16 {
					copy(l.Hash[:], t.Bytes)
					hasHash = true
				}
			case collAICHHash:
				if h, err := aich.ParseHash(t.Str); err == nil {
					l.AICH = h
				}
			}
		}
		if hi > 0 && hi < 1<<31 {
			l.Size = l.Size&0xFFFFFFFF | int64(hi)<<32
		}
		if !hasHash {
			continue
		}
		// The same checks as any link (name, size).
		if ok, err := ParseLink(l.String()); err == nil {
			links = append(links, ok)
		}
	}
	if r.Err() != nil {
		return "", nil, ErrNotCollection
	}
	return name, links, nil
}

// cleanTagString drops the UTF-8 byte order mark eMule may write.
func cleanTagString(s string) string {
	return string(bytes.TrimPrefix([]byte(s), []byte{0xEF, 0xBB, 0xBF}))
}
