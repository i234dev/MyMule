// Package ratings keeps what the user says about files (a rating, from 1
// fake or corrupt to 5 excellent, and a comment) and publishes it in Kad,
// as eMule does, so other users see it. Kad nodes keep notes for a day, so
// they are published again while they are kept here.
package ratings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"mymule/internal/ed2k"
	"mymule/internal/fsutil"
)

// Rating is what the user said about a file.
type Rating struct {
	Name      string    `json:"name"`
	Size      uint64    `json:"size"`
	Rating    int       `json:"rating"`  // 1 fake or corrupt, 2 poor, 3 fair, 4 good, 5 excellent
	Comment   string    `json:"comment"` // may be empty
	At        time.Time `json:"at"`
	Published time.Time `json:"published,omitzero"`
}

// Limits.
const (
	MaxComment = 128 // eMule's
	MaxRatings = 2000
)

// ErrBad is returned for a rating out of range.
var ErrBad = errors.New("rating must be 1 (fake or corrupt) to 5 (excellent)")

// Store is the user's ratings, by file hash.
type Store struct {
	path string
	mu   sync.Mutex
	m    map[ed2k.Hash]Rating
}

// Open loads the ratings from path.
func Open(path string) *Store {
	s := &Store{path: path, m: map[ed2k.Hash]Rating{}}
	if b, err := os.ReadFile(path); err == nil {
		var list map[string]Rating
		if json.Unmarshal(b, &list) == nil {
			for k, v := range list {
				if h, err := ed2k.ParseHash(k); err == nil {
					s.m[h] = v
				}
			}
		}
	}
	return s
}

// Clean makes a comment one line of at most MaxComment characters.
func Clean(c string) string {
	c = strings.Join(strings.FieldsFunc(c, func(r rune) bool { return unicode.IsControl(r) }), " ")
	c = strings.TrimSpace(c)
	if r := []rune(c); len(r) > MaxComment {
		c = string(r[:MaxComment])
	}
	return c
}

// Set records a rating (0 removes it).
func (s *Store) Set(h ed2k.Hash, r Rating) error {
	if r.Rating < 0 || r.Rating > 5 {
		return ErrBad
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Rating == 0 {
		delete(s.m, h)
		return s.saveLocked()
	}
	if _, ok := s.m[h]; !ok && len(s.m) >= MaxRatings {
		return errors.New("too many ratings")
	}
	r.Comment = Clean(r.Comment)
	r.At, r.Published = time.Now(), time.Time{}
	s.m[h] = r
	return s.saveLocked()
}

// Get returns the user's rating of a file.
func (s *Store) Get(h ed2k.Hash) (Rating, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[h]
	return r, ok
}

// Due returns a rating not published in every.
func (s *Store) Due(now time.Time, every time.Duration) (ed2k.Hash, Rating, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, r := range s.m {
		if now.Sub(r.Published) >= every {
			return h, r, true
		}
	}
	return ed2k.Hash{}, Rating{}, false
}

// Published records that a rating was sent to Kad.
func (s *Store) Published(h ed2k.Hash, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[h]; ok {
		r.Published = at
		s.m[h] = r
		s.saveLocked()
	}
}

// Publisher sends a note to Kad.
type Publisher func(h ed2k.Hash, size uint64, name string, rating int, comment string) error

// Run publishes the ratings, again every 12 hours, one every gap (Kad
// keeps notes for a day). It returns when ctx ends.
func (s *Store) Run(ctx context.Context, publish Publisher, gap time.Duration) {
	const every = 12 * time.Hour
	t := time.NewTicker(gap)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h, r, ok := s.Due(time.Now(), every)
		if !ok {
			continue
		}
		if err := publish(h, r.Size, r.Name, r.Rating, r.Comment); err == nil {
			s.Published(h, time.Now())
		}
	}
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	out := make(map[string]Rating, len(s.m))
	for h, r := range s.m {
		out[h.String()] = r
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(s.path, b, 0o600)
}

// All returns every rating, by hash.
func (s *Store) All() map[string]Rating {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Rating, len(s.m))
	for h, r := range s.m {
		out[h.String()] = r
	}
	return out
}
