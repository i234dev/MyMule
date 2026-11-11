// Package wanted keeps the wish list: movies and episodes the user wants
// but that were not found yet, or not in a version good enough. They are
// searched for again from time to time, and downloaded when found.
package wanted

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"mymule/internal/fsutil"
	"mymule/internal/release"
)

// Status of an entry.
const (
	Wanted      = "wanted"      // still being looked for
	Downloading = "downloading" // a file was found and is downloading
	Done        = "done"        // the download finished
	Following   = "following"   // a followed series (see follow.go)
)

// Entry is a movie, an episode or a season the user wants.
type Entry struct {
	ID      string `json:"id"` // WorkID, plus the season and episode
	WorkID  string `json:"work_id"`
	Kind    string `json:"kind"` // movie or series
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Poster  string `json:"poster,omitempty"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
	Lang    string `json:"lang"` // the language it was looked up in

	// NotBefore: it is not searched for before it comes out at home
	// (digital or disc) or airs; NotBeforeIs says which ("digital",
	// "disc", "aired", "estimated", or "manual": a date the user chose,
	// kept as it is); DateChecked: when that was last asked.
	NotBefore   time.Time `json:"not_before,omitzero"`
	NotBeforeIs string    `json:"not_before_is,omitempty"`
	DateChecked time.Time `json:"date_checked,omitzero"`
	// Force: search for it now, even before NotBefore (asked by the user).
	Force bool `json:"force,omitempty"`

	Status     string    `json:"status"`
	Added      time.Time `json:"added"`
	LastSearch time.Time `json:"last_search,omitzero"`
	Searches   int       `json:"searches"`
	// Hash and File: the download, once found.
	Hash string `json:"hash,omitempty"`
	File string `json:"file,omitempty"`
	// Skip: files of this entry the user cancelled, not taken again.
	Skip []string `json:"skip,omitempty"`

	// A followed series (Follow): every episode from FromSeason on, the
	// ones already aired and those to come (see follow.go). With ToSeason
	// (a whole season on the wish list), only the episodes up to it: the
	// entry is Done once they all are.
	Follow     bool              `json:"follow,omitempty"`
	FromSeason int               `json:"from_season,omitempty"`
	ToSeason   int               `json:"to_season,omitempty"`
	Episodes   []Episode         `json:"episodes,omitempty"`
	Searched   map[int]time.Time `json:"season_searched,omitempty"` // by season
}

// MakeID is the identifier of a work, season and episode.
func MakeID(workID string, season, episode int) string {
	if season == 0 && episode == 0 {
		return workID
	}
	return fmt.Sprintf("%s:s%de%d", workID, season, episode)
}

// MaxEntries bounds the list (each one costs searches on the servers).
const MaxEntries = 200

// ErrFull is returned when the list has MaxEntries entries.
var ErrFull = fmt.Errorf("the wish list is full (%d entries)", MaxEntries)

// ErrNotFound is returned for an entry that is not in the list.
var ErrNotFound = errors.New("not in the wish list")

// Store is the wish list, kept in a JSON file.
type Store struct {
	path string
	mu   sync.Mutex
	list []Entry
}

// Open loads the list from path (empty if it does not exist yet).
func Open(path string) *Store {
	s := &Store{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.list)
	}
	changed := false
	for i := range s.list {
		changed = toSeasonEntry(&s.list[i]) || changed
	}
	if changed {
		s.saveLocked()
	}
	return s
}

// toSeasonEntry turns a whole season saved by an older version (one file
// for the season, which was often a single episode) into one that gets
// every episode. A file it already got counts for its episode; one that
// held the whole season leaves the entry as it is.
func toSeasonEntry(e *Entry) bool {
	if e.Follow || e.Kind != "series" || e.Season == 0 || e.Episode != 0 {
		return false
	}
	var got []Episode
	if e.Hash != "" {
		n := release.Parse(e.File)
		if n.Episode == 0 {
			return false // the season in one file
		}
		got = append(got, Episode{Season: e.Season, Episode: n.Episode, Status: e.Status, Hash: e.Hash, File: e.File})
	}
	e.Follow, e.FromSeason, e.ToSeason = true, e.Season, e.Season
	e.Status, e.Hash, e.File, e.Episodes, e.Searched = Following, "", "", got, nil
	e.DateChecked = time.Time{} // its episodes are asked for at once
	return true
}

// List returns the entries, newest first.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.list))
	for i, e := range s.list {
		out[i] = cloneEntry(e) // the episodes change in place
	}
	slices.SortStableFunc(out, func(a, b Entry) int { return b.Added.Compare(a.Added) })
	return out
}

// Add puts e in the list, waiting to be searched for. An entry already
// there is kept as it is (and returned).
func (s *Store) Add(e Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.ID = MakeID(e.WorkID, e.Season, e.Episode)
	switch {
	case e.Kind == "series" && e.Season > 0 && e.Episode == 0:
		// A whole season: every episode of it (see follow.go).
		e.Follow, e.FromSeason, e.ToSeason = true, e.Season, e.Season
	case e.Follow:
		e.ID, e.Season, e.Episode, e.ToSeason = FollowID(e.WorkID), 0, 0, 0
	}
	for _, x := range s.list {
		if x.ID == e.ID {
			return x, nil
		}
	}
	if len(s.list) >= MaxEntries {
		return Entry{}, ErrFull
	}
	e.Status, e.Hash, e.File, e.Skip, e.Searches, e.Force = Wanted, "", "", nil, 0, false
	e.LastSearch = time.Time{}
	if e.Follow {
		e.Status, e.Episodes, e.Searched = Following, nil, nil
		e.FromSeason = max(e.FromSeason, 1)
	}
	if e.Added.IsZero() {
		e.Added = time.Now()
	}
	s.list = append(s.list, e)
	return e, s.saveLocked()
}

// Remove takes an entry out of the list.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return ErrNotFound
	}
	s.list = slices.Delete(s.list, i, i+1)
	return s.saveLocked()
}

// Get returns an entry.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.indexLocked(id); i >= 0 {
		return cloneEntry(s.list[i]), true
	}
	return Entry{}, false
}

// Due returns the entry that waits longest for a search, if one has not
// been searched for in every: those never searched for first. Entries not
// out yet (NotBefore) wait, unless the user asked for a search.
func (s *Store) Due(now time.Time, every time.Duration) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := -1
	for i, e := range s.list {
		if e.Status != Wanted || (!e.Force && (now.Sub(e.LastSearch) < every || e.NotBefore.After(now))) {
			continue
		}
		if e.Force {
			return e, true
		}
		if best < 0 || e.LastSearch.Before(s.list[best].LastSearch) {
			best = i
		}
	}
	if best < 0 {
		return Entry{}, false
	}
	return s.list[best], true
}

// Searched records a search for an entry that found nothing to take.
func (s *Store) Searched(id string, at time.Time) {
	s.update(id, func(e *Entry) {
		e.LastSearch = at
		e.Searches++
		e.Force = false
	})
}

// DateDue returns an entry not out yet whose date was not checked in
// every (dates move: a release gets announced, or brought forward).
func (s *Store) DateDue(now time.Time, every time.Duration) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.list {
		if e.Status == Wanted && e.NotBeforeIs != Manual && e.NotBefore.After(now) && now.Sub(e.DateChecked) >= every {
			return e, true
		}
	}
	return Entry{}, false
}

// Manual is the NotBeforeIs of a date the user chose.
const Manual = "manual"

// Dated records when an entry comes out (zero: not known). A date the
// user chose is kept.
func (s *Store) Dated(id string, notBefore time.Time, kind string, at time.Time) {
	s.update(id, func(e *Entry) {
		if e.NotBeforeIs != Manual {
			e.NotBefore, e.NotBeforeIs, e.DateChecked = notBefore, kind, at
		}
	})
}

// SetDate makes an entry wait until day (a date the user chose); a zero
// day goes back to the automatic date, which the caller looks up.
func (s *Store) SetDate(id string, day time.Time) error {
	ok := s.update(id, func(e *Entry) {
		if day.IsZero() {
			e.NotBefore, e.NotBeforeIs, e.DateChecked = time.Time{}, "", time.Time{}
		} else {
			e.NotBefore, e.NotBeforeIs, e.DateChecked = day, Manual, time.Time{}
		}
		e.Force = false
	})
	if !ok {
		return ErrNotFound
	}
	return nil
}

// SearchNow makes an entry due at once, even if it is not out yet.
func (s *Store) SearchNow(id string) error {
	ok := s.update(id, func(e *Entry) {
		if e.Status == Wanted || e.Follow {
			e.Force = true
		}
	})
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Found records the file being downloaded for an entry.
func (s *Store) Found(id, hash, file string, at time.Time) {
	s.update(id, func(e *Entry) {
		e.Status, e.Hash, e.File = Downloading, strings.ToUpper(hash), file
		e.LastSearch = at
		e.Searches++
		e.Force = false
	})
}

// Completed marks the entries downloading hash as done.
func (s *Store) Completed(hash string) {
	hash = strings.ToUpper(hash)
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.list {
		if s.list[i].Status == Downloading && s.list[i].Hash == hash {
			s.list[i].Status = Done
			changed = true
		}
		for j := range s.list[i].Episodes {
			ep := &s.list[i].Episodes[j]
			if ep.Status == Downloading && ep.Hash == hash {
				ep.Status = Done
				changed = true
			}
		}
		if seasonDone(s.list[i]) {
			s.list[i].Status = Done
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
}

// seasonDone reports whether a whole season on the wish list has every
// episode downloaded.
func seasonDone(e Entry) bool {
	if e.ToSeason == 0 || e.Status == Done || len(e.Episodes) == 0 {
		return false
	}
	for _, ep := range e.Episodes {
		if ep.Status != Done {
			return false
		}
	}
	return true
}

// Dropped puts an entry whose download went away (cancelled) back in the
// list, to be searched for again at once, and never takes that file again.
func (s *Store) Dropped(id string) {
	s.update(id, func(e *Entry) {
		if e.Status != Downloading {
			return
		}
		if e.Hash != "" && !slices.Contains(e.Skip, e.Hash) {
			e.Skip = append(e.Skip, e.Hash)
		}
		e.Status, e.Hash, e.File = Wanted, "", ""
		e.LastSearch = time.Time{}
	})
}

// Downloading returns the entries with a download going on.
func (s *Store) Downloading() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.list {
		if e.Status == Downloading {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) update(id string, f func(*Entry)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return false
	}
	f(&s.list[i])
	s.saveLocked()
	return true
}

func (s *Store) indexLocked(id string) int {
	return slices.IndexFunc(s.list, func(e Entry) bool { return e.ID == id })
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(s.path, b, 0o600)
}
