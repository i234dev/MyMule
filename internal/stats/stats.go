// Package stats keeps MyMule's all-time totals: bytes uploaded and
// downloaded since the first run that counted them, across restarts.
package stats

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"mymule/internal/fsutil"
)

// Totals are the all-time counts.
type Totals struct {
	Uploaded   int64     `json:"uploaded"`   // bytes sent to other users
	Downloaded int64     `json:"downloaded"` // bytes received from them
	Since      time.Time `json:"since"`      // when counting started
}

// saveEvery is how often the totals are written while running: a crash
// loses at most this much of the count.
const saveEvery = time.Minute

// Store holds the totals of earlier runs and adds this run's.
type Store struct {
	path    string
	current func() (up, down int64) // this run's counts so far
	log     func(string, ...any)

	mu   sync.Mutex
	base Totals // earlier runs
}

// Open reads the totals from path (none yet: counting starts now).
// current gives this run's counts, which only grow.
func Open(path string, current func() (up, down int64), log func(string, ...any)) *Store {
	if log == nil {
		log = func(string, ...any) {}
	}
	s := &Store{path: path, current: current, log: log}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(b, &s.base) != nil || s.base.Uploaded < 0 || s.base.Downloaded < 0 {
			log("stats: %s unreadable; counting from now", path)
			s.base = Totals{}
		}
	case !os.IsNotExist(err):
		log("stats: %v", err)
	}
	if s.base.Since.IsZero() {
		s.base.Since = time.Now().UTC().Truncate(time.Second)
	}
	return s
}

// Totals returns the all-time totals: earlier runs plus this one.
func (s *Store) Totals() Totals {
	up, down := s.current()
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.base
	t.Uploaded += up
	t.Downloaded += down
	return t
}

// Save writes the totals.
func (s *Store) Save() error {
	b, err := json.Marshal(s.Totals())
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(s.path, b, 0o600); err != nil {
		s.log("stats: %v", err)
		return err
	}
	return nil
}

// Run saves the totals every saveEvery until ctx ends, and once more then.
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
