// Package share keeps the list of files we offer to other peers: complete
// files found in shared folders (hashed in the background, with a cache so
// restarts don't rehash) plus partial downloads registered by the
// download manager.
package share

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mymule/internal/config"
	"mymule/internal/ed2k"
	"mymule/internal/fsutil"
	"mymule/internal/library"
)

// File is something we can serve to peers.
type File interface {
	FileHash() ed2k.Hash
	FileName() string
	FileSize() int64
	// PartHashes returns the eD2K hash set for multi-part files (nil for
	// single-part files, or if still unknown).
	PartHashes() []ed2k.Hash
	// Avail returns which parts can be served; nil means all of them.
	Avail() []bool
	// ReadAt reads data; it must fail for ranges that are not available.
	ReadAt(p []byte, off int64) (int, error)
	Partial() bool
}

// ErrNotAvailable is returned when reading a range we don't have.
var ErrNotAvailable = errors.New("range not available")

// diskFile is a complete file on disk.
type diskFile struct {
	path  string
	name  string
	size  int64
	hash  ed2k.Hash
	parts []ed2k.Hash
}

func (d *diskFile) FileHash() ed2k.Hash     { return d.hash }
func (d *diskFile) FileName() string        { return d.name }
func (d *diskFile) FileSize() int64         { return d.size }
func (d *diskFile) PartHashes() []ed2k.Hash { return d.parts }
func (d *diskFile) Avail() []bool           { return nil }
func (d *diskFile) Partial() bool           { return false }

func (d *diskFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > d.size {
		return 0, ErrNotAvailable
	}
	f, err := os.Open(d.path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.ReadAt(p, off)
}

// cacheEntry is how a hashed file is remembered in known.json.
type cacheEntry struct {
	Size  int64     `json:"size"`
	MTime int64     `json:"mtime"`
	Hash  ed2k.Hash `json:"hash"`
	Parts []string  `json:"parts,omitempty"`
}

// Status summarises the library for display.
type Status struct {
	Files     int    `json:"files"`
	Partial   int    `json:"partial"`
	Bytes     int64  `json:"bytes"`
	Hashing   string `json:"hashing,omitempty"`    // file being hashed right now
	ToHash    int    `json:"to_hash,omitempty"`    // files waiting to be hashed
	HashedPct int    `json:"hashed_pct,omitempty"` // progress of the current file
}

// Library is the set of shared files.
type Library struct {
	dirs      []string
	cachePath string
	log       func(string, ...any)

	mu       sync.Mutex
	trees    []string             // folders shared with their subfolders
	private  []string             // folders whose own files are never shared (the data folder)
	hidden   []string             // folders never shared at all (the temporary folder)
	disk     map[string]*diskFile // by path
	partial  map[ed2k.Hash]File
	byHash   map[ed2k.Hash]File
	cache    map[string]cacheEntry
	version  int
	hashing  string
	toHash   int
	hashPct  int
	rescanCh chan struct{}
}

// NewLibrary shares the files in dirs (not recursive, like eMule).
// cachePath is where computed hashes are remembered.
func NewLibrary(dirs []string, cachePath string, log func(string, ...any)) *Library {
	if log == nil {
		log = func(string, ...any) {}
	}
	l := &Library{
		dirs:      dirs,
		cachePath: cachePath,
		log:       log,
		disk:      map[string]*diskFile{},
		partial:   map[ed2k.Hash]File{},
		byHash:    map[ed2k.Hash]File{},
		cache:     map[string]cacheEntry{},
		rescanCh:  make(chan struct{}, 1),
	}
	if data, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(data, &l.cache)
	}
	return l
}

// Run scans the shared folders now and then every interval, or when
// Rescan is called, until ctx ends.
func (l *Library) Run(ctx context.Context, interval time.Duration) {
	for {
		l.scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-l.rescanCh:
		}
	}
}

// SetTrees sets the folders that are shared together with their
// subfolders (movie and series libraries), and rescans.
func (l *Library) SetTrees(dirs []string) {
	l.mu.Lock()
	l.trees = append([]string{}, dirs...)
	l.mu.Unlock()
	l.Rescan()
}

// Protect keeps MyMule's own files off the network whatever the shared
// folders are: files directly in each of dataDirs (mymule.conf with the
// api_key, api.json, known.json, the log...) and everything in each of
// tempDirs. Subfolders of a data folder (such as downloads/) are not
// affected.
func (l *Library) Protect(dataDirs, tempDirs []string) {
	clean := func(in []string) (out []string) {
		for _, d := range in {
			if d == "" {
				continue
			}
			if abs, err := filepath.Abs(d); err == nil {
				out = append(out, filepath.Clean(abs))
			}
		}
		return out
	}
	l.mu.Lock()
	l.private, l.hidden = clean(dataDirs), clean(tempDirs)
	l.mu.Unlock()
	l.Rescan()
}

