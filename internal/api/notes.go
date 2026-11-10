package api

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/kad"
)

// What users say about a file: eMule's ratings and comments, kept in Kad.
// The web interface shows them next to the files found for a movie or
// series, and the wish list does not take a file people call fake.

// Notes sums up the notes on a file.
type Notes struct {
	Votes    int       `json:"votes"`          // ratings given
	Fake     int       `json:"fake"`           // of them, "fake or corrupt"
	Average  float64   `json:"average"`        // of the ratings, 1 to 5 (0: none)
	Suspect  bool      `json:"suspect"`        // people call it fake (see summarize)
	Comments []Comment `json:"comments"`       // some of the comments
	Kad      bool      `json:"kad"`            // false: Kad is off, nothing could be asked
	Asked    time.Time `json:"asked,omitzero"` // when Kad was asked
}

// Comment is one user's note.
type Comment struct {
	Rating  int    `json:"rating"`
	Comment string `json:"comment"`
}

const (
	maxComments  = 10
	notesTTL     = time.Hour
	maxNotesKept = 2000
	notesWait    = 12 * time.Second // for the web interface
	notesAtOnce  = 4                // lookups running at the same time
)

// summarize turns notes (one per user and node that gave it) into a
// Notes. Every user's note is shown, but whether the file is suspect,
// which hides it, is decided only by the notes that nodes of at least two
// networks gave: one node can make up any number of users. A file is
// suspect when at least half of those ratings say "fake or corrupt" (one
// vote alone counts too: it is all there is).
func summarize(list []kad.Note) Notes {
	out := Notes{Kad: true, Comments: []Comment{}}
	type user struct {
		note kad.Note
		nets map[netip.Prefix]bool
	}
	var users []*user
	byHash := map[[16]byte]*user{}
	for _, n := range list {
		u := byHash[n.UserHash]
		if u == nil {
			u = &user{note: n, nets: map[netip.Prefix]bool{}}
			byHash[n.UserHash] = u
			users = append(users, u)
		}
		if n.Node.IsValid() {
			u.nets[nodeNet(n.Node)] = true
		}
	}
	sum, votes, fake := 0, 0, 0 // votes and fake: of the confirmed notes
	for _, u := range users {
		n := u.note
		if n.Rating > 0 {
			out.Votes++
			sum += n.Rating
			if n.Rating == 1 {
				out.Fake++
			}
			if len(u.nets) >= 2 {
				votes++
				if n.Rating == 1 {
					fake++
				}
			}
		}
		if n.Comment != "" && len(out.Comments) < maxComments {
			out.Comments = append(out.Comments, Comment{Rating: n.Rating, Comment: n.Comment})
		}
	}
	if out.Votes > 0 {
		out.Average = float64(sum) / float64(out.Votes)
	}
	out.Suspect = fake > 0 && fake*2 >= votes
	return out
}

// nodeNet is the network a node is in (a /24, or a /48 for IPv6): nodes
// of one network may be one owner's.
func nodeNet(ip netip.Addr) netip.Prefix {
	bits := 24
	if !ip.Is4() {
		bits = 48
	}
	p, _ := ip.Prefix(bits)
	return p
}

type notesCache struct {
	mu    sync.Mutex
	byKey map[ed2k.Hash]Notes
	slots chan struct{}
}

// fileNotes returns what users say about a file, from the cache or Kad.
func (r *REST) fileNotes(ctx context.Context, h ed2k.Hash, size uint64, wait time.Duration) (Notes, error) {
	c := &r.notes
	c.mu.Lock()
	if c.byKey == nil {
		c.byKey = map[ed2k.Hash]Notes{}
		c.slots = make(chan struct{}, notesAtOnce)
	}
	if n, ok := c.byKey[h]; ok && time.Since(n.Asked) < notesTTL {
		c.mu.Unlock()
		return n, nil
	}
	slots := c.slots
	c.mu.Unlock()

	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return Notes{}, ctx.Err()
	}
	list, err := r.notesSearch(ctx, h, size, wait)
	if errors.Is(err, download.ErrNoKad) {
		return Notes{Comments: []Comment{}}, nil
	}
	if err != nil && !errors.Is(err, kad.ErrBusy) {
		return Notes{}, err
	}
	n := summarize(list)
	n.Asked = time.Now()
	if err == nil { // a search cut short by another one is not kept
		c.mu.Lock()
		if len(c.byKey) >= maxNotesKept {
			clear(c.byKey)
		}
		c.byKey[h] = n
		c.mu.Unlock()
	}
	return n, nil
}

// notesSearch asks Kad (replaced in tests).
func (r *REST) notesSearch(ctx context.Context, h ed2k.Hash, size uint64, wait time.Duration) ([]kad.Note, error) {
	if r.testNotes != nil {
		return r.testNotes(h)
	}
	return r.m.FileNotes(ctx, h, size, wait)
}

// getNotes: GET /api/v1/files/{hash}/notes?size=N
func (r *REST) getNotes(w http.ResponseWriter, req *http.Request) {
	h, err := ed2k.ParseHash(req.PathValue("hash"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	size, err := strconv.ParseUint(req.URL.Query().Get("size"), 10, 64)
	if err != nil || size == 0 {
		fail(w, http.StatusBadRequest, errors.New("size: the file size in bytes"))
		return
	}
	n, err := r.fileNotes(req.Context(), h, size, notesWait)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	reply(w, map[string]any{"notes": n})
}
