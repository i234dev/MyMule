package credit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
)

// The keys of eMule's secure identification are 384-bit RSA keys, and
// its signatures are RSASSA-PKCS1-v1_5 with SHA-1. Go's crypto/rsa
// refuses keys that small, so the few operations needed are done here.
// Such keys are weak by today's standards; they only decide who gets
// upload credits, as in every eMule client.
const keyBits = 384

// MaxKeySize is the largest public key eMule takes (MAXPUBKEYSIZE).
const MaxKeySize = 80

var oidRSA = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}

// sha1Prefix is the DER DigestInfo header of a SHA-1 hash (PKCS #1).
var sha1Prefix = []byte{0x30, 0x21, 0x30, 0x09, 0x06, 0x05, 0x2b, 0x0e, 0x03, 0x02, 0x1a, 0x05, 0x00, 0x04, 0x14}

type publicKey struct {
	N *big.Int
	E int
}

type privateKey struct {
	publicKey
	D, P, Q *big.Int
}

// generateKey makes a new key with exponent 17, as Crypto++ (and so
// eMule) does: the public key then encodes to the usual 76 bytes.
func generateKey() (*privateKey, error) {
	e := big.NewInt(17)
	one := big.NewInt(1)
	for range 1000 {
		p, err := rand.Prime(rand.Reader, keyBits/2)
		if err != nil {
			return nil, err
		}
		q, err := rand.Prime(rand.Reader, keyBits/2)
		if err != nil {
			return nil, err
		}
		if p.Cmp(q) == 0 {
			continue
		}
		n := new(big.Int).Mul(p, q)
		if n.BitLen() != keyBits {
			continue
		}
		phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))
		d := new(big.Int).ModInverse(e, phi)
		if d == nil {
			continue
		}
		return &privateKey{publicKey{n, 17}, d, p, q}, nil
	}
	return nil, errors.New("could not generate a key")
}

// pkcs1Private and pkcs8 are the encodings of a private key: eMule keeps
// its key (cryptkey.dat) as base64 PKCS #8, and so do we.
type pkcs1Private struct {
	Version          int
	N                *big.Int
	E                int
	D, P, Q          *big.Int
	Dp, Dq, Qinv     *big.Int
	AdditionalPrimes []asn1.RawValue `asn1:"optional"`
}

type pkcs8 struct {
	Version    int
	Algo       pkix.AlgorithmIdentifier
	PrivateKey []byte
}

func (k *privateKey) marshal() ([]byte, error) {
	one := big.NewInt(1)
	inner, err := asn1.Marshal(pkcs1Private{
		N: k.N, E: k.E, D: k.D, P: k.P, Q: k.Q,
		Dp:   new(big.Int).Mod(k.D, new(big.Int).Sub(k.P, one)),
		Dq:   new(big.Int).Mod(k.D, new(big.Int).Sub(k.Q, one)),
		Qinv: new(big.Int).ModInverse(k.Q, k.P),
	})
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(pkcs8{Algo: pkix.AlgorithmIdentifier{Algorithm: oidRSA, Parameters: asn1.NullRawValue}, PrivateKey: inner})
}

func parsePrivateKey(der []byte) (*privateKey, error) {
	var p8 pkcs8
	if rest, err := asn1.Unmarshal(der, &p8); err != nil || len(rest) != 0 {
		return nil, errors.New("not a PKCS #8 key")
	}
	if !p8.Algo.Algorithm.Equal(oidRSA) {
		return nil, errors.New("not an RSA key")
	}
	var p1 pkcs1Private
	if rest, err := asn1.Unmarshal(p8.PrivateKey, &p1); err != nil || len(rest) != 0 {
		return nil, errors.New("bad RSA key")
	}
	k := &privateKey{publicKey{p1.N, p1.E}, p1.D, p1.P, p1.Q}
	// Check it is a working key before using it for anything.
	if k.N == nil || k.D == nil || k.P == nil || k.Q == nil || k.E < 3 || k.N.BitLen() < 256 || k.N.BitLen() > 2048 ||
		new(big.Int).Mul(k.P, k.Q).Cmp(k.N) != 0 {
		return nil, errors.New("bad RSA key")
	}
	m := big.NewInt(0x1234567)
	c := new(big.Int).Exp(m, big.NewInt(int64(k.E)), k.N)
	if new(big.Int).Exp(c, k.D, k.N).Cmp(m) != 0 {
		return nil, errors.New("bad RSA key")
	}
	return k, nil
}

type spki struct {
	Algo pkix.AlgorithmIdentifier
	Key  asn1.BitString
}

// marshalPublic encodes a public key as eMule sends it: an X.509
// SubjectPublicKeyInfo.
func (k *publicKey) marshal() []byte {
	inner, _ := asn1.Marshal(*k)
	b, _ := asn1.Marshal(spki{
		Algo: pkix.AlgorithmIdentifier{Algorithm: oidRSA, Parameters: asn1.NullRawValue},
		Key:  asn1.BitString{Bytes: inner, BitLength: 8 * len(inner)},
	})
	return b
}

// parsePublicKey reads a public key a peer sent.
func parsePublicKey(der []byte) (*publicKey, error) {
	if len(der) > MaxKeySize {
		return nil, errors.New("key too long")
	}
	var s spki
	if rest, err := asn1.Unmarshal(der, &s); err != nil || len(rest) != 0 {
		return nil, errors.New("bad public key")
	}
	if !s.Algo.Algorithm.Equal(oidRSA) || s.Key.BitLength%8 != 0 {
		return nil, errors.New("not an RSA key")
	}
	var k publicKey
	if rest, err := asn1.Unmarshal(s.Key.Bytes, &k); err != nil || len(rest) != 0 {
		return nil, errors.New("bad RSA key")
	}
	if k.N == nil || k.N.Sign() <= 0 || k.N.BitLen() < 256 || k.E < 3 || k.E%2 == 0 {
		return nil, errors.New("bad RSA key")
	}
	return &k, nil
}

func (k *publicKey) size() int { return (k.N.BitLen() + 7) / 8 }

// encoded is the PKCS #1 v1.5 block of msg's SHA-1 hash for a key of k
// bytes (nil if it does not fit).
func encoded(msg []byte, k int) []byte {
	h := sha1.Sum(msg)
	tlen := len(sha1Prefix) + len(h)
	if k < tlen+11 {
		return nil
	}
	em := make([]byte, k)
	em[1] = 1
	for i := 2; i < k-tlen-1; i++ {
		em[i] = 0xFF
	}
	copy(em[k-tlen:], sha1Prefix)
	copy(em[k-len(h):], h[:])
	return em
}

func (k *privateKey) sign(msg []byte) []byte {
	em := encoded(msg, k.size())
	if em == nil {
		return nil
	}
	s := new(big.Int).Exp(new(big.Int).SetBytes(em), k.D, k.N)
	return s.FillBytes(make([]byte, k.size()))
}

func (k *publicKey) verify(msg, sig []byte) bool {
	n := k.size()
	if len(sig) != n {
		return false
	}
	s := new(big.Int).SetBytes(sig)
	if s.Cmp(k.N) >= 0 {
		return false
	}
	em := encoded(msg, n)
	if em == nil {
		return false
	}
	m := new(big.Int).Exp(s, big.NewInt(int64(k.E)), k.N)
	return bytes.Equal(m.FillBytes(make([]byte, n)), em)
}