// protectedLocked reports whether path must never be shared (see Protect).
func (l *Library) protectedLocked(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	dir := filepath.Dir(abs)
	for _, d := range l.private {
		if strings.EqualFold(d, dir) {
			return true
		}
	}
	for _, d := range l.hidden {
		if rel, err := filepath.Rel(d, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// maxTreeDepth limits how deep shared trees are walked.
const maxTreeDepth = 5

// Rescan asks Run to scan the folders again soon.
func (l *Library) Rescan() {
	select {
	case l.rescanCh <- struct{}{}:
	default:
	}
}

// skipName reports files that must never be shared.
func skipName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(name, ".") || strings.HasSuffix(lower, ".part") ||
		strings.HasSuffix(lower, ".part.json") || strings.HasSuffix(lower, ".tmp") ||
		strings.HasSuffix(lower, ".mymule-tmp") ||
		lower == "desktop.ini" || lower == "thumbs.db" {
		return true
	}
	// MyMule's own files, wherever they are (a second line behind Protect).
	for _, f := range config.PrivateFiles {
		if strings.EqualFold(name, f) {
			return true
		}
	}
	return false
}

// skipDirName reports folders never walked inside a shared tree: hidden
// ones, and the recycle bins, snapshots and index folders that NAS boxes
// and Windows keep inside shares (they hold files the user deleted).
func skipDirName(name string) bool {
	switch strings.ToLower(name) {
	case "#recycle", "@recycle", "$recycle.bin", "recycler", "#snapshot", "@eadir",
		"@tmp", "system volume information", "lost+found", ".trash", ".trashes":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func (l *Library) scan(ctx context.Context) {
	type cand struct {
		path  string
		size  int64
		mtime int64
	}
	var found []cand
	l.mu.Lock()
	trees := append([]string{}, l.trees...)
	protected := func(path string) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.protectedLocked(path)
	}
	l.mu.Unlock()
	for _, root := range trees {
		root = filepath.Clean(root)
		filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() {
				rel, _ := filepath.Rel(root, path)
				if path != root && (skipDirName(e.Name()) || strings.Count(rel, string(filepath.Separator)) >= maxTreeDepth) {
					return filepath.SkipDir
				}
				return nil
			}
			// Trees are the movie and series folders: only media there.
			if !e.Type().IsRegular() || skipName(e.Name()) || !library.IsMedia(e.Name()) || protected(path) {
				return nil
			}
			if info, err := e.Info(); err == nil && info.Size() > 0 {
				found = append(found, cand{path, info.Size(), info.ModTime().UnixNano()})
			}
			return nil
		})
	}
	for _, dir := range l.dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || skipName(e.Name()) || protected(filepath.Join(dir, e.Name())) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() == 0 {
				continue
			}
			found = append(found, cand{filepath.Join(dir, e.Name()), info.Size(), info.ModTime().UnixNano()})
		}
	}

	// Forget files that disappeared or changed.
	seen := map[string]cand{}
	for _, c := range found {
		seen[c.path] = c
	}
	l.mu.Lock()
	for path, d := range l.disk {
		if c, ok := seen[path]; !ok || c.size != d.size || c.mtime != l.cache[path].MTime {
			delete(l.disk, path)
			l.reindexLocked()
		}
	}
	var todo []cand
	for _, c := range found {
		if _, ok := l.disk[c.path]; ok {
			continue
		}
		if ce, ok := l.cache[c.path]; ok && ce.Size == c.size && ce.MTime == c.mtime {
			l.addDiskLocked(c.path, ce)
			continue
		}
		todo = append(todo, c)
	}
	l.toHash = len(todo)
	l.mu.Unlock()

	for i, c := range todo {
		if ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		l.hashing, l.toHash, l.hashPct = filepath.Base(c.path), len(todo)-i, 0
		l.mu.Unlock()
		fh, err := ed2k.HashFile(c.path, func(done int64) {
			l.mu.Lock()
			l.hashPct = int(done * 100 / c.size)
			l.mu.Unlock()
		})
		if err != nil {
			l.log("cannot hash %s: %v", c.path, err)
			continue
		}
		ce := cacheEntry{Size: c.size, MTime: c.mtime, Hash: fh.Root}
		if len(fh.Parts) > 1 {
			for _, p := range fh.Parts {
				ce.Parts = append(ce.Parts, p.String())
			}
		}
		l.mu.Lock()
		l.cache[c.path] = ce
		l.addDiskLocked(c.path, ce)
		l.mu.Unlock()
		l.saveCache()
		l.log("shared %s", filepath.Base(c.path))
	}
	l.mu.Lock()
	l.hashing, l.toHash, l.hashPct = "", 0, 0
	// Drop cache entries for files that no longer exist.
	for path := range l.cache {
		if _, ok := seen[path]; !ok {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				delete(l.cache, path)
			}
		}
	}
	l.mu.Unlock()
}

