// Package download implements file downloading from eD2K peers:
// partial file storage, source management and peer sessions.
package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"mymule/internal/ed2k"
	"mymule/internal/fsutil"
	"mymule/internal/md4"
	"mymule/internal/netutil"
	"mymule/internal/peer"
)

// PartFile is a file being downloaded: the data lives in <hash>.part and
// the state in <hash>.part.json next to it.
type PartFile struct {
	mu sync.Mutex

	Hash    ed2k.Hash
	Name    string
	Size    int64
	Added   time.Time
	hashSet []ed2k.Hash // nil until known; not needed for single-part files

	done     RangeSet            // bytes written to disk
	verified []bool              // per part: hash checked OK
	checked  []bool              // per part: verified in this run, not just per the state file
	reserved map[int64]string    // block start -> owner (peer key)
	resAt    map[int64]time.Time // block start -> reserved or last written
	corrupt  int                 // parts that failed verification (stats)

	// Who sent the data of each part being downloaded, to find a peer
	// that sends bad data (see blameLocked).
	writers map[int]map[string]bool
	unknown map[int]bool           // part has bytes from before a restart (writer unknown)
	isolate map[int]bool           // part failed with several writers: next time one writer only
	lastWr  map[int]time.Time      // last write into an isolated part
	strikes map[string]int         // per owner: distinct parts it sent alone that failed
	badPart map[string]map[int]int // per owner: failures per part (2 = not given to it again)
	ownerAt map[string]time.Time   // per owner: last data it sent us
	banned  map[string]bool        // owners that sent too many bad parts

	f        *os.File
	dataPath string
	metaPath string
	dirty    bool
	deleted  bool // Delete was called: never write the state again
}

type partMeta struct {
	Hash     string     `json:"hash"`
	Name     string     `json:"name"`
	Size     int64      `json:"size"`
	Added    time.Time  `json:"added"`
	HashSet  []string   `json:"hashset,omitempty"`
	Done     [][2]int64 `json:"done"`
	Verified []bool     `json:"verified"`
}

// NumParts is the number of 9.28 MB parts (as peers count them).
func NumParts(size int64) int {
	return int((size + ed2k.PartSize - 1) / ed2k.PartSize)
}

func (pf *PartFile) partRange(p int) Range {
	start := int64(p) * ed2k.PartSize
	return Range{start, min(start+ed2k.PartSize, pf.Size)}
}

// freeSpaceMargin is left free on top of a new download's size.
const freeSpaceMargin = 64 << 20

// diskFree is freeSpace, replaced in tests.
var diskFree = freeSpace

func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

// Create starts a new download in dir.
func Create(dir string, link ed2k.Link) (*PartFile, error) {
	return create(dir, link, true)
}

func create(dir string, link ed2k.Link, checkSpace bool) (*PartFile, error) {
	if !ed2k.ValidSize(link.Size) {
		return nil, fmt.Errorf("invalid file size %d", link.Size)
	}
	// Better to say so now than to fail with a full disk halfway through
	// (and fill the disk for everything else on the machine).
	if free, ok := diskFree(dir); checkSpace && ok && free < uint64(link.Size)+freeSpaceMargin {
		return nil, fmt.Errorf("not enough free space in %s: the file needs %s and %s are free",
			dir, humanBytes(uint64(link.Size)), humanBytes(free))
	}
	base := filepath.Join(dir, link.Hash.String())
	if _, err := os.Stat(base + ".part.json"); err == nil {
		return nil, fmt.Errorf("already downloading %s", link.Hash)
	}
	pf := &PartFile{
		Hash:     link.Hash,
		Name:     netutil.CleanText(link.Name, 1024),
		Size:     link.Size,
		Added:    time.Now(),
		verified: make([]bool, NumParts(link.Size)),
		checked:  make([]bool, NumParts(link.Size)),
		reserved: map[int64]string{},
		resAt:    map[int64]time.Time{},
		writers:  map[int]map[string]bool{},
		unknown:  map[int]bool{},
		isolate:  map[int]bool{},
		lastWr:   map[int]time.Time{},
		strikes:  map[string]int{},
		badPart:  map[string]map[int]int{},
		ownerAt:  map[string]time.Time{},
		banned:   map[string]bool{},
		dataPath: base + ".part",
		metaPath: base + ".part.json",
	}
	f, err := openRegular(pf.dataPath, os.O_RDWR, true)
	if err != nil {
		return nil, err
	}
	pf.f = f
	if err := pf.saveLocked(); err != nil {
		f.Close()
		return nil, err
	}
	return pf, nil
}

