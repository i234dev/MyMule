package kad

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"mymule/internal/md4"
	"mymule/internal/netutil"
	"mymule/internal/proto"
)

// Source is a client that has a file, found on Kad.
type Source struct {
	File     ID       // the file's hash
	UserHash [16]byte // the client's user hash
	// Type: 1 or 4 = reachable at IP:TCPPort; 3 or 5 = firewalled,
	// reachable through its buddy; 6 = firewalled, reachable by a UDP
	// callback to IP:UDPPort.
	Type      byte
	IP        netip.Addr
	TCPPort   uint16
	UDPPort   uint16
	Buddy     ID
	BuddyIP   netip.Addr
	BuddyPort uint16
	Crypt     byte // connect options
}

// Direct reports whether the source accepts connections at IP:TCPPort.
func (s Source) Direct() bool {
	return (s.Type == 1 || s.Type == 4) && s.TCPPort != 0 && GoodAddr(s.IP, s.TCPPort)
}

// KeywordResult is a file found by a keyword search.
type KeywordResult struct {
	Hash        [16]byte
	Name        string
	Size        uint64
	Type        string // eD2K file type ("Video"...)
	Format      string
	Artist      string
	Album       string
	Title       string
	Codec       string
	Length      uint32 // seconds
	Bitrate     uint32
	Sources     uint32 // how many clients published it
	PublishInfo uint32
}

// Query is a keyword search.
type Query struct {
	Keywords  string
	Type      string // eD2K file type ("Video"...), "" = any
	MinSize   uint64
	MaxSize   uint64
	Extension string
}

// invalidKeywordChars separate words (eMule's).
const invalidKeywordChars = " ()[]{}<>,._-!?:;\\/\""

// Words splits s into the words Kad indexes: lowercase, at least 3 bytes
// long. With dedupe, each word once.
func Words(s string, dedupe bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(invalidKeywordChars, r) }) {
		if len(w) < 3 {
			continue
		}
		w = strings.ToLower(w)
		if dedupe && seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// plain lowercases s and keeps only its letters and digits.
func plain(s string) string {
	return strings.Map(func(c rune) rune {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return -1
		}
		return unicode.ToLower(c)
	}, s)
}

// KeywordID is the Kad ID of a keyword: the MD4 of its lowercase UTF-8.
func KeywordID(word string) ID { return ID(md4.Sum([]byte(strings.ToLower(word)))) }

// Search expression opcodes (as in eD2K server searches).
const (
	seBool      = 0x00
	seString    = 0x01
	seMetaStr   = 0x02
	seNumeric   = 0x03
	seNumeric64 = 0x08
	boolAnd     = 0x00
	cmpGreaterE = 0x03
	cmpLessE    = 0x04
)

// maxQueryBytes bounds the keywords of a search.
const maxQueryBytes = 512