func (l *Library) addDiskLocked(path string, ce cacheEntry) {
	d := &diskFile{path: path, name: filepath.Base(path), size: ce.Size, hash: ce.Hash}
	for _, s := range ce.Parts {
		if h, err := ed2k.ParseHash(s); err == nil {
			d.parts = append(d.parts, h)
		}
	}
	l.disk[path] = d
	l.reindexLocked()
}

// reindexLocked rebuilds the hash index and bumps the version so the
// server offer gets refreshed.
func (l *Library) reindexLocked() {
	l.byHash = map[ed2k.Hash]File{}
	for _, f := range l.partial {
		l.byHash[f.FileHash()] = f
	}
	for _, d := range l.disk { // complete files win over partial ones
		l.byHash[d.hash] = d
	}
	l.version++
}

func (l *Library) saveCache() {
	l.mu.Lock()
	data, err := json.Marshal(l.cache)
	l.mu.Unlock()
	if err != nil || l.cachePath == "" {
		return
	}
	fsutil.WriteFile(l.cachePath, data, 0o600)
}

// AddComplete shares a file whose hashes are already known (e.g. a
// download that just finished), without hashing it again.
func (l *Library) AddComplete(path string, hash ed2k.Hash, parts []ed2k.Hash) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	ce := cacheEntry{Size: info.Size(), MTime: info.ModTime().UnixNano(), Hash: hash}
	if len(parts) > 1 {
		for _, p := range parts {
			ce.Parts = append(ce.Parts, p.String())
		}
	}
	l.mu.Lock()
	l.cache[path] = ce
	if l.inSharedDirLocked(path) {
		l.addDiskLocked(path, ce)
	}
	l.mu.Unlock()
	l.saveCache()
}

func (l *Library) inSharedDirLocked(path string) bool {
	if skipName(filepath.Base(path)) || l.protectedLocked(path) {
		return false
	}
	dir := filepath.Clean(filepath.Dir(path))
	for _, d := range l.dirs {
		if strings.EqualFold(filepath.Clean(d), dir) {
			return true
		}
	}
	if !library.IsMedia(path) {
		return false // trees only share media
	}
	for _, t := range l.trees {
		if rel, err := filepath.Rel(filepath.Clean(t), dir); err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Moved records that a shared or known file was moved or renamed, so it
// is not hashed again; it stays shared if the new place is shared.
func (l *Library) Moved(oldPath, newPath string) {
	info, err := os.Stat(newPath)
	if err != nil {
		return
	}
	l.mu.Lock()
	ce, ok := l.cache[oldPath]
	if ok {
		ce.MTime = info.ModTime().UnixNano()
		delete(l.cache, oldPath)
		delete(l.disk, oldPath)
		l.cache[newPath] = ce
		if l.inSharedDirLocked(newPath) {
			l.addDiskLocked(newPath, ce)
		} else {
			l.reindexLocked()
		}
	}
	l.mu.Unlock()
	if ok {
		l.saveCache()
	}
}

// SetPartial shares (or with f == nil, stops sharing) a partial download.
func (l *Library) SetPartial(h ed2k.Hash, f File) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f == nil {
		delete(l.partial, h)
	} else {
		l.partial[h] = f
	}
	l.reindexLocked()
}

// Touch marks the library as changed (e.g. a partial file verified a new
// part), so the next server offer includes it.
func (l *Library) Touch() {
	l.mu.Lock()
	l.version++
	l.mu.Unlock()
}

// Get returns the shared file with hash h, if any.
func (l *Library) Get(h ed2k.Hash) File {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byHash[h]
}

// List returns every shared file, sorted by name.
func (l *Library) List() []File {
	l.mu.Lock()
	out := make([]File, 0, len(l.byHash))
	for _, f := range l.byHash {
		out = append(out, f)
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].FileName() < out[j].FileName() })
	return out
}

// Version changes whenever the set of shared files changes.
func (l *Library) Version() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.version
}

// Status summarises the library.
func (l *Library) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Status{Hashing: l.hashing, ToHash: l.toHash, HashedPct: l.hashPct}
	for _, f := range l.byHash {
		s.Files++
		if f.Partial() {
			s.Partial++
		}
		s.Bytes += f.FileSize()
	}
	return s
}

// ReadFull reads exactly len(p) bytes or fails.
func ReadFull(f File, p []byte, off int64) error {
	n, err := f.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}
