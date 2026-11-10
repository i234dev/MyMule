package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/meta"
	"mymule/internal/wanted"
)

// The wish list: movies and episodes searched for again from time to
// time, and downloaded (as if picked in the web interface) once a file
// good enough for the search preferences shows up.

// Timing of the wish list searches. One search at a time, a few minutes
// apart, so that a long list does not flood the servers.
var (
	wantedTick  = 10 * time.Minute // between two searches
	wantedFirst = time.Minute      // after starting (time to connect)
	wantedEvery = 6 * time.Hour    // between two searches of the same entry
	wantedLimit = 2 * time.Minute  // for one search
	wantedDates = 24 * time.Hour   // between two checks of a release date
)

// EnableWanted adds the wish list endpoints. RunWanted searches for it.
// It needs EnableMedia and EnableLibrary.
func (r *REST) EnableWanted(s *wanted.Store, logf func(string, ...any)) {
	r.wanted, r.logf = s, logf
	r.nudge = make(chan struct{}, 1)
	r.mux.HandleFunc("GET /api/v1/wanted", r.listWanted)
	r.mux.HandleFunc("POST /api/v1/wanted", r.addWanted)
	r.mux.HandleFunc("DELETE /api/v1/wanted/{id}", r.removeWanted)
	r.mux.HandleFunc("POST /api/v1/wanted/{id}/search", r.searchWanted)
}

func (r *REST) listWanted(w http.ResponseWriter, req *http.Request) {
	list := r.wanted.List()
	if list == nil {
		list = []wanted.Entry{}
	}
	reply(w, map[string]any{"wanted": list})
}

