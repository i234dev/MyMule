// Package netutil holds network checks shared by the protocol code.
package netutil

import (
	"net"
	"net/netip"
)

// nonPublic lists IPv4 ranges that are never a peer on the Internet:
// this host, private networks, carrier-grade NAT, link-local, test and
// documentation networks, multicast and reserved space.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // private
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
}

// IsPublicIPv4 reports whether ip is an IPv4 address that can belong to a
// host on the public Internet. Addresses received from servers or peers
// that fail this check must not be connected to: they point at the user's
// own machine or network, or at nothing.
func IsPublicIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	addr := netip.AddrFrom4([4]byte(ip4))
	for _, p := range nonPublic {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}
