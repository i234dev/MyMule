// Package credit implements eMule's credit system: peers that uploaded to
// us move up in our upload queue, and we prove who we are (secure
// identification) so that the peers we upload to do the same for us.
//
// Credits are kept per user hash, but a user hash is only what a peer
// says. So a peer earns credits, and gets them, only once it signed a
// challenge of ours with the RSA key first seen with that hash: nobody
// can take another user's credits, or spend them, by sending its hash.
package credit

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/fsutil"
)

const (
	// expiry: credits of a peer not seen for this long are forgotten
	// (eMule: 150 days).
	expiry = 150 * 24 * time.Hour
	// maxRecords bounds the credits kept; when full, the peer seen
	// longest ago makes room.
	maxRecords = 50000
	saveEvery  = 13 * time.Minute

	fileMagic = "MMCR"
	fileVer   = 1
)

type record struct {
	up, down uint64 // bytes we sent to it, bytes it sent us
	seen     int64  // unix time
	key      []byte
	identIP  string // where it proved its identity this run; "" = not
}

// Store holds our key and the credits of the peers we know.
type Store struct {
	key *privateKey
	pub []byte
	log func(string, ...any)
	// Debug, if set, gets per-peer events (set it before use).
	Debug func(string, ...any)

	mu    sync.Mutex
	path  string
	recs  map[ed2k.Hash]*record
	dirty bool
	now   func() time.Time
}

// Open loads (or creates) our key from dir/cryptkey.dat, in eMule's
// format, and the credits from dir/credits.dat.
func Open(dir string, log func(string, ...any)) (*Store, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	key, err := loadKey(dir + string(os.PathSeparator) + "cryptkey.dat")
	if err != nil {
		return nil, err
	}
	s := &Store{key: key, pub: key.publicKey.marshal(), log: log, Debug: func(string, ...any) {}, path: dir + string(os.PathSeparator) + "credits.dat",
		recs: map[ed2k.Hash]*record{}, now: time.Now}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log("credits: %v", err)
	default:
		if err := s.decode(b); err != nil {
			// Starting again from nothing costs little: credits come back
			// as peers upload to us.
			log("credits: %s: %v; starting with none", s.path, err)
			s.recs = map[ed2k.Hash]*record{}
		}
	}
	return s, nil
}

func loadKey(path string) (*privateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b)), ""))
		if err == nil {
			if k, err := parsePrivateKey(der); err == nil {
				return k, nil
			}
		}
		return nil, fmt.Errorf("%s is damaged: delete it and a new key is made (peers then give the credits earned with the old one to nobody)", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := generateKey()
	if err != nil {
		return nil, err
	}
	der, err := k.marshal()
	if err != nil {
		return nil, err
	}
	if err := fsutil.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(der)), 0o600); err != nil {
		return nil, fmt.Errorf("saving the key: %w", err)
	}
	return k, nil
}

// PublicKey is our public key, as sent to peers.
func (s *Store) PublicKey() []byte { return s.pub }

// KeyOf returns the key a peer proved its identity with before (nil if
// none).
func (s *Store) KeyOf(h ed2k.Hash) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.recs[h]; r != nil {
		return r.key
	}
	return nil
}

// Identified reports whether the peer h proved its identity from ip in
// this run.
func (s *Store) Identified(h ed2k.Hash, ip net.IP) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[h]
	return r != nil && r.identIP != "" && r.identIP == ip.String()
}

// verified records that h, from ip, signed our challenge with key.
func (s *Store) verified(h ed2k.Hash, ip net.IP, key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[h]
	if r == nil {
		if len(s.recs) >= maxRecords {
			s.evictLocked()
		}
		r = &record{key: append([]byte(nil), key...)}
		s.recs[h] = r
	}
	r.identIP = ip.String()
	r.seen = s.now().Unix()
	s.dirty = true
}

func (s *Store) evictLocked() {
	var old ed2k.Hash
	var oldest int64 = math.MaxInt64
	for h, r := range s.recs {
		if r.seen < oldest {
			old, oldest = h, r.seen
		}
	}
	delete(s.recs, old)
}

// identifiedLocked returns h's record if it proved its identity from ip.
func (s *Store) identifiedLocked(h ed2k.Hash, ip net.IP) *record {
	r := s.recs[h]
	if r == nil || r.identIP == "" || r.identIP != ip.String() {
		return nil
	}
	return r
}

