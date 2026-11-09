// Package ipfilter reads IP filter lists (eMule's ipfilter.dat, and the
// P2P format of PeerGuardian) and tells whether an address is in them.
package ipfilter

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// Level is eMule's: ranges whose access level is below it are blocked.
const Level = 127

// MaxRanges bounds a list (the usual ones have a few hundred thousand).
const MaxRanges = 2_000_000

// maxLine bounds a line read.
const maxLine = 1024

type span struct{ lo, hi uint32 }

// Filter is a set of IPv4 ranges.
type Filter struct {
	spans []span // sorted, not overlapping
}

// Parse reads a list in either format, one range per line:
//
//	eMule:  1.2.3.0 - 1.2.3.255 , 100 , description
//	P2P:    description:1.2.3.0-1.2.3.255
//
// Comments (# or //) and lines it cannot read are skipped; eMule ranges
// at Level or above are allowed ones, and skipped too. It reports how
// many lines were skipped as unreadable.
func Parse(r io.Reader) (*Filter, int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	var spans []span
	bad := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		s, ok, allowed := parseLine(line)
		if allowed {
			continue
		}
		if !ok {
			bad++
			continue
		}
		if len(spans) >= MaxRanges {
			return nil, bad, errors.New("ipfilter: more than 2,000,000 ranges")
		}
		spans = append(spans, s)
	}
	if err := sc.Err(); err != nil {
		return nil, bad, err
	}
	return &Filter{spans: merge(spans)}, bad, nil
}

// parseLine reads one range; allowed: an eMule range of an allowed level.
func parseLine(line string) (s span, ok, allowed bool) {
	// eMule: "a - b , level , description"
	if fields := strings.SplitN(line, ",", 3); len(fields) >= 2 && strings.Contains(fields[0], "-") {
		lo, hi, ok := parseRange(fields[0])
		if !ok {
			return span{}, false, false
		}
		level, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return span{}, false, false
		}
		if level >= Level {
			return span{}, false, true
		}
		return span{lo, hi}, true, false
	}
	// P2P: "description:a-b" (the description may hold ':').
	if i := strings.LastIndexByte(line, ':'); i >= 0 {
		lo, hi, ok := parseRange(line[i+1:])
		return span{lo, hi}, ok, false
	}
	return span{}, false, false
}

func parseRange(s string) (uint32, uint32, bool) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, false
	}
	lo, ok1 := parseIP(a)
	hi, ok2 := parseIP(b)
	if !ok1 || !ok2 || hi < lo {
		return 0, 0, false
	}
	return lo, hi, true
}

// parseIP reads an IPv4 address, with leading zeros as eMule writes them
// (001.002.003.004).
func parseIP(s string) (uint32, bool) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 {
		return 0, false
	}
	var v uint32
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return 0, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return 0, false
		}
		v = v<<8 | uint32(n)
	}
	return v, true
}

// merge sorts spans and joins those that overlap or touch.
func merge(spans []span) []span {
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	var out []span
	for _, s := range spans {
		if n := len(out); n > 0 && (s.lo <= out[n-1].hi || (out[n-1].hi != ^uint32(0) && s.lo == out[n-1].hi+1)) {
			out[n-1].hi = max(out[n-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	return out
}

// Blocked reports whether ip is in the list (IPv4 only; nil filter: no).
func (f *Filter) Blocked(ip netip.Addr) bool {
	if f == nil {
		return false
	}
	ip = ip.Unmap()
	if !ip.Is4() {
		return false
	}
	a := ip.As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	i := sort.Search(len(f.spans), func(i int) bool { return f.spans[i].hi >= v })
	return i < len(f.spans) && f.spans[i].lo <= v
}

// Len returns how many ranges the list holds (after joining).
func (f *Filter) Len() int {
	if f == nil {
		return 0
	}
	return len(f.spans)
}

// MaxFile bounds a list file, once unpacked (the usual ones are 10-20 MB).
const MaxFile = 128 << 20

// Decode reads a list as stored or downloaded: plain text, gzip, or a zip
// holding it (its first file).
func Decode(b []byte) (*Filter, int, error) {
	switch {
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, 0, err
		}
		return parseBounded(zr)
	case len(b) >= 4 && string(b[:4]) == "PK\x03\x04":
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return nil, 0, err
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, 0, err
			}
			defer rc.Close()
			return parseBounded(rc)
		}
		return nil, 0, errors.New("ipfilter: empty zip")
	}
	return parseBounded(bytes.NewReader(b))
}

// parseBounded parses at most MaxFile bytes, failing past them.
func parseBounded(r io.Reader) (*Filter, int, error) {
	lr := &io.LimitedReader{R: r, N: MaxFile + 1}
	f, bad, err := Parse(lr)
	if err == nil && lr.N <= 0 {
		return nil, bad, errors.New("ipfilter: list too big")
	}
	return f, bad, err
}

// Load reads a list file (see Decode).
func Load(path string) (*Filter, int, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, MaxFile+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > MaxFile {
		return nil, 0, errors.New("ipfilter: list too big")
	}
	return Decode(b)
}

// Holder holds the filter in use, which can be swapped while it is read.
// The zero value blocks nothing; a nil *Holder too.
type Holder struct {
	p atomic.Pointer[Filter]
}

// Set swaps the filter in use (nil: none).
func (h *Holder) Set(f *Filter) { h.p.Store(f) }

// Get returns the filter in use (nil: none).
func (h *Holder) Get() *Filter {
	if h == nil {
		return nil
	}
	return h.p.Load()
}

// Blocked reports whether ip is blocked by the filter in use.
func (h *Holder) Blocked(ip netip.Addr) bool { return h.Get().Blocked(ip) }

// BlockedIP is Blocked for a net.IP (nil or unreadable: not blocked).
func (h *Holder) BlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && h.Blocked(a)
}

// PacketConn wraps a UDP socket so that packets from blocked addresses
// are dropped as read and packets to them are not sent.
func PacketConn(c net.PacketConn, h *Holder) net.PacketConn {
	return &filteredConn{PacketConn: c, h: h}
}

type filteredConn struct {
	net.PacketConn
	h *Holder
}

func addrIP(a net.Addr) (netip.Addr, bool) {
	if ua, ok := a.(*net.UDPAddr); ok {
		return netip.AddrFromSlice(ua.IP)
	}
	return netip.Addr{}, false
}

func (c *filteredConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, from, err := c.PacketConn.ReadFrom(b)
		if err == nil {
			if ip, ok := addrIP(from); ok && c.h.Blocked(ip) {
				continue
			}
		}
		return n, from, err
	}
}

func (c *filteredConn) WriteTo(b []byte, to net.Addr) (int, error) {
	if ip, ok := addrIP(to); ok && c.h.Blocked(ip) {
		return len(b), nil // as if sent, and lost
	}
	return c.PacketConn.WriteTo(b, to)
}