// Open resumes a download from its .part.json state file.
//
// The temporary folder is trusted no further than necessary: links are
// refused (see openRegular), the hash set must match the file hash, and
// parts the state file calls verified are hashed again before any of
// their data is uploaded (and all of them before the file is finished).
func Open(metaPath string) (*PartFile, error) {
	data, err := readRegular(metaPath)
	if err != nil {
		return nil, err
	}
	var m partMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", metaPath, err)
	}
	h, err := ed2k.ParseHash(m.Hash)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", metaPath, err)
	}
	if !ed2k.ValidSize(m.Size) {
		return nil, fmt.Errorf("%s: invalid file size %d", metaPath, m.Size)
	}
	pf := &PartFile{
		Hash:     h,
		Name:     netutil.CleanText(m.Name, 1024),
		Size:     m.Size,
		Added:    m.Added,
		verified: make([]bool, NumParts(m.Size)),
		checked:  make([]bool, NumParts(m.Size)),
		reserved: map[int64]string{},
		resAt:    map[int64]time.Time{},
		writers:  map[int]map[string]bool{},
		unknown:  map[int]bool{},
		isolate:  map[int]bool{},
		lastWr:   map[int]time.Time{},
		strikes:  map[string]int{},
		badPart:  map[string]map[int]int{},
		ownerAt:  map[string]time.Time{},
		banned:   map[string]bool{},
		metaPath: metaPath,
		dataPath: strings.TrimSuffix(metaPath, ".json"),
	}
	for _, s := range m.HashSet {
		ph, err := ed2k.ParseHash(s)
		if err != nil {
			return nil, err
		}
		pf.hashSet = append(pf.hashSet, ph)
	}
	if pf.hashSet != nil && !validHashSet(pf.Hash, pf.Size, pf.hashSet) {
		return nil, fmt.Errorf("%s: the hash set does not match the file", metaPath)
	}
	for _, r := range m.Done {
		pf.done.Add(Range{r[0], min(r[1], pf.Size)})
	}
	copy(pf.verified, m.Verified)
	f, err := openRegular(pf.dataPath, os.O_RDWR, true)
	if err != nil {
		return nil, err
	}
	pf.f = f
	// If the data file is shorter than what the state claims (e.g. crash
	// before flush), forget the bytes we don't actually have.
	if st, err := f.Stat(); err == nil && st.Size() < pf.Size {
		pf.done.Remove(Range{st.Size(), pf.Size})
		for p := range pf.verified {
			if !pf.done.Contains(pf.partRange(p)) {
				pf.verified[p] = false
			}
		}
	}
	for p := range pf.verified {
		if !pf.verified[p] && pf.done.Covered(pf.partRange(p)) > 0 {
			pf.unknown[p] = true
		}
	}
	return pf, nil
}

// LoadAll opens every download state file in dir. A state file that
// cannot be opened is skipped and reported in bad, so one broken or
// malicious download cannot stop all the others (or the daemon) from
// starting. err is only set when dir itself cannot be read.
func LoadAll(dir string) (pfs []*PartFile, bad []error, err error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.part.json"))
	if err != nil {
		return nil, nil, err
	}
	var out []*PartFile
	for _, m := range matches {
		pf, err := Open(m)
		if err != nil {
			bad = append(bad, err)
			continue
		}
		out = append(out, pf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.Before(out[j].Added) })
	return out, bad, nil
}

// Save writes the state file if anything changed.
func (pf *PartFile) Save() error {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if !pf.dirty {
		return nil
	}
	return pf.saveLocked()
}

