// Package server implements the eD2K client <-> server protocol and the
// server.met server list format.
package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"

	"mymule/internal/netutil"
	"mymule/internal/proto"
)

// Info describes a known eD2K server.
type Info struct {
	IP          net.IP
	Port        uint16
	Name        string
	Description string
	Users       uint32
	Files       uint32
	MaxUsers    uint32
	Fails       uint32
	Preference  uint32
	Version     uint32
	UDPFlags    uint32
	ObfuTCPPort uint16
	Tags        proto.TagList
}

func (s Info) Addr() string { return net.JoinHostPort(s.IP.String(), strconv.Itoa(int(s.Port))) }

func (s Info) String() string {
	name := s.Name
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("%-21s %s", s.Addr(), name)
}

// IPFromU32 decodes an eD2K IPv4 address (network-order bytes read as LE u32).
func IPFromU32(v uint32) net.IP {
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).To4()
}

// IPToU32 is the inverse of IPFromU32.
func IPToU32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(ip4)
}

// ParseServerMet parses a server.met file.
//
// Layout: [header u8 0x0E|0xE0][count u32] then per server:
// [ip u32][port u16][tagcount u32][tags...]
func ParseServerMet(data []byte) ([]Info, error) {
	r := proto.NewReader(data)
	hdr := r.U8()
	if hdr != 0x0E && hdr != 0xE0 {
		return nil, fmt.Errorf("server.met: bad header 0x%02X", hdr)
	}
	count := int(r.U32())
	if count > 100000 {
		return nil, fmt.Errorf("server.met: implausible count %d", count)
	}
	var out []Info
	for i := 0; i < count; i++ {
		ip := IPFromU32(r.U32())
		port := r.U16()
		ntags := int(r.U32())
		if r.Err() != nil {
			return out, fmt.Errorf("server.met: truncated at entry %d", i)
		}
		tags, err := r.ReadTags(ntags)
		if err != nil {
			return out, fmt.Errorf("server.met: entry %d: %w", i, err)
		}
		out = append(out, infoFromTags(ip, port, tags))
	}
	return out, nil
}

func infoFromTags(ip net.IP, port uint16, tags proto.TagList) Info {
	s := Info{IP: ip, Port: port, Tags: tags}
	for _, t := range tags {
		if t.Name != "" {
			// Old-style string names.
			switch t.Name {
			case "users":
				s.Users = uint32(t.Int)
			case "files":
				s.Files = uint32(t.Int)
			case "maxusers":
				s.MaxUsers = uint32(t.Int)
			}
			continue
		}
		switch t.ID {
		case proto.STServerName:
			s.Name = netutil.CleanText(t.Str, 128)
		case proto.STDescription:
			s.Description = netutil.CleanText(t.Str, 256)
		case proto.STFail:
			s.Fails = uint32(t.Int)
		case proto.STPreference:
			s.Preference = uint32(t.Int)
		case proto.STMaxUsers:
			s.MaxUsers = uint32(t.Int)
		case proto.STVersion:
			s.Version = uint32(t.Int)
		case proto.STUDPFlags:
			s.UDPFlags = uint32(t.Int)
		case proto.STTCPPortObfu:
			s.ObfuTCPPort = uint16(t.Int)
		}
	}
	return s
}

// LoadServerMet reads and parses a server.met file from disk.
func LoadServerMet(path string) ([]Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseServerMet(data)
}

// WriteServerMet serialises a server list in server.met format.
func WriteServerMet(w io.Writer, list []Info) error {
	var b proto.Writer
	b.U8(0xE0)
	b.U32(uint32(len(list)))
	for _, s := range list {
		b.U32(IPToU32(s.IP))
		b.U16(s.Port)
		var tags []proto.Tag
		if s.Name != "" {
			tags = append(tags, proto.StrTag(proto.STServerName, s.Name))
		}
		if s.Description != "" {
			tags = append(tags, proto.StrTag(proto.STDescription, s.Description))
		}
		tags = append(tags,
			proto.Tag{Name: "users", Type: proto.TagUint32, Int: uint64(s.Users)},
			proto.Tag{Name: "files", Type: proto.TagUint32, Int: uint64(s.Files)},
			proto.IntTag(proto.STFail, s.Fails),
		)
		b.U32(uint32(len(tags)))
		for _, t := range tags {
			b.WriteTag(t)
		}
	}
	_, err := w.Write(b.B)
	return err
}

// SaveServerMet writes a server list to disk atomically.
func SaveServerMet(path string, list []Info) error {
	var buf bytes.Buffer
	if err := WriteServerMet(&buf, list); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SortByQuality orders servers: fewest failures first, then most users.
func SortByQuality(list []Info) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Fails != list[j].Fails {
			return list[i].Fails < list[j].Fails
		}
		return list[i].Users > list[j].Users
	})
}