// Uploaded counts n bytes we sent to h. Only an identified peer's count
// moves: otherwise anyone could use up another user's credits.
func (s *Store) Uploaded(h ed2k.Hash, ip net.IP, n int64) {
	s.add(h, ip, n, true)
}

// Downloaded counts n bytes h sent us.
func (s *Store) Downloaded(h ed2k.Hash, ip net.IP, n int64) {
	s.add(h, ip, n, false)
}

func (s *Store) add(h ed2k.Hash, ip net.IP, n int64, up bool) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.identifiedLocked(h, ip)
	if r == nil {
		return
	}
	if up {
		r.up += uint64(n)
	} else {
		r.down += uint64(n)
	}
	r.seen = s.now().Unix()
	s.dirty = true
}

// Ratio is the factor by which h's waiting time counts in our upload
// queue, from 1 to 10, as in eMule: it grows with what h uploaded to us
// and shrinks with what we uploaded to it. A peer that did not prove its
// identity from ip gets 1.
func (s *Store) Ratio(h ed2k.Hash, ip net.IP) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.identifiedLocked(h, ip)
	if r == nil {
		return 1
	}
	return ratio(r.up, r.down)
}

func ratio(up, down uint64) float64 {
	if down < 1<<20 {
		return 1
	}
	res := 10.0
	if up > 0 {
		res = float64(down) * 2 / float64(up)
	}
	res = min(res, math.Sqrt(float64(down)/(1<<20)+2))
	return min(max(res, 1), 10)
}

// Credit is what we know of one peer, for the status pages.
type Credit struct {
	Uploaded, Downloaded uint64
	Ratio                float64
	Identified           bool
}

// Get returns h's credits.
func (s *Store) Get(h ed2k.Hash, ip net.IP) (Credit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[h]
	if r == nil {
		return Credit{Ratio: 1}, false
	}
	id := s.identifiedLocked(h, ip) != nil
	c := Credit{Uploaded: r.up, Downloaded: r.down, Ratio: 1, Identified: id}
	if id {
		c.Ratio = ratio(r.up, r.down)
	}
	return c, true
}

// Len is the number of peers we keep credits for.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// Run saves the credits now and then until ctx ends, and once more then.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(saveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Save()
			return
		case <-t.C:
			s.Save()
		}
	}
}

// Save writes the credits if they changed.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b := s.encodeLocked()
	s.dirty = false
	s.mu.Unlock()
	if err := fsutil.WriteFile(s.path, b, 0o600); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		s.log("credits: %v", err)
		return err
	}
	return nil
}

// File: "MMCR", version, count, then per peer: hash, uploaded,
// downloaded, last seen, key length, key. Little-endian.
func (s *Store) encodeLocked() []byte {
	b := append([]byte(fileMagic), fileVer)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s.recs)))
	for h, r := range s.recs {
		b = append(b, h[:]...)
		b = binary.LittleEndian.AppendUint64(b, r.up)
		b = binary.LittleEndian.AppendUint64(b, r.down)
		b = binary.LittleEndian.AppendUint64(b, uint64(r.seen))
		b = append(b, byte(len(r.key)))
		b = append(b, r.key...)
	}
	return b
}

func (s *Store) decode(b []byte) error {
	bad := errors.New("damaged file")
	if len(b) < 9 || string(b[:4]) != fileMagic {
		return bad
	}
	if b[4] != fileVer {
		return fmt.Errorf("unknown version %d", b[4])
	}
	n := binary.LittleEndian.Uint32(b[5:])
	b = b[9:]
	old := s.now().Add(-expiry).Unix()
	for range n {
		if len(b) < 16+24+1 {
			return bad
		}
		var h ed2k.Hash
		copy(h[:], b)
		r := &record{
			up:   binary.LittleEndian.Uint64(b[16:]),
			down: binary.LittleEndian.Uint64(b[24:]),
			seen: int64(binary.LittleEndian.Uint64(b[32:])),
		}
		kl := int(b[40])
		b = b[41:]
		if kl > MaxKeySize || len(b) < kl {
			return bad
		}
		r.key = append([]byte(nil), b[:kl]...)
		b = b[kl:]
		if r.seen < old || kl == 0 || len(s.recs) >= maxRecords {
			s.dirty = true
			continue
		}
		s.recs[h] = r
	}
	if len(b) != 0 {
		return bad
	}
	return nil
}
