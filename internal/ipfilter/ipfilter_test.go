package ipfilter

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestParseAndBlock(t *testing.T) {
	list := `# a comment
// another
001.002.003.000 - 001.002.003.255 , 000 , Example range one
5.6.7.8 - 5.6.7.8 , 100 , one address
9.0.0.0 - 9.255.255.255 , 127 , allowed level
Example P2P range:10.20.30.0-10.20.30.127
Name: with colons:11.0.0.0-11.0.0.10
not a range
1.2.3.4 - 1.2.3.0 , 0 , backwards
300.1.1.1 - 300.1.1.2 , 0 , bad
1.2.4.0 - 1.2.4.255 , 50 , touches the first
`
	f, bad, err := Parse(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	if bad != 3 {
		t.Fatalf("%d unreadable lines", bad)
	}
	for ip, want := range map[string]bool{
		"1.2.3.0": true, "1.2.3.255": true, "1.2.4.100": true, "1.2.5.0": false, "1.2.2.255": false,
		"5.6.7.8": true, "5.6.7.9": false, "9.1.1.1": false,
		"10.20.30.127": true, "10.20.30.128": false, "11.0.0.5": true,
		"::ffff:1.2.3.4": true, "2001:db8::1": false,
	} {
		if got := f.Blocked(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: blocked %v", ip, got)
		}
	}
	// The two touching ranges are one.
	if f.Len() != 4 {
		t.Fatalf("%d ranges", f.Len())
	}
	if (*Filter)(nil).Blocked(netip.MustParseAddr("1.2.3.4")) {
		t.Fatal("nil filter blocks")
	}
}

// A huge line or a list of too many ranges fails instead of eating memory.
func TestParseBounded(t *testing.T) {
	if _, _, err := Parse(strings.NewReader(strings.Repeat("x", 5000) + "\n")); err == nil {
		t.Fatal("huge line read")
	}
	var b strings.Builder
	for i := range MaxRanges + 1 {
		b.WriteString("r:")
		b.WriteString(itoa(i>>16) + "." + itoa(i>>8&255) + "." + itoa(i&255) + ".1-")
		b.WriteString(itoa(i>>16) + "." + itoa(i>>8&255) + "." + itoa(i&255) + ".1\n")
	}
	if _, _, err := Parse(strings.NewReader(b.String())); err == nil {
		t.Fatal("more than MaxRanges taken")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// A list reads the same plain, gzipped or zipped; a gzip bomb stops at
// MaxFile.
func TestDecodeFormats(t *testing.T) {
	list := "1.2.3.0 - 1.2.3.255 , 0 , example\n"
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(list))
	w.Close()
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	fw, _ := zw.Create("guarding.p2p")
	fw.Write([]byte(list))
	zw.Close()
	for name, b := range map[string][]byte{"plain": []byte(list), "gzip": gz.Bytes(), "zip": zb.Bytes()} {
		f, _, err := Decode(b)
		if err != nil || !f.Blocked(netip.MustParseAddr("1.2.3.4")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var bomb bytes.Buffer
	w = gzip.NewWriter(&bomb)
	line := []byte("# padding padding padding padding padding padding padding\n")
	for written := 0; written <= MaxFile; written += len(line) {
		w.Write(line)
	}
	w.Close()
	if _, _, err := Decode(bomb.Bytes()); err == nil {
		t.Fatal("list past MaxFile read")
	}
}

// The filtered socket drops packets from blocked addresses and does not
// send to them.
func TestPacketConn(t *testing.T) {
	a, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	h := &Holder{}
	fa := PacketConn(a, h)
	buf := make([]byte, 16)

	// Not blocked: both ways.
	b.WriteTo([]byte("one"), a.LocalAddr())
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _, err := fa.ReadFrom(buf); err != nil || string(buf[:n]) != "one" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	// Blocked: what comes in is dropped, what goes out is not sent.
	f, _, _ := Parse(strings.NewReader("127.0.0.1 - 127.0.0.1 , 0 , loopback\n"))
	h.Set(f)
	b.WriteTo([]byte("two"), a.LocalAddr())
	a.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := fa.ReadFrom(buf); err == nil {
		t.Fatalf("read %q from a blocked address", buf[:n])
	}
	if n, err := fa.WriteTo([]byte("three"), b.LocalAddr()); err != nil || n != 5 {
		t.Fatalf("write: %d %v", n, err)
	}
	b.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := b.ReadFrom(buf); err == nil {
		t.Fatalf("%q sent to a blocked address", buf[:n])
	}
}
