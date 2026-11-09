package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SSDPAddr is where UPnP devices are searched for.
const SSDPAddr = "239.255.255.250:1900"

// ssdpWait is how long answers to a search are waited for (a variable
// for tests).
var ssdpWait = 3 * time.Second

// maxXML bounds a device description or a SOAP answer.
const maxXML = 1 << 20

// What is searched for, and the services that map ports, best first.
var (
	ssdpTargets = []string{
		"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
		"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	}
	wanServices = []string{
		"urn:schemas-upnp-org:service:WANIPConnection:2",
		"urn:schemas-upnp-org:service:WANIPConnection:1",
		"urn:schemas-upnp-org:service:WANPPPConnection:1",
	}
)

// igd is a router's port mapping service.
type igd struct {
	control string     // SOAP control URL
	service string     // service type
	local   netip.Addr // our address on the router's network
}

// httpClient talks to the router: it does not follow redirects.
var httpClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// discover searches for a router with a port mapping service, sending the
// search to ssdp (SSDPAddr; tests use another).
func discover(ctx context.Context, ssdp string) (*igd, error) {
	to, err := net.ResolveUDPAddr("udp4", ssdp)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	for _, st := range ssdpTargets {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + SSDPAddr + "\r\nST: " + st + "\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n\r\n"
		if _, err := c.WriteToUDP([]byte(msg), to); err != nil {
			return nil, err
		}
	}
	c.SetReadDeadline(time.Now().Add(ssdpWait))
	tried := map[string]bool{}
	buf := make([]byte, 2048)
	var last error = errNoAnswer
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, last
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if loc == "" || tried[loc] {
			continue
		}
		tried[loc] = true
		g, err := describe(ctx, loc, from.AddrPort().Addr().Unmap())
		if err != nil {
			last = err
			continue
		}
		return g, nil
	}
}

// sameHost: the device description, and the control URL in it, must be
// on the device that answered the search, and on our network: an answer
// cannot send us to ask anything of another host.
func sameHost(u *url.URL, from netip.Addr) bool {
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && u.Scheme == "http" && ip.Unmap() == from && (from.IsPrivate() || from.IsLoopback() || from.IsLinkLocalUnicast())
}

type xService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type xDevice struct {
	Services []xService `xml:"serviceList>service"`
	Devices  []xDevice  `xml:"deviceList>device"`
}

type xRoot struct {
	URLBase string  `xml:"URLBase"`
	Device  xDevice `xml:"device"`
}

func (d *xDevice) find(service string) string {
	for _, s := range d.Services {
		if strings.TrimSpace(s.ServiceType) == service {
			return strings.TrimSpace(s.ControlURL)
		}
	}
	for i := range d.Devices {
		if c := d.Devices[i].find(service); c != "" {
			return c
		}
	}
	return ""
}

// describe reads a router's device description and finds its port
// mapping service.
func describe(ctx context.Context, location string, from netip.Addr) (*igd, error) {
	loc, err := url.Parse(location)
	if err != nil || !sameHost(loc, from) {
		return nil, fmt.Errorf("UPnP: device description at %q, not on the device that answered", location)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", loc.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UPnP: device description: HTTP %s", resp.Status)
	}
	var root xRoot
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxXML)).Decode(&root); err != nil {
		return nil, fmt.Errorf("UPnP: device description: %v", err)
	}
	base := loc
	if root.URLBase != "" {
		if b, err := url.Parse(strings.TrimSpace(root.URLBase)); err == nil {
			base = b
		}
	}
	for _, svc := range wanServices {
		ctl := root.Device.find(svc)
		if ctl == "" {
			continue
		}
		u, err := base.Parse(ctl)
		if err != nil || !sameHost(u, from) {
			return nil, fmt.Errorf("UPnP: control URL %q not on the device", ctl)
		}
		local, err := localAddrTo(from)
		if err != nil {
			return nil, err
		}
		return &igd{control: u.String(), service: svc, local: local}, nil
	}
	return nil, errors.New("UPnP: the router offers no port mapping")
}

// localAddrTo returns our address on the way to ip (no packet is sent).
func localAddrTo(ip netip.Addr) (netip.Addr, error) {
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, 1900)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

// upnpError is an error a router answered a SOAP action with.
type upnpError struct {
	code int
	desc string
}

func (e *upnpError) Error() string {
	return fmt.Sprintf("UPnP: error %d (%s)", e.code, e.desc)
}

var (
	reErrorCode = regexp.MustCompile(`<(?:\w+:)?errorCode>\s*(\d+)\s*</`)
	reErrorDesc = regexp.MustCompile(`<(?:\w+:)?errorDescription>([^<]{0,200})</`)
)

// call runs a SOAP action; args are name, value pairs in order.
func (g *igd) call(ctx context.Context, action string, args ...string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\r\n")
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<u:` + action + ` xmlns:u="` + g.service + `">`)
	for i := 0; i+1 < len(args); i += 2 {
		b.WriteString("<" + args[i] + ">")
		xml.EscapeText(&b, []byte(args[i+1]))
		b.WriteString("</" + args[i] + ">")
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>` + "\r\n")
	req, err := http.NewRequestWithContext(ctx, "POST", g.control, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+g.service+"#"+action+`"`)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxXML))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if m := reErrorCode.FindSubmatch(body); m != nil {
			code, _ := strconv.Atoi(string(m[1]))
			e := &upnpError{code: code}
			if d := reErrorDesc.FindSubmatch(body); d != nil {
				e.desc = strings.TrimSpace(string(d[1]))
			}
			return nil, e
		}
		return nil, fmt.Errorf("UPnP: %s: HTTP %s", action, resp.Status)
	}
	return body, nil
}

// Errors routers give when adding a mapping.
const (
	upnpConflict      = 718 // the port is mapped to another computer
	upnpOnlyPermanent = 725 // leases must be 0 (no expiry)
)

// add maps port (same number outside and in) to us for lease seconds;
// it returns the lease used (0 when the router only takes permanent
// mappings).
func (g *igd) add(ctx context.Context, proto string, port uint16, lease uint32, desc string) (uint32, error) {
	p := strconv.Itoa(int(port))
	try := func(lease uint32) error {
		_, err := g.call(ctx, "AddPortMapping",
			"NewRemoteHost", "",
			"NewExternalPort", p,
			"NewProtocol", strings.ToUpper(proto),
			"NewInternalPort", p,
			"NewInternalClient", g.local.String(),
			"NewEnabled", "1",
			"NewPortMappingDescription", desc,
			"NewLeaseDuration", strconv.FormatUint(uint64(lease), 10))
		return err
	}
	err := try(lease)
	var ue *upnpError
	if errors.As(err, &ue) && ue.code == upnpOnlyPermanent {
		return 0, try(0)
	}
	if errors.As(err, &ue) && ue.code == upnpConflict {
		return 0, fmt.Errorf("UPnP: %s port %d is already mapped to another computer", strings.ToUpper(proto), port)
	}
	return lease, err
}

// remove deletes a mapping.
func (g *igd) remove(ctx context.Context, proto string, port uint16) error {
	_, err := g.call(ctx, "DeletePortMapping",
		"NewRemoteHost", "",
		"NewExternalPort", strconv.Itoa(int(port)),
		"NewProtocol", strings.ToUpper(proto))
	return err
}