func (pf *PartFile) saveLocked() error {
	if pf.deleted {
		// A save that raced with cancelling (the periodic save, a pause)
		// would bring the cancelled download back on the next start.
		return nil
	}
	m := partMeta{
		Hash:     pf.Hash.String(),
		Name:     pf.Name,
		Size:     pf.Size,
		Added:    pf.Added,
		Verified: pf.verified,
		Done:     [][2]int64{},
	}
	for _, h := range pf.hashSet {
		m.HashSet = append(m.HashSet, h.String())
	}
	for _, r := range pf.done {
		m.Done = append(m.Done, [2]int64{r.Start, r.End})
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if pf.f != nil {
		// Data before state, so the state never claims bytes that are not
		// on the disk: if they cannot be flushed, the old state stays.
		// (Some network and FUSE file systems cannot flush at all; there
		// the state is saved as before rather than never.)
		if err := pf.f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, syscall.ENOTSUP) {
			return err
		}
	}
	if err := fsutil.WriteFile(pf.metaPath, data, 0o600); err != nil {
		return err
	}
	pf.dirty = false
	return nil
}

// Close saves state and closes the data file.
func (pf *PartFile) Close() error {
	err := pf.Save()
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.f != nil {
		if cerr := pf.f.Close(); err == nil {
			err = cerr
		}
		pf.f = nil
	}
	return err
}

// Progress returns bytes done and total size.
func (pf *PartFile) Progress() (done, size int64) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.done.Total(), pf.Size
}

// Corrupt returns how many parts failed verification so far.
func (pf *PartFile) Corrupt() int {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.corrupt
}

// Complete reports whether every part has been verified.
func (pf *PartFile) Complete() bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.completeLocked()
}

func (pf *PartFile) completeLocked() bool {
	for _, v := range pf.verified {
		if !v {
			return false
		}
	}
	return true
}

// IsLarge reports whether 64-bit packets are needed.
func (pf *PartFile) IsLarge() bool { return pf.Size > peer.LargeFileLimit }

// NeedsHashSet reports whether we still need the part hashes from a peer.
func (pf *PartFile) NeedsHashSet() bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return !pf.rootIsPartHash() && pf.hashSet == nil
}

// SetHashSet installs the part hashes received from a peer after checking
// that they hash to the file's root hash.
func (pf *PartFile) SetHashSet(set []ed2k.Hash) error {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.hashSet != nil || pf.rootIsPartHash() {
		return nil
	}
	// eD2K hash sets have size/PartSize+1 entries (the last may be the
	// hash of an empty part when the size is an exact multiple).
	if want := int(pf.Size/ed2k.PartSize) + 1; len(set) != want {
		return fmt.Errorf("hashset has %d entries, want %d", len(set), want)
	}
	if !validHashSet(pf.Hash, pf.Size, set) {
		return errors.New("hashset does not match file hash")
	}
	pf.hashSet = set
	pf.dirty = true
	// Verify parts that completed before we had the hash set.
	for p := range pf.verified {
		if !pf.verified[p] && pf.done.Contains(pf.partRange(p)) {
			pf.verifyLocked(p)
		}
	}
	return nil
}

// validHashSet reports whether set is the hash set of a file with this
// hash and size.
func validHashSet(hash ed2k.Hash, size int64, set []ed2k.Hash) bool {
	if size < ed2k.PartSize || len(set) != int(size/ed2k.PartSize)+1 {
		return false
	}
	h := md4.New()
	for _, p := range set {
		h.Write(p[:])
	}
	return bytes.Equal(h.Sum(nil), hash[:])
}

// HasNeededParts reports whether a peer with the given availability
// (nil = complete file) has anything we still need.
func (pf *PartFile) HasNeededParts(avail []bool) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	for p, v := range pf.verified {
		if !v && (avail == nil || (p < len(avail) && avail[p])) {
			return true
		}
	}
	return false
}