// tokens splits s like Words, keeping short tokens too (they still filter
// the results), lowercase, each once.
func tokens(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(invalidKeywordChars, r) }) {
		w = strings.ToLower(w)
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// keywordCandidates returns the words a search can ask the nodes of, best
// first: the longest, as eMule does (longer words are rarer, which spreads
// the load).
func keywordCandidates(q Query) ([]string, error) {
	if len(q.Keywords) > maxQueryBytes {
		return nil, errors.New("kad: search too long")
	}
	words := Words(q.Keywords, true)
	if len(words) == 0 {
		return nil, errors.New("kad: no word of 3 letters or more to search")
	}
	sort.SliceStable(words, func(i, j int) bool {
		return utf8.RuneCountInString(words[i]) > utf8.RuneCountInString(words[j])
	})
	return words, nil
}

// keywordSearch builds the search tree the nodes of keyword filter their
// entries with: the other words, and the type, extension and size limits.
// It also returns every word, which result names must have.
func keywordSearch(q Query, keyword string) (words []string, terms []byte) {
	words = tokens(q.Keywords)
	var rest []string
	for _, w := range words {
		if w != keyword {
			rest = append(rest, w)
		}
	}
	var parts []func(*proto.Writer)
	if len(rest) > 0 {
		str := strings.Join(rest, " ")
		parts = append(parts, func(w *proto.Writer) { w.U8(seString); w.Str16(str) })
	}
	meta := func(id byte, v string) func(*proto.Writer) {
		return func(w *proto.Writer) { w.U8(seMetaStr); w.Str16(v); w.U16(1); w.U8(id) }
	}
	typ := q.Type
	if typ == "Arc" || typ == "Iso" {
		typ = "Pro" // what eMule publishes archives and CD images as
	}
	if typ != "" {
		parts = append(parts, meta(TagFileType, typ))
	}
	if ext := strings.TrimPrefix(q.Extension, "."); ext != "" {
		parts = append(parts, meta(TagFileFormat, ext))
	}
	size := func(v uint64, cmp byte) func(*proto.Writer) {
		return func(w *proto.Writer) {
			if v > 0xFFFFFFFF {
				w.U8(seNumeric64)
				w.U64(v)
			} else {
				w.U8(seNumeric)
				w.U32(uint32(v))
			}
			w.U8(cmp)
			w.U16(1)
			w.U8(TagFileSize)
		}
	}
	if q.MinSize > 0 {
		parts = append(parts, size(q.MinSize, cmpGreaterE))
	}
	if q.MaxSize > 0 {
		parts = append(parts, size(q.MaxSize, cmpLessE))
	}
	var w proto.Writer
	for i, p := range parts {
		if i < len(parts)-1 {
			w.U8(seBool)
			w.U8(boolAnd)
		}
		p(&w)
	}
	return words, w.B
}

// ErrNotConnected is returned when Kad has no contacts to ask.
var ErrNotConnected = errors.New("kad: not connected")

// ErrBusy is returned when a search for the same target already runs.
var ErrBusy = errors.New("kad: already searching for that")

// FindSources looks for sources of the file with hash and size; found is
// called for each one (not with the node locked). It returns at once; the
// search runs for about 45 seconds.
func (n *Node) FindSources(hash [16]byte, size uint64, found func(Source)) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.table.Len() == 0 {
		return ErrNotConnected
	}
	if _, ok := n.searches[ID(hash)]; ok {
		return ErrBusy
	}
	n.startSearch(searchFile, ID(hash), n.now(), func(s *search) {
		s.fileSize = size
		s.onSource = found
	})
	return nil
}

