package kad

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"mymule/internal/proto"
)

// Publishing our shared files (eMule's CSharedFileList::Publish): the
// words of complete files' names go to the nodes of each word, so that
// keyword searches find them, and every shared file (partial ones too) to
// the nodes of its hash as a source.

// SharedFile is a file we share, as Kad publishes it.
type SharedFile struct {
	Hash     [16]byte
	Name     string
	Size     uint64
	Complete bool
}

// Publishing rhythm and limits (eMule's).
const (
	publishEvery         = 2 * time.Second // one publish started per kind
	keywordRepublish     = 24 * time.Hour
	sourceRepublish      = 5 * time.Hour
	maxStoreKeyword      = 2   // keyword publishes at once
	maxStoreSource       = 3   // source publishes at once
	maxPublishPerKeyword = 150 // files sent per keyword publish
	filesPerPublish      = 50  // files per KADEMLIA2_PUBLISH_KEY_REQ
	storeAnswers         = 10  // answers after which a store stops
	storeLifetime        = 140 * time.Second
	largeFileSize        = 4290048000
	overloadedLoad       = 20 // average load above which a keyword waits
	maxOverloadedWaiting = 7 * 24 * time.Hour
)

// pubKeyword is one word of our shared files' names.
type pubKeyword struct {
	word   string
	id     ID
	files  [][16]byte
	offset int // rotation, when it has more files than one publish takes
	next   time.Time
}

// publisher state, kept by the node.
type publishState struct {
	allowed  bool // our own lookup finished: we know our neighbourhood
	version  int  // of the shared files list the keywords come from
	keywords []*pubKeyword
	byWord   map[string]*pubKeyword
	kwCursor int
	kwNext   time.Time
	files    []SharedFile
	srcNext  map[[16]byte]time.Time
	srcCur   int
	srcTime  time.Time
}

// kadType is the file type eMule publishes and searches for a name:
// archives and CD images count as programs.
func kadType(name string, typeOf func(string) string) string {
	if typeOf == nil {
		return ""
	}
	t := typeOf(name)
	if t == "Arc" || t == "Iso" {
		return "Pro"
	}
	return t
}

// nameWords are the words a file is published under: those of its name
// without the extension (else nodes would index "avi" for every video).
func nameWords(name string) []string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i-1 <= 5 {
		ext := name[i+1:]
		if strings.IndexFunc(ext, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) < 0 {
			name = name[:i]
		}
	}
	return Words(name, true)
}

// refreshShared reloads the shared files when their list changed.
func (n *Node) refreshShared() {
	if n.cfg.Shared == nil {
		return
	}
	version, files := n.cfg.Shared()
	p := &n.pub
	if p.byWord != nil && version == p.version {
		return
	}
	p.version = version
	p.files = files
	shared := map[[16]byte]bool{}
	for _, f := range files {
		shared[f.Hash] = true
	}
	if p.srcNext == nil {
		p.srcNext = map[[16]byte]time.Time{}
	}
	for h := range p.srcNext {
		if !shared[h] {
			delete(p.srcNext, h) // no longer shared
		}
	}
	old := p.byWord
	p.byWord = map[string]*pubKeyword{}
	p.keywords = nil
	for _, f := range files {
		if !f.Complete {
			continue
		}
		for _, w := range nameWords(f.Name) {
			k := p.byWord[w]
			if k == nil {
				k = &pubKeyword{word: w, id: KeywordID(w)}
				if o := old[w]; o != nil {
					k.next, k.offset = o.next, o.offset
				}
				p.byWord[w] = k
				p.keywords = append(p.keywords, k)
			}
			k.files = append(k.files, f.Hash)
		}
	}
	if p.kwCursor >= len(p.keywords) {
		p.kwCursor = 0
	}
}

func (n *Node) storeSearches(typ searchType) int {
	c := 0
	for _, s := range n.searches {
		if s.typ == typ {
			c++
		}
	}
	return c
}

// tickPublish starts the publishes that are due.
func (n *Node) tickPublish(now time.Time) {
	if !n.pub.allowed || !n.connected(now) || n.cfg.Shared == nil || !n.canPublish() {
		return
	}
	n.refreshShared()
	p := &n.pub
	if !now.Before(p.kwNext) && n.storeSearches(searchStoreKeyword) < maxStoreKeyword && len(p.keywords) > 0 {
		p.kwNext = now.Add(publishEvery)
		k := p.keywords[p.kwCursor]
		p.kwCursor = (p.kwCursor + 1) % len(p.keywords)
		if !now.Before(k.next) {
			if until, ok := n.index.load[k.id]; !ok || !now.Before(until) {
				delete(n.index.load, k.id)
				files := k.files
				rotated := len(files) > maxPublishPerKeyword
				if rotated {
					k.offset %= len(files)
					files = append(append([][16]byte(nil), files[k.offset:]...), files[:k.offset]...)[:maxPublishPerKeyword]
				}
				if n.startSearch(searchStoreKeyword, k.id, now, func(s *search) { s.files = files }) != nil {
					k.next = now.Add(keywordRepublish)
					if rotated {
						k.offset += maxPublishPerKeyword // the next files next time
					}
				}
			}
		}
	}
	if !now.Before(p.srcTime) && n.storeSearches(searchStoreFile) < maxStoreSource && len(p.files) > 0 {
		p.srcTime = now.Add(publishEvery)
		if p.srcCur >= len(p.files) {
			p.srcCur = 0
		}
		f := p.files[p.srcCur]
		p.srcCur++
		if !now.Before(p.srcNext[f.Hash]) {
			if n.startSearch(searchStoreFile, ID(f.Hash), now, func(s *search) { s.file = f }) != nil {
				p.srcNext[f.Hash] = now.Add(sourceRepublish)
			}
		}
	}
}