// Reserve picks up to n blocks the peer can serve and we still need,
// marking them as owned by owner so other peers don't request them.
//
// Strategy: finish parts already in progress first (so they can be
// verified and shared sooner), then start fresh parts in random order to
// spread load across sources.
func (pf *PartFile) Reserve(owner string, avail []bool, n int) []Range {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.banned[owner] {
		return nil
	}
	now := time.Now()

	type cand struct {
		part int
		done int64
		rnd  int
	}
	// Blocks already requested count as progress too; otherwise a peer
	// asking for one block at a time would start a new part each time.
	inFlight := map[int]int64{}
	others := map[int]bool{} // parts where another peer holds live blocks
	for start, o := range pf.reserved {
		inFlight[int(start/ed2k.PartSize)] += peer.BlockSize
		// A stale reservation (nothing sent for reserveStale) does not
		// count: it would keep an isolated part from everybody else.
		if o != owner && !pf.staleLocked(start, o, now) {
			others[int(start/ed2k.PartSize)] = true
		}
	}
	var cands []cand
	for p, v := range pf.verified {
		if v || (avail != nil && (p >= len(avail) || !avail[p])) {
			continue
		}
		if pf.badPart[owner][p] > 1 {
			continue // this peer's copy of it is bad
		}
		if pf.isolate[p] && pf.writtenByOther(p, owner) && !others[p] && time.Since(pf.lastWr[p]) > isolateAbandon {
			// Its single peer went away: start the part over, and take
			// the part's blocks from it.
			pr := pf.partRange(p)
			pf.done.Remove(pr)
			delete(pf.writers, p)
			delete(pf.unknown, p)
			for start := pr.Start; start < pr.End; start += peer.BlockSize {
				delete(pf.reserved, start)
				delete(pf.resAt, start)
			}
			pf.dirty = true
		}
		if pf.isolate[p] && (others[p] || pf.writtenByOther(p, owner)) {
			continue // this part goes to a single peer now
		}
		cands = append(cands, cand{p, pf.done.Covered(pf.partRange(p)) + inFlight[p], rand.IntN(1 << 30)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].done != cands[j].done {
			return cands[i].done > cands[j].done
		}
		return cands[i].rnd < cands[j].rnd
	})

	var out []Range
	for _, c := range cands {
		pr := pf.partRange(c.part)
		for start := pr.Start; start < pr.End && len(out) < n; start += peer.BlockSize {
			blk := Range{start, min(start+peer.BlockSize, pr.End)}
			if pf.done.Contains(blk) {
				continue
			}
			if o, taken := pf.reserved[start]; taken && (o == owner || !pf.staleLocked(start, o, now)) {
				continue
			}
			// Free, or held by a peer that sent nothing for it for too
			// long: a peer must not be able to stall a download at 99%
			// by holding its last blocks.
			pf.reserved[start] = owner
			pf.resAt[start] = now
			out = append(out, blk)
		}
		if len(out) >= n {
			break
		}
	}
	return out
}

// Release returns a block reserved by owner to the pool.
func (pf *PartFile) Release(owner string, r Range) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.reserved[r.Start] == owner {
		delete(pf.reserved, r.Start)
		delete(pf.resAt, r.Start)
	}
}

// Has reports whether every byte of r has been written.
func (pf *PartFile) Has(r Range) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.done.Contains(r)
}

// staleLocked reports whether block start, reserved for o, may be taken
// over: nothing sent for it for reserveStale, and nothing from o at all
// for as long (a slow peer still sending its first pipelined block keeps
// the later ones).
func (pf *PartFile) staleLocked(start int64, o string, now time.Time) bool {
	return now.Sub(pf.resAt[start]) >= reserveStale && now.Sub(pf.ownerAt[o]) >= reserveStale
}

// reserveStale is how long a block stays reserved for a peer that sends
// nothing for it; then another peer may take it over.
const reserveStale = 3 * time.Minute

// maxPiecesPerBlock is how many separate pieces of a block may be on disk.
const maxPiecesPerBlock = 16

// ErrNotReserved is returned by Write for data the sender has no right to
// write: a block that is not reserved for it (any more), or one in a part
// that is already verified. Such data is dropped; it is not a disk error.
var ErrNotReserved = errors.New("block not reserved by this peer")

