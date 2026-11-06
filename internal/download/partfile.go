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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"mymule/internal/ed2k"
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

	done     RangeSet         // bytes written to disk
	verified []bool           // per part: hash checked OK
	reserved map[int64]string // block start -> owner (peer key)
	corrupt  int              // parts that failed verification (stats)

	f        *os.File
	dataPath string
	metaPath string
	dirty    bool
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

// Create starts a new download in dir.
func Create(dir string, link ed2k.Link) (*PartFile, error) {
	if !ed2k.ValidSize(link.Size) {
		return nil, fmt.Errorf("invalid file size %d", link.Size)
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
		reserved: map[int64]string{},
		dataPath: base + ".part",
		metaPath: base + ".part.json",
	}
	f, err := os.OpenFile(pf.dataPath, os.O_RDWR|os.O_CREATE, 0o600)
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
func Open(metaPath string) (*PartFile, error) {
	data, err := os.ReadFile(metaPath)
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
		reserved: map[int64]string{},
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
	for _, r := range m.Done {
		pf.done.Add(Range{r[0], min(r[1], pf.Size)})
	}
	copy(pf.verified, m.Verified)
	f, err := os.OpenFile(pf.dataPath, os.O_RDWR|os.O_CREATE, 0o600)
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
		pf.f.Sync() // data before state, so state never claims unwritten bytes
	}
	tmp := pf.metaPath + ".tmp"
	if err := writeSynced(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, pf.metaPath); err != nil {
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
		pf.f.Close()
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
	h := md4.New()
	for _, p := range set {
		h.Write(p[:])
	}
	if !bytes.Equal(h.Sum(nil), pf.Hash[:]) {
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

	type cand struct {
		part int
		done int64
		rnd  int
	}
	// Blocks already requested count as progress too; otherwise a peer
	// asking for one block at a time would start a new part each time.
	inFlight := map[int]int64{}
	for start := range pf.reserved {
		inFlight[int(start/ed2k.PartSize)] += peer.BlockSize
	}
	var cands []cand
	for p, v := range pf.verified {
		if v || (avail != nil && (p >= len(avail) || !avail[p])) {
			continue
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
			if _, taken := pf.reserved[start]; taken || pf.done.Contains(blk) {
				continue
			}
			pf.reserved[start] = owner
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
	}
}

// Has reports whether every byte of r has been written.
func (pf *PartFile) Has(r Range) bool {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return pf.done.Contains(r)
}

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
	if blk := pf.blockAt(off); end > blk.End || pf.reserved[blk.Start] != owner ||
		pf.verified[off/ed2k.PartSize] {
		return false, ErrNotReserved
	}
	if _, err := pf.f.WriteAt(data, off); err != nil {
		return false, err
	}
	pf.done.Add(Range{off, end})
	pf.dirty = true

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
	} else {
		pf.done.Remove(pr)
		pf.corrupt++
	}
	pf.dirty = true
	// Drop reservations inside this part either way: verified blocks are
	// no longer needed and corrupt ones must be re-reserved.
	for start := pr.Start; start < pr.End; start += peer.BlockSize {
		delete(pf.reserved, start)
	}
}

// rootIsPartHash reports whether the file hash is the hash of its only
// part. That holds only below PartSize: a file of exactly PartSize bytes
// has a two-entry hash set (its part and an empty one) and its file hash
// is computed over both, so its part is checked against the hash set.
func (pf *PartFile) rootIsPartHash() bool { return pf.Size < ed2k.PartSize }

// checkPartLocked hashes part p from disk and compares it with the
// expected hash. known is false while the hash set is still missing.
func (pf *PartFile) checkPartLocked(p int) (ok, known bool) {
	var want ed2k.Hash
	switch {
	case pf.rootIsPartHash():
		want = pf.Hash
	case pf.hashSet != nil:
		want = pf.hashSet[p]
	default:
		return false, false
	}
	pr := pf.partRange(p)
	h := md4.New()
	if _, err := io.Copy(h, io.NewSectionReader(pf.f, pr.Start, pr.Len())); err != nil {
		return false, true
	}
	return bytes.Equal(h.Sum(nil), want[:]), true
}

// recheck hashes every part again before the file is handed over, so
// nothing written after a part was verified can slip through. Parts that
// no longer match are marked for download again.
func (pf *PartFile) recheck() error {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.f == nil {
		return errors.New("file closed")
	}
	if !pf.completeLocked() {
		return errors.New("file not complete")
	}
	bad := 0
	for p := range pf.verified {
		if ok, _ := pf.checkPartLocked(p); !ok {
			pf.verified[p] = false
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
	if err := pf.Close(); err != nil {
		return "", err
	}
	dst := uniquePath(filepath.Join(dir, SanitizeName(pf.Name)))
	if err := os.Rename(pf.dataPath, dst); err != nil {
		// Different volume: copy instead.
		if err := copyFile(pf.dataPath, dst); err != nil {
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

// writeSynced writes a file and flushes it to disk before returning, so
// a rename that follows never exposes an empty or partial file after a
// power cut.
func writeSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// reopen opens the data file again after a failed Finish.
func (pf *PartFile) reopen() error {
	f, err := os.OpenFile(pf.dataPath, os.O_RDWR, 0o600)
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
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Copy under a temporary name and rename at the end, so dst never
	// exists half-written (a crash or a full disk midway leaves only the
	// temporary file, which is never shared and is replaced next time).
	tmp := dst + ".mymule-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
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
	s := strings.TrimSpace(strings.TrimRight(b.String(), ". "))
	if s == "" || s == "." || s == ".." {
		s = "download"
	}
	if len(s) > maxNameBytes {
		ext := filepath.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		s = strings.TrimRight(netutil.Truncate(strings.TrimSuffix(s, ext), maxNameBytes-len(ext)), ". ") + ext
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

// uniquePath returns p, or p with " (1)", " (2)"... added if it exists.
// Only an existing file moves it on to the next name: any other error
// (a name too long, a missing permission) returns the name as is, so the
// move that follows reports the real problem. This used to loop forever
// on such errors, leaving the download stuck at 100%.
func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p // free, or an error the move will report
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); err != nil {
			return cand // free, or an error the move will report
		}
	}
	return p
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
	}
	return pf.f.ReadAt(p, off)
}
