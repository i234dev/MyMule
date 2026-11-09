package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNATPMP answers NAT-PMP requests; giveOther makes it give another
// external port than asked, code answers with an error.
type fakeNATPMP struct {
	conn      *net.UDPConn
	mu        sync.Mutex
	mapped    map[string]uint32 // "tcp 4662" -> lifetime
	giveOther bool
	code      uint16
}

func newFakeNATPMP(t *testing.T) *fakeNATPMP {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNATPMP{conn: c, mapped: map[string]uint32{}}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n != 12 || buf[0] != 0 {
				continue
			}
			op := buf[1]
			in := binary.BigEndian.Uint16(buf[4:])
			ext := binary.BigEndian.Uint16(buf[6:])
			life := binary.BigEndian.Uint32(buf[8:])
			proto := map[byte]string{1: "udp", 2: "tcp"}[op]
			f.mu.Lock()
			if f.giveOther && life > 0 {
				ext++
			}
			if life == 0 {
				delete(f.mapped, fmt.Sprintf("%s %d", proto, in))
			} else if f.code == 0 {
				f.mapped[fmt.Sprintf("%s %d", proto, in)] = life
			}
			code := f.code
			f.mu.Unlock()
			resp := make([]byte, 16)
			resp[1] = 128 + op
			binary.BigEndian.PutUint16(resp[2:], code)
			binary.BigEndian.PutUint16(resp[8:], in)
			binary.BigEndian.PutUint16(resp[10:], ext)
			binary.BigEndian.PutUint32(resp[12:], life)
			c.WriteToUDP(resp, from)
		}
	}()
	return f
}

func (f *fakeNATPMP) addr() netip.AddrPort { return f.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

func (f *fakeNATPMP) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.mapped[key]
	return ok
}

// noGateway: no NAT-PMP router.
func noGateway() (netip.AddrPort, error) { return netip.AddrPort{}, fmt.Errorf("no gateway") }

// nowhere is a UDP address nothing answers on.
func nowhere(t *testing.T) string {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.LocalAddr().String()
}

func TestMain(m *testing.M) {
	ssdpWait = 500 * time.Millisecond
	os.Exit(m.Run())
}

func ports() []Port { return []Port{{"tcp", 4662}, {"udp", 4672}} }

