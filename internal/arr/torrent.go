// Package arr lets Radarr, Sonarr, Lidarr, Readarr and Prowlarr use MyMule.
//
// Those apps only talk to torrent/usenet indexers (Torznab) and to
// download clients such as qBittorrent. MyMule therefore offers:
//
//   - a Torznab indexer (/torznab/api) that searches the eD2K network, and
//   - a subset of the qBittorrent Web API (/api/v2/...) that accepts the
//     "torrents" the indexer handed out and reports download progress.
//
// The torrents are stand-ins: a small, valid .torrent whose info
// dictionary carries the ed2k link. The apps identify downloads by the
// torrent's info hash, which is derived deterministically from the link,
// so it can be recomputed at any time.
package arr

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"mymule/internal/ed2k"
)

// ---- minimal bencode ----

func bencode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case string:
		fmt.Fprintf(buf, "%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(buf, "%d:", len(x))
		buf.Write(x)
	case int:
		fmt.Fprintf(buf, "i%de", x)
	case int64:
		fmt.Fprintf(buf, "i%de", x)
	case []any:
		buf.WriteByte('l')
		for _, e := range x {
			if err := bencode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // bencode dictionaries are sorted by key
		buf.WriteByte('d')
		for _, k := range keys {
			bencode(buf, k)
			if err := bencode(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('e')
	default:
		return fmt.Errorf("bencode: unsupported type %T", v)
	}
	return nil
}

// bdecode parses one value; dictionaries become map[string]any, strings
// []byte, integers int64. It also returns the raw bytes of the "info"
// dictionary when decoding a torrent's top level.
func bdecode(b []byte) (any, error) {
	v, rest, err := bdecodeAt(b, 0)
	if err != nil {
		return nil, err
	}
	if rest != len(b) {
		return nil, errors.New("bencode: trailing data")
	}
	return v, nil
}

func bdecodeAt(b []byte, i int) (any, int, error) {
	if i >= len(b) {
		return nil, i, errors.New("bencode: unexpected end")
	}
	switch c := b[i]; {
	case c == 'i':
		j := bytes.IndexByte(b[i:], 'e')
		if j < 0 {
			return nil, i, errors.New("bencode: bad integer")
		}
		n, err := strconv.ParseInt(string(b[i+1:i+j]), 10, 64)
		return n, i + j + 1, err
	case c == 'l':
		var list []any
		i++
		for i < len(b) && b[i] != 'e' {
			v, next, err := bdecodeAt(b, i)
			if err != nil {
				return nil, i, err
			}
			list = append(list, v)
			i = next
		}
		return list, i + 1, nil
	case c == 'd':
		m := map[string]any{}
		i++
		for i < len(b) && b[i] != 'e' {
			k, next, err := bdecodeAt(b, i)
			if err != nil {
				return nil, i, err
			}
			key, ok := k.([]byte)
			if !ok {
				return nil, i, errors.New("bencode: non-string key")
			}
			v, next2, err := bdecodeAt(b, next)
			if err != nil {
				return nil, i, err
			}
			m[string(key)] = v
			i = next2
		}
		return m, i + 1, nil
	case c >= '0' && c <= '9':
		j := bytes.IndexByte(b[i:], ':')
		if j < 0 {
			return nil, i, errors.New("bencode: bad string")
		}
		n, err := strconv.Atoi(string(b[i : i+j]))
		start := i + j + 1
		if err != nil || n < 0 || start+n > len(b) {
			return nil, i, errors.New("bencode: bad string length")
		}
		return b[start : start+n], start + n, nil
	}
	return nil, i, fmt.Errorf("bencode: unexpected byte %q", b[i])
}

// ---- stand-in torrents ----

const (
	pieceLength = 1 << 24 // 16 MiB, a power of two as clients expect
	linkKey     = "x-ed2k-link"
)

func infoDict(l ed2k.Link) map[string]any {
	pieces := int((l.Size + pieceLength - 1) / pieceLength)
	return map[string]any{
		"name":         l.Name,
		"length":       l.Size,
		"piece length": pieceLength,
		"pieces":       make([]byte, 20*max(pieces, 1)),
		"private":      1, // keep real BitTorrent clients off it
		linkKey:        l.String(),
	}
}

// InfoHash returns the BitTorrent info hash the *arr apps will use for
// the download of l (lowercase hex, as qBittorrent reports it).
func InfoHash(l ed2k.Link) string {
	var buf bytes.Buffer
	bencode(&buf, infoDict(l))
	sum := sha1.Sum(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// Torrent builds the stand-in .torrent file for l.
func Torrent(l ed2k.Link) []byte {
	var buf bytes.Buffer
	bencode(&buf, map[string]any{
		"announce":   "http://mymule.invalid/announce",
		"comment":    "eD2K download for MyMule: " + l.String(),
		"created by": "MyMule",
		"info":       infoDict(l),
	})
	return buf.Bytes()
}

// LinkFromTorrent extracts the ed2k link from a stand-in torrent.
func LinkFromTorrent(data []byte) (ed2k.Link, error) {
	v, err := bdecode(data)
	if err != nil {
		return ed2k.Link{}, err
	}
	top, ok := v.(map[string]any)
	if !ok {
		return ed2k.Link{}, errors.New("not a torrent")
	}
	info, ok := top["info"].(map[string]any)
	if !ok {
		return ed2k.Link{}, errors.New("torrent has no info")
	}
	raw, ok := info[linkKey].([]byte)
	if !ok {
		return ed2k.Link{}, errors.New("this torrent was not made by MyMule (no ed2k link inside)")
	}
	return ed2k.ParseLink(string(raw))
}
