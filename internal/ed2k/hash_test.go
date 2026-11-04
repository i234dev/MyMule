package ed2k

import (
	"bytes"
	"testing"

	"mymule/internal/md4"
)

func TestHashSmall(t *testing.T) {
	data := []byte("hello mule")
	res, err := HashReader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := Hash(md4.Sum(data)); res.Root != want {
		t.Fatalf("root = %s, want %s", res.Root, want)
	}
	if len(res.Parts) != 1 || res.Size != int64(len(data)) {
		t.Fatalf("parts=%d size=%d", len(res.Parts), res.Size)
	}
}

func TestHashEmpty(t *testing.T) {
	res, err := HashReader(bytes.NewReader(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Root.String() != "31D6CFE0D16AE931B73C59D7E0C089C0" {
		t.Fatalf("empty root = %s", res.Root)
	}
}

func TestHashExactPartBoundary(t *testing.T) {
	data := bytes.Repeat([]byte{0x42}, PartSize)
	res, err := HashReader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	p1 := md4.Sum(data)
	empty := md4.Sum(nil)
	want := Hash(md4.Sum(append(p1[:], empty[:]...)))
	if len(res.Parts) != 2 || res.Root != want {
		t.Fatalf("parts=%d root=%s want %s", len(res.Parts), res.Root, want)
	}
}

func TestHashMultiPart(t *testing.T) {
	data := make([]byte, PartSize+1234)
	for i := range data {
		data[i] = byte(i * 7)
	}
	res, err := HashReader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	p1 := md4.Sum(data[:PartSize])
	p2 := md4.Sum(data[PartSize:])
	want := Hash(md4.Sum(append(p1[:], p2[:]...)))
	if res.Root != want || len(res.Parts) != 2 {
		t.Fatalf("root=%s want %s", res.Root, want)
	}
}

func TestLinkRoundTrip(t *testing.T) {
	in := "ed2k://|file|Some%20File.iso|734003200|0123456789ABCDEF0123456789ABCDEF|/"
	l, err := ParseLink(in)
	if err != nil {
		t.Fatal(err)
	}
	if l.Name != "Some File.iso" || l.Size != 734003200 {
		t.Fatalf("parsed %+v", l)
	}
	if l.String() != in {
		t.Fatalf("round trip: %s", l.String())
	}
}