// With NAT-PMP, both ports are mapped, the status says so, and they are
// removed when the mapper stops.
func TestNATPMP(t *testing.T) {
	r := newFakeNATPMP(t)
	var logs []string
	var logMu sync.Mutex
	m := New(Config{Ports: ports(), SSDP: nowhere(t),
		Gateway: func() (netip.AddrPort, error) { return r.addr(), nil },
		Log: func(f string, a ...any) {
			logMu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			logMu.Unlock()
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, func() bool { return m.State().Method == "NAT-PMP" })
	if !r.has("tcp 4662") || !r.has("udp 4672") {
		t.Fatalf("mapped %v", r.mapped)
	}
	cancel()
	<-done
	if r.has("tcp 4662") || r.has("udp 4672") {
		t.Fatal("mappings left when stopping")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "TCP 4662 and UDP 4672 with NAT-PMP") {
		t.Fatalf("logs %q", logs)
	}
}

// A router that gives another external port, or an error, is no use:
// the mapping is undone and the error said.
func TestNATPMPRefused(t *testing.T) {
	r := newFakeNATPMP(t)
	r.giveOther = true
	m := New(Config{Ports: ports()[:1]})
	_, err := m.natpmpOpen(context.Background(), r.addr())
	if err == nil || !strings.Contains(err.Error(), "gave TCP port 4663") {
		t.Fatalf("other port: %v", err)
	}
	r.giveOther, r.code = false, 2
	if _, err := m.natpmpOpen(context.Background(), r.addr()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refused: %v", err)
	}
}

// fakeIGD is a UPnP router: an SSDP responder and its HTTP server.
type fakeIGD struct {
	ssdp     *net.UDPConn
	http     *httptest.Server
	location string // what SSDP answers point to (default: our description)
	mu       sync.Mutex
	mapped   map[string]string // "TCP 4662" -> internal client
	leases   []string
	onlyPerm bool
	conflict bool
}

var reArg = regexp.MustCompile(`<(New\w+)>([^<]*)</`)

func newFakeIGD(t *testing.T) *fakeIGD {
	f := &fakeIGD{mapped: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
<deviceList><device><deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
<deviceList><device><deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
<serviceList><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
<controlURL>/ctl/IPConn</controlURL></service></serviceList></device></deviceList></device></deviceList></device></root>`)
	})
	mux.HandleFunc("/ctl/IPConn", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		args := map[string]string{}
		for _, m := range reArg.FindAllStringSubmatch(string(b), -1) {
			args[m[1]] = m[2]
		}
		key := args["NewProtocol"] + " " + args["NewExternalPort"]
		f.mu.Lock()
		defer f.mu.Unlock()
		fail := func(code int) {
			w.WriteHeader(500)
			fmt.Fprintf(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>%d</errorCode><errorDescription>test</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code)
		}
		switch {
		case strings.Contains(r.Header.Get("SOAPAction"), "#AddPortMapping"):
			f.leases = append(f.leases, args["NewLeaseDuration"])
			if f.conflict {
				fail(718)
				return
			}
			if f.onlyPerm && args["NewLeaseDuration"] != "0" {
				fail(725)
				return
			}
			f.mapped[key] = args["NewInternalClient"]
		case strings.Contains(r.Header.Get("SOAPAction"), "#DeletePortMapping"):
			delete(f.mapped, key)
		}
		fmt.Fprint(w, `<s:Envelope><s:Body></s:Body></s:Envelope>`)
	})
	f.http = httptest.NewServer(mux)
	t.Cleanup(f.http.Close)
	f.location = f.http.URL + "/desc.xml"
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f.ssdp = c
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !strings.HasPrefix(string(buf[:n]), "M-SEARCH") {
				continue
			}
			f.mu.Lock()
			loc := f.location
			f.mu.Unlock()
			c.WriteToUDP([]byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nLOCATION: "+loc+"\r\n\r\n"), from)
		}
	}()
	return f
}

func (f *fakeIGD) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.mapped[key]
	return ok
}

// Without NAT-PMP, UPnP maps both ports to our address, and removes them
// when the mapper stops.
func TestUPnP(t *testing.T) {
	r := newFakeIGD(t)
	m := New(Config{Ports: ports(), SSDP: r.ssdp.LocalAddr().String(), Gateway: noGateway})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, func() bool { return m.State().Method == "UPnP" })
	r.mu.Lock()
	client := r.mapped["TCP 4662"]
	r.mu.Unlock()
	if client != "127.0.0.1" || !r.has("UDP 4672") {
		t.Fatalf("mapped %v", r.mapped)
	}
	cancel()
	<-done
	if r.has("TCP 4662") || r.has("UDP 4672") {
		t.Fatal("mappings left when stopping")
	}
}

// A router that only takes permanent mappings gets lease 0; one where the
// port is another computer's says so.
func TestUPnPRouterAnswers(t *testing.T) {
	r := newFakeIGD(t)
	r.onlyPerm = true
	g, err := discover(context.Background(), r.ssdp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := g.add(context.Background(), "tcp", 4662, 3600, "MyMule"); err != nil || lease != 0 || !r.has("TCP 4662") {
		t.Fatalf("permanent only: lease %d, %v", lease, err)
	}
	r.conflict = true
	if _, err := g.add(context.Background(), "udp", 4672, 3600, "MyMule"); err == nil || !strings.Contains(err.Error(), "another computer") {
		t.Fatalf("conflict: %v", err)
	}
}

// An SSDP answer cannot send us to another host, or to a public address:
// the description must be on the device that answered.
func TestUPnPOnlyTheDeviceThatAnswered(t *testing.T) {
	r := newFakeIGD(t)
	for _, loc := range []string{
		"http://198.51.100.1:80/desc.xml",
		"https://127.0.0.1/desc.xml",
		"file:///etc/passwd",
	} {
		r.mu.Lock()
		r.location = loc
		r.mu.Unlock()
		if _, err := discover(context.Background(), r.ssdp.LocalAddr().String()); err == nil {
			t.Errorf("%s: taken", loc)
		}
	}
	if sameHost(mustURL("http://198.51.100.1/x"), netip.MustParseAddr("198.51.100.1")) {
		t.Fatal("public device taken")
	}
}

// Nothing answers: the status says why, once in the log.
func TestNoRouter(t *testing.T) {
	var logs []string
	m := New(Config{Ports: ports(), SSDP: nowhere(t), Gateway: noGateway,
		Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	if _, _, _, err := m.find(context.Background()); err == nil {
		t.Fatal("found a router")
	}
	m.set(State{Error: "x"})
	m.set(State{Error: "x"})
	if len(logs) != 1 || !strings.Contains(logs[0], "forward them by hand") {
		t.Fatalf("logs %q", logs)
	}
}

func TestLinuxGateway(t *testing.T) {
	p := filepath.Join(t.TempDir(), "route")
	os.WriteFile(p, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"+
		"eth0\t0002A8C0\tFE01A8C0\t0003\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"+ // to another network
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"), 0o644)
	if gw, ok := linuxGateway(p); !ok || gw != netip.MustParseAddr("192.168.1.1") {
		t.Fatalf("%v %v", gw, ok)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