// blockAt returns the download block that contains off. Blocks start at
// each part's start and are peer.BlockSize long (the last one shorter).
func (pf *PartFile) blockAt(off int64) Range {
	pr := pf.partRange(int(off / ed2k.PartSize))
	start := pr.Start + (off-pr.Start)/peer.BlockSize*peer.BlockSize
	return Range{start, min(start+peer.BlockSize, pr.End)}
}

// Owns reports whether block r is still reserved for owner. A reservation
// is dropped when its part is verified or fails verification.
func (pf *PartFile) Owns(owner string, r Range) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.reserved[r.Start] == owner
}

// Write stores data that owner sent for off. It returns true if the write
// completed the whole file.
//
// The data must lie inside one block that is reserved for owner right now.
// Without this check a peer could keep sending a block after its part had
// failed verification and been downloaded again from others, overwriting
// a verified part with bytes of its choice.
func (pf *PartFile) Write(owner string, off int64, data []byte) (bool, error) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.f == nil {
		return false, errors.New("file closed")
	}
	end := off + int64(len(data))
	if off < 0 || end > pf.Size || end < off {
		return false, fmt.Errorf("write out of bounds: %d-%d", off, end)
	}
	blk := pf.blockAt(off)
	if end > blk.End || pf.reserved[blk.Start] != owner ||
		pf.verified[off/ed2k.PartSize] || pf.banned[owner] {
		return false, ErrNotReserved
	}
	// Honest peers send a block in a few large pieces, nearly always in
	// order. Countless tiny scattered pieces only serve to make our
	// bookkeeping slow, so a block may be split into a few pieces only.
	if pieces := pf.done.Pieces(blk) - pf.done.Pieces(Range{off, end}) + 1; pieces > maxPiecesPerBlock {
		return false, ErrNotReserved
	}
	if _, err := pf.f.WriteAt(data, off); err != nil {
		return false, err
	}
	pf.done.Add(Range{off, end})
	pf.dirty = true
	part := int(off / ed2k.PartSize)
	if pf.writers[part] == nil {
		pf.writers[part] = map[string]bool{}
	}
	pf.writers[part][owner] = true
	pf.resAt[blk.Start] = time.Now()
	pf.ownerAt[owner] = pf.resAt[blk.Start]
	if pf.isolate[part] {
		pf.lastWr[part] = time.Now()
	}

	// Verify any part this write completed.
	first := int(off / ed2k.PartSize)
	last := int((end - 1) / ed2k.PartSize)
	for p := first; p <= last; p++ {
		if !pf.verified[p] && pf.done.Contains(pf.partRange(p)) {
			pf.verifyLocked(p)
		}
	}
	return pf.completeLocked(), nil
}

// verifyLocked checks part p against its hash. On mismatch the part's
// data is discarded so it gets downloaded again.
func (pf *PartFile) verifyLocked(p int) {
	ok, known := pf.checkPartLocked(p)
	if !known {
		return // can't verify yet; SetHashSet will do it
	}
	pr := pf.partRange(p)
	if ok {
		pf.verified[p] = true
		pf.checked[p] = true
		delete(pf.isolate, p)
	} else {
		pf.done.Remove(pr)
		pf.corrupt++
		pf.blameLocked(p)
	}
	delete(pf.writers, p)
	delete(pf.unknown, p)
	delete(pf.lastWr, p)
	pf.dirty = true
	// Drop reservations inside this part either way: verified blocks are
	// no longer needed and corrupt ones must be re-reserved.
	for start := pr.Start; start < pr.End; start += peer.BlockSize {
		delete(pf.reserved, start)
		delete(pf.resAt, start)
	}
}

// banAfter is how many parts a peer may send on its own that fail
// verification before it is banned from the download. One can be an
// accident; more is a peer that keeps the file from ever finishing.
const banAfter = 2

