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

	"mymule/internal/ed2k"
	"mymule/internal/md4"
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
	if link.Size <= 0 {
		return nil, errors.New("invalid file size")
	}
	base := filepath.Join(dir, link.Hash.String())
	if _, err := os.Stat(base + ".part.json"); err == nil {
		return nil, fmt.Errorf("already downloading %s", link.Hash)
	}
	pf := &PartFile{
		Hash:     link.Hash,
		Name:     link.Name,
		Size:     link.Size,
		Added:    time.Now(),
		verified: make([]bool, NumParts(link.Size)),
		reserved: map[int64]string{},
		dataPath: base + ".part",
		metaPath: base + ".part.json",
	}
	f, err := os.OpenFile(pf.dataPath, os.O_RDWR|os.O_CREATE, 0o644)
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
		return nil, err
	}
	pf := &PartFile{
		Hash:     h,
		Name:     m.Name,
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
	f, err := os.OpenFile(pf.dataPath, os.O_RDWR|os.O_CREATE, 0o644)
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

// LoadAll opens every download state file in dir.
func LoadAll(dir string) ([]*PartFile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.part.json"))
	if err != nil {
		return nil, err
	}
	var out []*PartFile
	for _, m := range matches {
		pf, err := Open(m)
		if err != nil {
			return out, err
		}
		out = append(out, pf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.Before(out[j].Added) })
	return out, nil
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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
	return len(pf.verified) > 1 && pf.hashSet == nil
}

// SetHashSet installs the part hashes received from a peer after checking
// that they hash to the file's root hash.
func (pf *PartFile) SetHashSet(set []ed2k.Hash) error {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.hashSet != nil || len(pf.verified) <= 1 {
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

// Write stores data at off. It returns true if the write completed the
// whole file.
func (pf *PartFile) Write(off int64, data []byte) (bool, error) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.f == nil {
		return false, errors.New("file closed")
	}
	end := off + int64(len(data))
	if off < 0 || end > pf.Size {
		return false, fmt.Errorf("write out of bounds: %d-%d", off, end)
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
	var want ed2k.Hash
	switch {
	case len(pf.verified) == 1:
		want = pf.Hash
	case pf.hashSet != nil:
		want = pf.hashSet[p]
	default:
		return // can't verify yet; SetHashSet will do it
	}
	pr := pf.partRange(p)
	h := md4.New()
	if _, err := io.Copy(h, io.NewSectionReader(pf.f, pr.Start, pr.Len())); err == nil &&
		bytes.Equal(h.Sum(nil), want[:]) {
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

// Finish moves a completed file into dir and removes the state file.
// It returns the final path.
func (pf *PartFile) Finish(dir string) (string, error) {
	if !pf.Complete() {
		return "", errors.New("file not complete")
	}
	if err := pf.Close(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := uniquePath(filepath.Join(dir, SanitizeName(pf.Name)))
	if err := os.Rename(pf.dataPath, dst); err != nil {
		// Different volume: copy instead.
		if err := copyFile(pf.dataPath, dst); err != nil {
			return "", err
		}
		os.Remove(pf.dataPath)
	}
	os.Remove(pf.metaPath)
	return dst, nil
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
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// SanitizeName makes a peer-supplied file name safe to use on any OS.
func SanitizeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	s := strings.TrimRight(b.String(), ". ")
	if s == "" || s == "." || s == ".." {
		s = "download"
	}
	// Windows reserved device names.
	upper := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	switch upper {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	return s
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}
