package obfus

import (
	"bytes"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"

	"mymule/internal/ed2k"
)

func pair(t *testing.T, hashB, hashDial ed2k.Hash, plainOK, obfusOK bool) (a, b *Conn, errA, errB error) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		nc, err := ln.Accept()
		if err != nil {
			errB = err
			return
		}
		b, errB = Accept(nc, hashB, plainOK, obfusOK)
		if errB != nil {
			nc.Close()
		}
	}()
	nc, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	a, errA = Client(nc, hashDial)
	<-done
	if b != nil {
		t.Cleanup(func() { b.Close() })
	}
	return
}

func TestObfuscatedBothWays(t *testing.T) {
	h := ed2k.Hash{1, 2, 3}
	a, b, errA, errB := pair(t, h, h, true, true)
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if !a.Obfuscated() || !b.Obfuscated() {
		t.Fatal("not obfuscated")
	}
	msg := bytes.Repeat([]byte{0xE3, 1, 2, 3}, 5000)
	go a.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(b, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatal("A to B", err)
	}
	go b.Write(msg[:100])
	if _, err := io.ReadFull(a, got[:100]); err != nil || !bytes.Equal(got[:100], msg[:100]) {
		t.Fatal("B to A", err)
	}
}

// Dialing with the wrong user hash: the receiver does not find the magic
// value and drops the connection.
func TestWrongHash(t *testing.T) {
	_, _, errA, errB := pair(t, ed2k.Hash{1}, ed2k.Hash{2}, true, true)
	if errA == nil || errB == nil {
		t.Fatal("handshake with the wrong hash succeeded", errA, errB)
	}
}

func TestObfuscationRefused(t *testing.T) {
	_, _, errA, errB := pair(t, ed2k.Hash{1}, ed2k.Hash{1}, true, false)
	if errA == nil || errB == nil {
		t.Fatal("obfuscated connection accepted with obfuscation off")
	}
}

func TestPlain(t *testing.T) {
	for _, plainOK := range []bool{true, false} {
		ln, _ := net.Listen("tcp4", "127.0.0.1:0")
		res := make(chan error, 1)
		var got []byte
		go func() {
			nc, _ := ln.Accept()
			defer nc.Close()
			c, err := Accept(nc, ed2k.Hash{1}, plainOK, true)
			if err == nil {
				got = make([]byte, 6)
				_, err = io.ReadFull(c, got)
				if c.Obfuscated() {
					err = errors.New("plain seen as obfuscated")
				}
			}
			res <- err
		}()
		nc, _ := net.Dial("tcp4", ln.Addr().String())
		nc.Write([]byte{0xE3, 1, 0, 0, 0, 1})
		err := <-res
		nc.Close()
		ln.Close()
		if plainOK && (err != nil || !bytes.Equal(got, []byte{0xE3, 1, 0, 0, 0, 1})) {
			t.Fatal(err, got)
		}
		if !plainOK && !errors.Is(err, ErrNotObfuscated) {
			t.Fatal("plain accepted when obfuscation is required", err)
		}
	}
}

// The handshake bytes, decrypted by hand as the spec describes them.
func TestWireFormat(t *testing.T) {
	h := ed2k.Hash{9, 8, 7}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go Client(c1, h)
	head := make([]byte, 5+7)
	if _, err := io.ReadFull(c2, head); err != nil {
		t.Fatal(err)
	}
	if head[0] == 0xE3 || head[0] == 0xC5 || head[0] == 0xD4 {
		t.Fatal("marker is a protocol byte")
	}
	key := md5.Sum(append(append(append([]byte{}, h[:]...), 34), head[1:5]...))
	ciph, _ := rc4.NewCipher(key[:])
	drop := make([]byte, 1024)
	ciph.XORKeyStream(drop, drop)
	body := head[5:]
	ciph.XORKeyStream(body, body)
	if binary.LittleEndian.Uint32(body) != 0x835E6FC4 || body[4] != 0 || body[5] != 0 || body[6] >= maxPadding {
		t.Fatalf("% x", body)
	}
}

// A server written from the description: the handshake completes and
// data goes both ways.
func TestServerHandshake(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	done := make(chan error, 1)
	go func() {
		head := make([]byte, 1+primeSize+1)
		if _, err := io.ReadFull(c2, head); err != nil {
			done <- err
			return
		}
		io.CopyN(io.Discard, c2, int64(head[primeSize+1]))
		ga := new(big.Int).SetBytes(head[1 : 1+primeSize])
		b := big.NewInt(0x1234567890)
		gb := new(big.Int).Exp(big.NewInt(2), b, dhPrime)
		// Keys as the description gives them, written out here so the
		// test does not share the code it checks: MD5 of the secret in
		// 96 bytes and the magic value; RC4 without its first 1024 bytes.
		secret := new(big.Int).Exp(ga, b, dhPrime).FillBytes(make([]byte, primeSize))
		key := func(magic byte) *rc4.Cipher {
			k := md5.Sum(append(append([]byte{}, secret...), magic))
			c, _ := rc4.NewCipher(k[:])
			drop := make([]byte, 1024)
			c.XORKeyStream(drop, drop)
			return c
		}
		toClient, fromClient := key(203), key(34)
		msg := binary.LittleEndian.AppendUint32(nil, 0x835E6FC4)
		msg = append(msg, 0, 0, 3, 9, 9, 9)
		toClient.XORKeyStream(msg, msg)
		c2.Write(append(gb.FillBytes(make([]byte, primeSize)), msg...))
		fin := make([]byte, 6)
		io.ReadFull(c2, fin)
		fromClient.XORKeyStream(fin, fin)
		if binary.LittleEndian.Uint32(fin) != 0x835E6FC4 || fin[4] != 0 {
			done <- errors.New("bad answer from the client")
			return
		}
		io.CopyN(io.Discard, readerFunc(func(p []byte) (int, error) {
			n, err := c2.Read(p)
			fromClient.XORKeyStream(p[:n], p[:n])
			return n, err
		}), int64(fin[5]))
		login := make([]byte, 3)
		io.ReadFull(c2, login)
		fromClient.XORKeyStream(login, login)
		if !bytes.Equal(login, []byte{0xE3, 1, 2}) {
			done <- errors.New("data after the handshake not decrypted")
			return
		}
		done <- nil
	}()
	c, err := Server(c1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{0xE3, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
