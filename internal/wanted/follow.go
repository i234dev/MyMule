package wanted

import (
	"slices"
	"strings"
	"time"
)

// A followed series: MyMule gets every episode from a season on, those
// already aired and those to come. The entry keeps the list of episodes
// (from the movie database) and where each one is; a season is searched
// for as a whole, and the best file of each episode taken.

// Episode of a followed series.
type Episode struct {
	Season  int       `json:"season"`
	Episode int       `json:"episode"`
	Name    string    `json:"name,omitempty"`
	Airs    time.Time `json:"airs,omitzero"` // zero: no date yet (not aired)
	Status  string    `json:"status"`        // Wanted, Downloading or Done
	Hash    string    `json:"hash,omitempty"`
	File    string    `json:"file,omitempty"`
	Skip    []string  `json:"skip,omitempty"` // files cancelled, not taken again
}

// Aired reports whether the episode has aired by now.
func (e Episode) Aired(now time.Time) bool { return !e.Airs.IsZero() && !e.Airs.After(now) }

// FollowID is the identifier of a followed series.
func FollowID(workID string) string { return workID + ":follow" }

// SetEpisodes merges the episodes the movie database lists into a followed
// series: new ones are added as wanted (those before FromSeason are left
// out), known ones get their name and air date updated.
func (s *Store) SetEpisodes(id string, list []Episode, at time.Time) {
	s.update(id, func(e *Entry) {
		for _, n := range list {
			if n.Season < e.FromSeason || (e.ToSeason > 0 && n.Season > e.ToSeason) {
				continue
			}
			i := slices.IndexFunc(e.Episodes, func(x Episode) bool { return x.Season == n.Season && x.Episode == n.Episode })
			if i >= 0 {
				e.Episodes[i].Name, e.Episodes[i].Airs = n.Name, n.Airs
				continue
			}
			n.Status, n.Hash, n.File, n.Skip = Wanted, "", "", nil
			e.Episodes = append(e.Episodes, n)
		}
		slices.SortFunc(e.Episodes, func(a, b Episode) int {
			if a.Season != b.Season {
				return a.Season - b.Season
			}
			return a.Episode - b.Episode
		})
		e.DateChecked = at
	})
}

// FollowDue returns a followed series with a season to search for: one
// with aired episodes still wanted, not searched for in every (the user's
// Search now makes it due at once). The lowest such season comes first.
func (s *Store) FollowDue(now time.Time, every time.Duration) (Entry, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.list {
		if !e.Follow || e.Status == Done || (e.NotBeforeIs == Manual && e.NotBefore.After(now) && !e.Force) {
			continue
		}
		for _, ep := range e.Episodes {
			if ep.Status != Wanted || !ep.Aired(now) {
				continue
			}
			if e.Force || now.Sub(e.Searched[ep.Season]) >= every {
				return cloneEntry(e), ep.Season, true
			}
		}
	}
	return Entry{}, 0, false
}

// FollowListDue returns a followed series whose list of episodes was not
// asked for in every (new episodes get announced).
func (s *Store) FollowListDue(now time.Time, every time.Duration) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.list {
		if e.Follow && e.Status != Done && now.Sub(e.DateChecked) >= every {
			return cloneEntry(e), true
		}
	}
	return Entry{}, false
}

// SeasonSearched records a search for a season of a followed series.
func (s *Store) SeasonSearched(id string, season int, at time.Time) {
	s.update(id, func(e *Entry) {
		if e.Searched == nil {
			e.Searched = map[int]time.Time{}
		}
		e.Searched[season] = at
		e.LastSearch = at
		e.Searches++
		e.Force = false
	})
}

// EpisodeFound records the file downloading for an episode.
func (s *Store) EpisodeFound(id string, season, episode int, hash, file string) {
	s.update(id, func(e *Entry) {
		for i := range e.Episodes {
			ep := &e.Episodes[i]
			if ep.Season == season && ep.Episode == episode {
				ep.Status, ep.Hash, ep.File = Downloading, strings.ToUpper(hash), file
			}
		}
	})
}

// EpisodeDropped puts an episode whose download went away (cancelled)
// back to wanted, and never takes that file again; its season is searched
// for again soon.
func (s *Store) EpisodeDropped(id, hash string) {
	hash = strings.ToUpper(hash)
	s.update(id, func(e *Entry) {
		for i := range e.Episodes {
			ep := &e.Episodes[i]
			if ep.Status == Downloading && ep.Hash == hash {
				ep.Skip = append(ep.Skip, hash)
				ep.Status, ep.Hash, ep.File = Wanted, "", ""
				delete(e.Searched, ep.Season)
				if e.Status == Done {
					e.Status = Following
				}
			}
		}
	})
}

// FollowDownloads lists the downloads of followed series: entry id and
// hash.
func (s *Store) FollowDownloads() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][2]string
	for _, e := range s.list {
		for _, ep := range e.Episodes {
			if ep.Status == Downloading {
				out = append(out, [2]string{e.ID, ep.Hash})
			}
		}
	}
	return out
}

func cloneEntry(e Entry) Entry {
	e.Episodes = slices.Clone(e.Episodes)
	return e
}