// blameLocked handles part p failing verification. If one peer sent all
// of it, that is a strike against it; at banAfter strikes it gets no more
// blocks of this download (each bad block costs a whole 9.28 MB part, so
// such a peer could otherwise keep the file from ever completing). If
// several peers sent it, the culprit is unknown, so the part is
// downloaded again from a single peer, which settles it.
func (pf *PartFile) blameLocked(p int) {
	w := pf.writers[p]
	if len(w) == 1 && !pf.unknown[p] {
		for owner := range w {
			// A first failure may be an accident: it may send the part
			// again. Failing the same part twice means its copy is bad: it
			// is not asked for that part any more. Only bad data in
			// several different parts gets it banned, so a lone honest
			// source with one damaged part keeps serving the rest.
			if pf.badPart[owner] == nil {
				pf.badPart[owner] = map[int]int{}
			}
			pf.badPart[owner][p]++
			if pf.badPart[owner][p] > 1 {
				return
			}
			pf.strikes[owner]++
			if pf.strikes[owner] < banAfter {
				return
			}
			pf.banned[owner] = true
			for start, o := range pf.reserved {
				if o == owner {
					delete(pf.reserved, start)
					delete(pf.resAt, start)
				}
			}
			// What else it sent is just as suspect: download those parts
			// again (from the start, so nobody waits for it either).
			for q, ws := range pf.writers {
				if ws[owner] && !pf.verified[q] {
					pf.done.Remove(pf.partRange(q))
					delete(pf.writers, q)
					delete(pf.lastWr, q)
					pf.dirty = true
				}
			}
		}
		return
	}
	pf.isolate[p] = true
}

// isolateAbandon is how long a part being downloaded from a single peer
// waits for that peer before it is started over with another.
const isolateAbandon = 2 * time.Minute

// writtenByOther reports whether someone other than owner wrote data of
// part p in the current attempt (or it has data from before a restart).
func (pf *PartFile) writtenByOther(p int, owner string) bool {
	if pf.unknown[p] {
		return true
	}
	for o := range pf.writers[p] {
		if o != owner {
			return true
		}
	}
	return false
}

// Banned reports whether owner sent this download banAfter parts that
// failed verification on their own; it gets no more blocks of it.
func (pf *PartFile) Banned(owner string) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.banned[owner]
}

// BanIdentities bans, besides the owner key, other ways of naming the
// same peer (its IP address, its user hash): otherwise it could come
// back from another port, or with another LowID, and send bad data again.
func (pf *PartFile) BanIdentities(ids ...string) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	for _, id := range ids {
		if id != "" {
			pf.banned[id] = true
		}
	}
}

// BannedAny reports whether any of ids is banned from this download.
func (pf *PartFile) BannedAny(ids ...string) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	for _, id := range ids {
		if id != "" && pf.banned[id] {
			return true
		}
	}
	return false
}

// rootIsPartHash reports whether the file hash is the hash of its only
// part. That holds only below PartSize: a file of exactly PartSize bytes
// has a two-entry hash set (its part and an empty one) and its file hash
// is computed over both, so its part is checked against the hash set.
func (pf *PartFile) rootIsPartHash() bool { return pf.Size < ed2k.PartSize }

// checkPartLocked hashes part p from disk and compares it with the
// expected hash. known is false while the hash set is still missing.
func (pf *PartFile) checkPartLocked(p int) (ok, known bool) {
	want, known := pf.wantLocked(p)
	if !known {
		return false, false
	}
	return hashMatches(pf.f, pf.partRange(p), want), true
}

// wantLocked returns the expected hash of part p; false while the hash
// set is still missing.
func (pf *PartFile) wantLocked(p int) (ed2k.Hash, bool) {
	switch {
	case pf.rootIsPartHash():
		return pf.Hash, true
	case pf.hashSet != nil:
		return pf.hashSet[p], true
	}
	return ed2k.Hash{}, false
}

// hashMatches hashes r of f and compares it with want.
func hashMatches(f *os.File, r Range, want ed2k.Hash) bool {
	h := md4.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, r.Start, r.Len())); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), want[:])
}

