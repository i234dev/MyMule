package api

import (
	"errors"
	"net/http"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/ratings"
)

// The user's ratings of files, published in Kad for other users (see
// package ratings).

// EnableRatings adds the endpoints to rate files.
func (r *REST) EnableRatings(s *ratings.Store) {
	r.ratings = s
	r.mux.HandleFunc("GET /api/v1/ratings", r.listRatings)
	r.mux.HandleFunc("PUT /api/v1/files/{hash}/rating", r.putRating)
}

func (r *REST) listRatings(w http.ResponseWriter, req *http.Request) {
	reply(w, map[string]any{"ratings": r.ratings.All()})
}

type ratingRequest struct {
	Rating  int    `json:"rating"` // 1 fake or corrupt ... 5 excellent; 0 removes it
	Comment string `json:"comment"`
	Name    string `json:"name"`
	Size    uint64 `json:"size"`
}

// putRating records the user's rating of a file and publishes it in Kad.
func (r *REST) putRating(w http.ResponseWriter, req *http.Request) {
	h, err := ed2k.ParseHash(req.PathValue("hash"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var rr ratingRequest
	if !decode(w, req, &rr) {
		return
	}
	if rr.Rating != 0 && (rr.Name == "" || rr.Size == 0) {
		fail(w, http.StatusBadRequest, errors.New("name and size of the file are needed"))
		return
	}
	rt := ratings.Rating{Name: rr.Name, Size: rr.Size, Rating: rr.Rating, Comment: rr.Comment}
	if err := r.ratings.Set(h, rt); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	published := false
	if rt.Rating > 0 {
		saved, _ := r.ratings.Get(h)
		if r.m.PublishNote(h, saved.Size, saved.Name, saved.Rating, saved.Comment) == nil {
			r.ratings.Published(h, time.Now())
			published = true
		}
	}
	reply(w, map[string]any{"saved": true, "published": published})
}