// canPublish reports whether others can reach us to download: our TCP
// port open (or, firewalled, a buddy to relay callbacks).
func (n *Node) canPublish() bool {
	if n.cfg.TCPPort() == 0 {
		return false
	}
	return !n.firewalled() || n.udpReachable() || (n.mine != nil && n.mine.connected)
}

// storeFile sends a node our source entry for s.file.
func (n *Node) storeFile(s *search, c Contact) {
	if c.Version < 4 {
		return // only the version 2 publish is supported
	}
	tags := n.sourceTags(s.file)
	if tags == nil {
		s.prepareToStop(n.now()) // nobody could reach us now
		return
	}
	var w proto.Writer
	WriteID(&w, s.target)
	WriteID(&w, ID(n.keys.UserHash))
	WriteTags(&w, tags)
	n.sendTo(c, OpPublishSourceReq, w.B)
}

// PublishNote publishes what we say about a file (a rating from 1, fake,
// to 5, excellent, and a comment) on the nodes close to it, as eMule
// does; they keep it for a day, so it is published again while it holds.
func (n *Node) PublishNote(hash [16]byte, size uint64, name string, rating int, comment string) error {
	if rating < 0 || rating > 5 {
		return fmt.Errorf("kad: rating %d", rating)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.table.Len() == 0 {
		return ErrNotConnected
	}
	nt := Note{UserHash: n.keys.UserHash, Name: name, Rating: rating, Comment: comment}
	if n.startSearch(searchStoreNotes, ID(hash), n.now(), func(s *search) {
		s.fileSize, s.note = size, nt
	}) == nil {
		return ErrBusy
	}
	return nil
}

// storeNotes sends a node our note on a file (KADEMLIA2_PUBLISH_NOTES_REQ).
func (n *Node) storeNotes(s *search, c Contact) {
	if c.Version < 2 {
		return
	}
	tags := []proto.Tag{StrTag(TagFileName, s.note.Name), IntTag(TagFileSize, s.fileSize)}
	if s.note.Rating > 0 {
		tags = append(tags, IntTag(TagFileRating, uint64(s.note.Rating)))
	}
	if s.note.Comment != "" {
		tags = append(tags, StrTag(TagDescription, s.note.Comment))
	}
	var w proto.Writer
	WriteID(&w, s.target)
	WriteID(&w, ID(n.keys.UserHash))
	WriteTags(&w, tags)
	n.sendTo(c, OpPublishNotesReq, w.B)
}

// storeKeyword sends a node our files with the keyword, 50 per packet.
func (n *Node) storeKeyword(s *search, c Contact) {
	if c.Version < 2 {
		return
	}
	byHash := map[[16]byte]SharedFile{}
	for _, f := range n.pub.files {
		byHash[f.Hash] = f
	}
	files := s.files
	for len(files) > 0 {
		var w proto.Writer
		WriteID(&w, s.target)
		w.U16(0)
		count := 0
		for len(files) > 0 && count < filesPerPublish {
			f, ok := byHash[files[0]]
			files = files[1:]
			if !ok {
				continue // no longer shared
			}
			WriteID(&w, ID(f.Hash))
			tags := []proto.Tag{StrTag(TagFileName, f.Name), IntTag(TagFileSize, f.Size), IntTag(TagSources, 1)}
			if t := kadType(f.Name, n.cfg.FileType); t != "" {
				tags = append(tags, StrTag(TagFileType, t))
			}
			WriteTags(&w, tags)
			count++
		}
		if count == 0 {
			break
		}
		w.B[IDLen] = byte(count)
		w.B[IDLen+1] = byte(count >> 8)
		n.sendTo(c, OpPublishKeyReq, w.B)
	}
}

// onPublishRes handles a node's answer to a publish: how loaded it is.
func (n *Node) onPublishRes(in incoming, now time.Time) error {
	ip := in.from.Addr()
	if !n.wasAsked(ip, OpPublishKeyReq, now, false) && !n.wasAsked(ip, OpPublishSourceReq, now, false) &&
		!n.wasAsked(ip, OpPublishNotesReq, now, false) {
		return errBad
	}
	r := proto.NewReader(in.payload)
	target := ReadID(r)
	if r.Err() != nil {
		return errBad
	}
	hasLoad := r.Len() > 0
	load := r.U8()
	if in.op == OpPublishRes && r.Len() > 0 && r.U8()&1 != 0 {
		if k := in.senderKey.For(n.publicIP()); k != 0 {
			n.send(in.from, OpPublishResAck, nil, ID{}, k)
		}
	}
	s := n.searches[target]
	if s == nil || (s.typ != searchStoreKeyword && s.typ != searchStoreFile && s.typ != searchStoreNotes) {
		return nil
	}
	if s.typ == searchStoreKeyword && hasLoad {
		s.loadSum += int(load)
		s.loadN++
	}
	s.answers++
	return nil
}

// storeEnded notes the load of the nodes of a keyword we published: an
// overloaded keyword waits before it is published again.
func (n *Node) storeEnded(s *search, now time.Time) {
	if s.typ != searchStoreKeyword || s.loadN == 0 {
		return
	}
	if avg := s.loadSum / s.loadN; avg > overloadedLoad {
		n.index.load[s.target] = now.Add(maxOverloadedWaiting * time.Duration(avg) / 100)
	}
}