// SearchKeyword searches Kad for files; found is called for each result
// (not with the node locked, and never after SearchKeyword returned). It
// returns when the search ends (about 45 seconds) or ctx is done. When
// another search uses the best keyword, the next best is used.
func (n *Node) SearchKeyword(ctx context.Context, q Query, found func(KeywordResult)) error {
	candidates, err := keywordCandidates(q)
	if err != nil {
		return err
	}
	var mu sync.Mutex // held while found runs; stopped under it
	stopped := false
	n.mu.Lock()
	if n.table.Len() == 0 {
		n.mu.Unlock()
		return ErrNotConnected
	}
	var s *search
	for _, kw := range candidates {
		target := KeywordID(kw)
		if _, busy := n.searches[target]; busy {
			continue
		}
		words, terms := keywordSearch(q, kw)
		s = n.startSearch(searchKeyword, target, n.now(), func(s *search) {
			s.terms = terms
			s.words = words
			s.onResult = func(r KeywordResult) {
				mu.Lock()
				defer mu.Unlock()
				if !stopped {
					found(r)
				}
			}
		})
		break
	}
	n.mu.Unlock()
	if s == nil {
		return ErrBusy
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	n.mu.Lock()
	if n.searches[s.target] == s {
		delete(n.searches, s.target)
		n.searchEnded(s)
	}
	n.mu.Unlock()
	mu.Lock()
	stopped = true
	mu.Unlock()
	return nil
}

// Note is what a user said about a file: its rating (eMule's: 1 fake or
// corrupt, 2 poor, 3 fair, 4 good, 5 excellent; 0 none) and a comment.
type Note struct {
	UserHash [16]byte
	Name     string // the file name that user has
	Rating   int
	Comment  string
}

// SearchNotes asks Kad for the notes on a file; found is called for each
// one (not with the node locked, and never after SearchNotes returned). It
// returns when the search ends (about 45 seconds) or ctx is done.
func (n *Node) SearchNotes(ctx context.Context, hash [16]byte, size uint64, found func(Note)) error {
	var mu sync.Mutex
	stopped := false
	n.mu.Lock()
	if n.table.Len() == 0 {
		n.mu.Unlock()
		return ErrNotConnected
	}
	s := n.startSearch(searchNotes, ID(hash), n.now(), func(s *search) {
		s.fileSize = size
		s.onNote = func(nt Note) {
			mu.Lock()
			defer mu.Unlock()
			if !stopped {
				found(nt)
			}
		}
	})
	n.mu.Unlock()
	if s == nil {
		return ErrBusy
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	n.mu.Lock()
	if n.searches[s.target] == s {
		delete(n.searches, s.target)
		n.searchEnded(s)
	}
	n.mu.Unlock()
	mu.Lock()
	stopped = true
	mu.Unlock()
	return nil
}

func (n *Node) noteResult(s *search, answer ID, tags []proto.Tag) {
	nt := Note{UserHash: [16]byte(answer)}
	for _, t := range tags {
		if t.Name != "" {
			continue
		}
		switch t.ID {
		case TagFileName:
			nt.Name = netutil.CleanText(t.Str, 1024)
		case TagFileRating:
			if t.Int <= 5 {
				nt.Rating = int(t.Int)
			}
		case TagDescription:
			nt.Comment = netutil.CleanText(t.Str, 512)
		}
	}
	if nt.Rating == 0 && nt.Comment == "" {
		return
	}
	if s.seen[answer] { // one note per user
		return
	}
	s.seen[answer] = true
	s.answers++
	if s.onNote != nil {
		f := s.onNote
		n.later(func() { f(nt) })
	}
}

// storePacket asks a node close enough to the target for the value.
func (n *Node) storePacket(s *search, dist ID, c Contact, now time.Time) {
	if s.stopping || dist.Chunk(0) > searchTolerance || c.Version < 3 {
		return
	}
	var w proto.Writer
	switch s.typ {
	case searchFile:
		WriteID(&w, s.target)
		w.U16(0) // start position
		w.U64(s.fileSize)
		n.sendTo(c, OpSearchSourceReq, w.B)
	case searchKeyword:
		WriteID(&w, s.target)
		if len(s.terms) == 0 {
			w.U16(0)
		} else {
			w.U16(0x8000) // restrictive: the search tree follows
			w.Bytes(s.terms)
		}
		n.sendTo(c, OpSearchKeyReq, w.B)
	case searchNotes:
		WriteID(&w, s.target)
		w.U64(s.fileSize)
		n.sendTo(c, OpSearchNotesReq, w.B)
	case searchStoreFile:
		n.storeFile(s, c)
	case searchStoreNotes:
		n.storeNotes(s, c)
	case searchStoreKeyword:
		n.storeKeyword(s, c)
	case searchFindBuddy:
		n.storeFindBuddy(s, c)
	case searchFindSource:
		n.storeFindSource(s, c)
	default:
		return
	}
	s.asked[c.Addr()] = c
}

// maxResultsPerPacket bounds the answers one SEARCH_RES may carry.
const maxResultsPerPacket = 300

// onSearchRes handles the answers to a search (KADEMLIA2_SEARCH_RES, or
// the version 1 KADEMLIA_SEARCH_RES without the sender's ID).
func (n *Node) onSearchRes(in incoming, now time.Time) error {
	r := proto.NewReader(in.payload)
	if in.op == OpSearchRes {
		ReadID(r) // sender
	}
	target := ReadID(r)
	count := int(r.U16())
	if r.Err() != nil || count > maxResultsPerPacket || count > r.Len()/(IDLen+1) {
		return errBad
	}
	s := n.searches[target]
	if s == nil {
		return errBad
	}
	// Only from a node we sent the search to, and from nodes that can,
	// with our key: answers cannot be pushed into a search by anyone else,
	// even with a forged source address.
	c, ok := s.asked[in.from]
	if !ok || (c.Version >= 6 && !in.validKey) {
		return errBad
	}
	for range count {
		if s.answers >= maxAnswers {
			return nil // enough: the rest is not even read
		}
		answer := ReadID(r)
		tags, err := ReadTags(r)
		if err != nil {
			return errBad
		}
		switch s.typ {
		case searchFile:
			n.fileResult(s, answer, tags)
		case searchKeyword:
			n.keywordResult(s, answer, tags)
		case searchNotes:
			n.noteResult(s, answer, tags)
		}
	}
	return nil
}

func (n *Node) fileResult(s *search, answer ID, tags []proto.Tag) {
	src := Source{File: s.target, UserHash: answer}
	for _, t := range tags {
		if t.Name != "" {
			continue
		}
		switch t.ID {
		case TagSourceType:
			src.Type = byte(t.Int)
		case TagSourceIP:
			src.IP = ipFromU32(uint32(t.Int))
		case TagSourcePort:
			src.TCPPort = uint16(t.Int)
		case TagSourceUDPPort:
			src.UDPPort = uint16(t.Int)
		case TagServerIP:
			// Network order, unlike the source's own IP (as eMule).
			var a [4]byte
			binary.LittleEndian.PutUint32(a[:], uint32(t.Int))
			src.BuddyIP = netip.AddrFrom4(a)
		case TagServerPort:
			src.BuddyPort = uint16(t.Int)
		case TagBuddyHash:
			if h, ok := ParseID(t.Str); ok {
				src.Buddy = h
			}
		case TagEncryption:
			src.Crypt = byte(t.Int)
		}
	}
	switch src.Type {
	case 1, 3, 4, 5, 6:
	default:
		return
	}
	if src.IP == n.publicIP() && src.TCPPort == n.cfg.TCPPort() {
		return // ourselves
	}
	key := ID(src.UserHash)
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.answers++
	if s.onSource != nil {
		f := s.onSource
		n.later(func() { f(src) })
	}
}

func (n *Node) keywordResult(s *search, answer ID, tags []proto.Tag) {
	res := KeywordResult{Hash: answer}
	hasName, hasSize := false, false
	for _, t := range tags {
		if t.Name != "" {
			continue
		}
		switch t.ID {
		case TagFileName:
			res.Name = netutil.CleanText(t.Str, 1024)
			hasName = res.Name != ""
		case TagFileSize:
			if t.Type == proto.TagBsob && len(t.Bytes) == 8 {
				res.Size = proto.NewReader(t.Bytes).U64()
			} else {
				res.Size = t.Int
			}
			hasSize = true
		case TagFileType:
			res.Type = netutil.CleanText(t.Str, 32)
		case TagFileFormat:
			res.Format = netutil.CleanText(t.Str, 32)
		case TagMediaArtist:
			res.Artist = netutil.CleanText(t.Str, 256)
		case TagMediaAlbum:
			res.Album = netutil.CleanText(t.Str, 256)
		case TagMediaTitle:
			res.Title = netutil.CleanText(t.Str, 256)
		case TagMediaLength:
			res.Length = uint32(t.Int)
		case TagMediaBitrate:
			res.Bitrate = uint32(t.Int)
		case TagMediaCodec:
			res.Codec = netutil.CleanText(t.Str, 64)
		case TagSources:
			if t.Int <= 65500 {
				res.Sources = uint32(t.Int)
			}
		case TagPublishInfo:
			res.PublishInfo = uint32(t.Int)
		}
	}
	if !hasName || !hasSize || res.Size == 0 {
		return
	}
	// Every word of the search must be in the name (ignoring case and
	// punctuation): a node may answer with anything it has under the
	// keyword.
	name := plain(res.Name)
	for _, w := range s.words {
		if !strings.Contains(name, plain(w)) {
			return
		}
	}
	if s.seen[answer] {
		return
	}
	s.seen[answer] = true
	s.answers++
	if s.onResult != nil {
		f := s.onResult
		n.later(func() { f(res) })
	}
}