// recheck hashes every part again before the file is handed over, so
// nothing written after a part was verified can slip through. Parts that
// no longer match are marked for download again.
//
// The hashing (a minute or more for a large file) runs without holding
// pf.mu, so everything asking this download for its state (the web
// interface, the scheduler) keeps working meanwhile. Nothing can write
// into a complete file: every part is verified, and Write refuses those.
func (pf *PartFile) recheck() error {
	pf.mu.Lock()
	f := pf.f
	if f == nil {
		pf.mu.Unlock()
		return errors.New("file closed")
	}
	if !pf.completeLocked() {
		pf.mu.Unlock()
		return errors.New("file not complete")
	}
	want := make([]ed2k.Hash, len(pf.verified))
	for p := range want {
		var known bool
		if want[p], known = pf.wantLocked(p); !known {
			pf.mu.Unlock()
			return errors.New("part hashes not known")
		}
	}
	pf.mu.Unlock()

	good := make([]bool, len(want))
	for p := range want {
		good[p] = hashMatches(f, pf.partRange(p), want[p])
	}

	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.f != f {
		return errors.New("file closed during the final check")
	}
	bad := 0
	for p := range pf.verified {
		if !good[p] {
			pf.verified[p] = false
			pf.checked[p] = false
			pf.done.Remove(pf.partRange(p))
			pf.corrupt++
			pf.dirty = true
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("final check: %d part(s) changed after verification; downloading them again", bad)
	}
	return nil
}

// Finish moves a completed file into dir and removes the state file.
// It returns the final path.
func (pf *PartFile) Finish(dir string) (string, error) {
	if err := pf.recheck(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	pf.mu.Lock()
	var checked os.FileInfo
	if pf.f != nil {
		checked, _ = pf.f.Stat()
	}
	pf.mu.Unlock()
	if err := pf.Close(); err != nil {
		return "", err
	}
	// What is moved must be the file just checked, not something put in
	// its place under the same name.
	if now, err := os.Lstat(pf.dataPath); err != nil || checked == nil || !now.Mode().IsRegular() || !os.SameFile(checked, now) {
		if rerr := pf.reopen(); rerr != nil {
			return "", fmt.Errorf("%s: %w", pf.dataPath, errNotRegular)
		}
		return "", fmt.Errorf("%s changed on disk; not finishing it", pf.dataPath)
	}
	dst, err := ReserveName(filepath.Join(dir, SanitizeName(pf.Name)))
	if err != nil {
		if rerr := pf.reopen(); rerr != nil {
			return "", fmt.Errorf("%w (and reopening it: %v)", err, rerr)
		}
		return "", err
	}
	if err := os.Rename(pf.dataPath, dst); err != nil {
		// Different volume: copy instead.
		if err := copyFile(pf.dataPath, dst); err != nil {
			os.Remove(dst) // the reserved, still empty name
			// Leave the download as it was, so finishing can be retried.
			if rerr := pf.reopen(); rerr != nil {
				return "", fmt.Errorf("%w (and reopening it: %v)", err, rerr)
			}
			return "", err
		}
		os.Remove(pf.dataPath)
	}
	os.Remove(pf.metaPath)
	return dst, nil
}

// reopen opens the data file again after a failed Finish.
func (pf *PartFile) reopen() error {
	f, err := openRegular(pf.dataPath, os.O_RDWR, false)
	if err != nil {
		return err
	}
	pf.mu.Lock()
	pf.f = f
	pf.mu.Unlock()
	return nil
}

// Delete removes the download's files from disk.
func (pf *PartFile) Delete() error {
	pf.mu.Lock()
	pf.deleted = true
	pf.dirty = false
	if pf.f != nil {
		pf.f.Close()
		pf.f = nil
	}
	pf.mu.Unlock()
	err1 := os.Remove(pf.dataPath)
	err2 := os.Remove(pf.metaPath)
	if err1 != nil && !os.IsNotExist(err1) {
		return err1
	}
	if err2 != nil && !os.IsNotExist(err2) {
		return err2
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := openRegular(src, os.O_RDONLY, false)
	if err != nil {
		return err
	}
	defer in.Close()
	// Copy under a temporary name and rename at the end, so dst never
	// exists half-written (a crash or a full disk midway leaves only the
	// temporary file, which is never shared). The temporary name is random
	// and created exclusively, so nothing planted there is written through.
	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.mymule-tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if err := out.Chmod(0o644); err != nil && runtime.GOOS != "windows" {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// maxNameBytes keeps final file names under the 255-byte limit of most
// file systems, with room for " (2)" and the like. Longer names made the
// final move fail and left the download stuck at 100%.
const maxNameBytes = 200

// SanitizeName makes a peer-supplied file name safe to use on any OS:
// no path, no characters Windows forbids, no control or invisible format
// characters (a right-to-left override can disguise "x<U+202E>vkm.exe" as
// "xexe.mkv"), no Windows device names, a length every file system takes,
// and no file types Windows Explorer acts on just by showing the folder.
func SanitizeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		case (r >= 0x7F && r <= 0x9F) || unicode.Is(unicode.Cf, r):
			// invisible: dropped
		default:
			b.WriteRune(r)
		}
	}
	// No dots or spaces (of any kind) at the end: Windows drops them, so
	// the file would not get the name it was given.
	s := strings.TrimLeftFunc(strings.TrimRightFunc(b.String(), dotOrSpace), unicode.IsSpace)
	if s == "" || s == "." || s == ".." {
		s = "download"
	}
	if len(s) > maxNameBytes {
		ext := filepath.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		s = strings.TrimRightFunc(netutil.Truncate(strings.TrimSuffix(s, ext), maxNameBytes-len(ext)), dotOrSpace) + ext
	}
	// Windows reserved device names (with any extension).
	switch strings.ToUpper(strings.TrimRight(strings.SplitN(s, ".", 2)[0], " ")) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
		"COM0", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "COM¹", "COM²", "COM³",
		"LPT0", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "LPT¹", "LPT²", "LPT³":
		s = "_" + s
	}
	// Files Explorer reads when it merely lists a folder: they can make
	// Windows connect to another machine and send the user's credentials.
	lower := strings.ToLower(s)
	switch filepath.Ext(lower) {
	case ".url", ".scf", ".lnk", ".library-ms", ".searchconnector-ms":
		s += "_"
	}
	if lower == "desktop.ini" {
		s = "_" + s
	}
	return s
}

func dotOrSpace(r rune) bool { return r == '.' || unicode.IsSpace(r) }

// ReserveName claims a free name for a new file: p, or p with " (1)",
// " (2)"... added. It creates the file, empty, with O_EXCL, so two
// downloads finishing at once with the same name cannot both get it (a
// check followed by a rename could); the caller then renames over it.
func ReserveName(p string) (string, error) {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	cand := p
	for i := 1; i < 10000; i++ {
		f, err := os.OpenFile(cand, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			f.Close()
			return cand, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err // a real problem (name too long, no permission)
		}
		cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
	return "", fmt.Errorf("too many files named %s", filepath.Base(p))
}

// VerifiedParts returns which parts have passed hash verification.
func (pf *PartFile) VerifiedParts() []bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return append([]bool(nil), pf.verified...)
}

// VerifiedCount returns how many parts have passed verification.
func (pf *PartFile) VerifiedCount() int {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	n := 0
	for _, v := range pf.verified {
		if v {
			n++
		}
	}
	return n
}

// HashSet returns a copy of the part hashes (nil if not known yet).
func (pf *PartFile) HashSet() []ed2k.Hash {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return append([]ed2k.Hash(nil), pf.hashSet...)
}

// ReadVerified reads data that lies entirely within verified parts, so we
// never upload unchecked (possibly corrupt) data to other peers.
func (pf *PartFile) ReadVerified(p []byte, off int64) (int, error) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	end := off + int64(len(p))
	if pf.f == nil || off < 0 || end > pf.Size || len(p) == 0 {
		return 0, errors.New("range not available")
	}
	for part := int(off / ed2k.PartSize); part <= int((end-1)/ed2k.PartSize); part++ {
		if !pf.verified[part] {
			return 0, errors.New("range not available")
		}
		if !pf.checked[part] {
			// Verified per the state file only: hash it once before
			// serving it, so a planted or damaged state file cannot make
			// us upload data that does not match the hash.
			if ok, _ := pf.checkPartLocked(part); !ok {
				pf.verified[part] = false
				pf.done.Remove(pf.partRange(part))
				pf.corrupt++
				pf.dirty = true
				return 0, errors.New("range not available")
			}
			pf.checked[part] = true
		}
	}
	return pf.f.ReadAt(p, off)
}
