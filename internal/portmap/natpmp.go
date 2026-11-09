package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// NATPMPPort is the port routers take NAT-PMP requests on (RFC 6886).
const NATPMPPort = 5351

// natpmpTries is how many times a request is sent (250 ms, then twice as
// long each time: about 4 s in all).
const natpmpTries = 4

// natpmpMap asks the gateway to map external to our internal port, for
// lifetime seconds (0 deletes the mapping). It returns the external port
// and lifetime the gateway gave.
func natpmpMap(ctx context.Context, gw netip.AddrPort, proto string, internal, external uint16, lifetime uint32) (uint16, uint32, error) {
	var op byte
	switch proto {
	case "udp":
		op = 1
	case "tcp":
		op = 2
	default:
		return 0, 0, fmt.Errorf("protocol %q", proto)
	}
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(gw))
	if err != nil {
		return 0, 0, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()

	req := make([]byte, 12)
	req[1] = op
	binary.BigEndian.PutUint16(req[4:], internal)
	binary.BigEndian.PutUint16(req[6:], external)
	binary.BigEndian.PutUint32(req[8:], lifetime)
	wait := 250 * time.Millisecond
	buf := make([]byte, 64)
	for range natpmpTries {
		if _, err := c.Write(req); err != nil {
			return 0, 0, err
		}
		c.SetReadDeadline(time.Now().Add(wait))
		for {
			n, err := c.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return 0, 0, ctx.Err()
				}
				break // timed out (or ICMP unreachable): send again
			}
			// version 0, our opcode + 128, result, epoch, ports, lifetime
			if n < 16 || buf[0] != 0 || buf[1] != 128+op || binary.BigEndian.Uint16(buf[8:]) != internal {
				continue
			}
			if code := binary.BigEndian.Uint16(buf[2:]); code != 0 {
				return 0, 0, natpmpError(code)
			}
			return binary.BigEndian.Uint16(buf[10:]), binary.BigEndian.Uint32(buf[12:]), nil
		}
		wait *= 2
	}
	return 0, 0, errNoAnswer
}

var errNoAnswer = errors.New("no answer")

type natpmpError uint16

func (e natpmpError) Error() string {
	switch e {
	case 1:
		return "NAT-PMP: unsupported version"
	case 2:
		return "NAT-PMP: refused by the router (disabled?)"
	case 3:
		return "NAT-PMP: the router has no network"
	case 4:
		return "NAT-PMP: the router is out of resources"
	case 5:
		return "NAT-PMP: unsupported request"
	}
	return fmt.Sprintf("NAT-PMP: error %d", uint16(e))
}