type addWantedRequest struct {
	WorkID  string `json:"work_id"`
	Lang    string `json:"lang"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

func (r *REST) addWanted(w http.ResponseWriter, req *http.Request) {
	var ar addWantedRequest
	if !decode(w, req, &ar) {
		return
	}
	if ar.Season < 0 || ar.Episode < 0 || ar.Season > 1000 || ar.Episode > 10000 {
		fail(w, http.StatusBadRequest, errors.New("bad season or episode"))
		return
	}
	if ar.Lang == "" {
		ar.Lang = "en"
	}
	work, err := r.media.Get(req.Context(), ar.WorkID, ar.Lang)
	if err != nil {
		fail(w, http.StatusBadGateway, mediaError{err})
		return
	}
	e := wanted.Entry{WorkID: work.ID, Kind: work.Kind, Title: work.Title, Year: work.Year,
		Poster: work.Poster, Lang: ar.Lang}
	if work.Kind == "series" {
		e.Season, e.Episode = ar.Season, ar.Episode
	}
	if a, ok := r.availability(req.Context(), work.ID, e.Season, e.Episode); ok {
		e.NotBefore, e.NotBeforeIs, e.DateChecked = a.Date, a.Kind, time.Now()
	}
	e, err = r.wanted.Add(e)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	r.wake()
	reply(w, map[string]any{"entry": e})
}

func (r *REST) removeWanted(w http.ResponseWriter, req *http.Request) {
	if err := r.wanted.Remove(req.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	reply(w, map[string]any{"removed": true})
}

func (r *REST) searchWanted(w http.ResponseWriter, req *http.Request) {
	if err := r.wanted.SearchNow(req.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	r.wake()
	reply(w, map[string]any{"searching": true})
}

// wake makes RunWanted look at the list now.
func (r *REST) wake() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// WantedCompleted marks the wish list entries downloading h as done
// (called when a download finishes).
func (r *REST) WantedCompleted(h ed2k.Hash) {
	if r.wanted != nil {
		r.wanted.Completed(h.String())
	}
}

// RunWanted searches for the wish list until ctx ends: one entry at a
// time, every wantedTick (or when an entry is added).
func (r *REST) RunWanted(ctx context.Context) {
	if r.wanted == nil {
		return
	}
	timer := time.NewTimer(wantedFirst)
	defer timer.Stop()
	last := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.nudge:
			// An entry added or asked for: search now, unless a search
			// was made just before (the servers limit them).
			if wait := time.Until(last.Add(wantedTick / 10)); wait > 0 {
				timer.Reset(wait)
				continue
			}
		}
		r.checkWantedDownloads()
		r.checkWantedDate(ctx)
		if e, ok := r.wanted.Due(time.Now(), wantedEvery); ok {
			last = time.Now()
			r.searchWantedEntry(ctx, e)
		}
		timer.Reset(wantedTick)
	}
}

// availability is when a work (or an episode, or a season) comes out at
// home, if the movie database knows it.
func (r *REST) availability(ctx context.Context, id string, season, episode int) (meta.Availability, bool) {
	ad, ok := r.media.(meta.AvailabilityDater)
	if !ok {
		return meta.Availability{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	a, ok, err := ad.Available(ctx, id, season, episode)
	if err != nil || !ok {
		return meta.Availability{}, false
	}
	return a, true
}

// checkWantedDate checks again the release date of an entry not out yet,
// once a day: dates get announced, brought forward or put off.
func (r *REST) checkWantedDate(ctx context.Context) {
	e, ok := r.wanted.DateDue(time.Now(), wantedDates)
	if !ok {
		return
	}
	a, ok := r.availability(ctx, e.WorkID, e.Season, e.Episode)
	if !ok {
		r.wanted.Dated(e.ID, e.NotBefore, e.NotBeforeIs, time.Now()) // keep it, check tomorrow
		return
	}
	r.wanted.Dated(e.ID, a.Date, a.Kind, time.Now())
}

// checkWantedDownloads follows the downloads started for the list: done
// when they finished, wanted again when they were cancelled.
func (r *REST) checkWantedDownloads() {
	for _, e := range r.wanted.Downloading() {
		h, err := ed2k.ParseHash(e.Hash)
		if err != nil {
			r.wanted.Dropped(e.ID)
			continue
		}
		switch present, finished := r.m.Has(h); {
		case finished:
			r.wanted.Completed(e.Hash)
		case !present:
			r.logf("wish list: %s: its download was removed; looking for another file", e.Title)
			r.wanted.Dropped(e.ID)
		}
	}
}

// searchWantedEntry searches for an entry, and downloads the best file
// found if it is good enough.
func (r *REST) searchWantedEntry(ctx context.Context, e wanted.Entry) {
	ctx, cancel := context.WithTimeout(ctx, wantedLimit)
	defer cancel()
	found, err := r.findReleases(ctx, e.WorkID, e.Lang, e.Season, e.Episode)
	if err != nil {
		if errors.Is(err, download.ErrNoServer) || ctx.Err() != nil {
			return // not a search: try again later
		}
		r.logf("wish list: %s: %v", e.Title, err)
		r.wanted.Searched(e.ID, time.Now())
		return
	}
	pick, ok := pickWanted(found.releases, e)
	if !ok {
		r.wanted.Searched(e.ID, time.Now())
		return
	}
	link, err := ed2k.ParseLink(pick.Link)
	if err != nil {
		r.wanted.Searched(e.ID, time.Now())
		return
	}
	if _, err := r.addMedia(ctx, link, e.WorkID, e.Lang, e.Season, e.Episode); err != nil && !errors.Is(err, download.ErrExists) {
		r.logf("wish list: %s: cannot download %s: %v", e.Title, link.Name, err)
		r.wanted.Searched(e.ID, time.Now())
		return
	}
	r.logf("wish list: %s: found %s, downloading", e.Title, link.Name)
	r.wanted.Found(e.ID, link.Hash.String(), link.Name, time.Now())
}

// pickWanted is the file to download for e: the best one that the search
// preferences do not hide, has sources and was not cancelled before.
// Never a cinema copy (cam, telesync, screener), even if the preferences
// show them: nobody is there to look at it before it downloads.
func pickWanted(list []Release, e wanted.Entry) (Release, bool) {
	for _, x := range list {
		cinema := x.Info.Source == "cam" || x.Info.Source == "screener"
		if x.Hidden || cinema || x.Sources == 0 || slices.Contains(e.Skip, strings.ToUpper(x.Hash)) {
			continue
		}
		return x, true
	}
	return Release{}, false
}
