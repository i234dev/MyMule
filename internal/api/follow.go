package api

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/meta"
	"mymule/internal/wanted"
)

// Followed series (see wanted/follow.go): the list of their episodes comes
// from the movie database, and a season is searched for as a whole, the
// best file of each episode it still needs taken.

// pick is the file chosen for an episode.
type pick struct {
	ep   wanted.Episode
	link ed2k.Link
}

// followLimit is how long the search for one season may take (its files'
// ratings are asked too).
var followLimit = 5 * time.Minute

// listEpisodes asks the movie database for the episodes of a followed
// series, from its first season on.
func (r *REST) listEpisodes(ctx context.Context, e wanted.Entry) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	sl, ok := r.media.(meta.SeasonLister)
	work, err := r.media.Get(ctx, e.WorkID, e.Lang)
	if !ok || err != nil {
		r.wanted.SetEpisodes(e.ID, nil, time.Now()) // try again tomorrow
		return
	}
	var list []wanted.Episode
	for _, season := range work.Seasons {
		if season.Number < e.FromSeason || (e.ToSeason > 0 && season.Number > e.ToSeason) {
			continue
		}
		eps, err := sl.SeasonEpisodes(ctx, e.WorkID, season.Number, e.Lang)
		if err != nil {
			continue
		}
		for _, ep := range eps {
			x := wanted.Episode{Season: season.Number, Episode: ep.Number, Name: ep.Name}
			if at, err := time.Parse(time.DateOnly, ep.AirDate); err == nil {
				x.Airs = at
			}
			list = append(list, x)
		}
	}
	r.wanted.SetEpisodes(e.ID, list, time.Now())
}

// searchFollowSeason searches for a season of a followed series, and
// downloads the best file of each aired episode it still needs. Only when
// some have no file of their own, a file with the whole season is taken
// for them (it is only offered once every episode has aired).
func (r *REST) searchFollowSeason(ctx context.Context, e wanted.Entry, season int) {
	ctx, cancel := context.WithTimeout(ctx, followLimit)
	defer cancel()
	found, err := r.findReleases(ctx, e.WorkID, e.Lang, season, 0)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("wish list: %s season %d: %v", e.Title, season, err)
			r.wanted.SeasonSearched(e.ID, season, time.Now())
		}
		return
	}
	now := time.Now()
	var (
		mu    sync.Mutex
		picks []pick
		wg    sync.WaitGroup
	)
	for _, ep := range e.Episodes {
		if ep.Season != season || ep.Status != wanted.Wanted || !ep.Aired(now) {
			continue
		}
		var files []Release
		for _, x := range found.releases {
			if x.Info.Episode == ep.Episode && (x.Info.Season == 0 || x.Info.Season == season) {
				files = append(files, x)
			}
		}
		wg.Add(1)
		go func() { // the ratings are asked of several files at once
			defer wg.Done()
			label := wanted.Entry{Title: fmt.Sprintf("%s S%02dE%02d", e.Title, ep.Season, ep.Episode), Skip: ep.Skip}
			if link, ok := r.chooseWanted(ctx, files, label); ok {
				mu.Lock()
				picks = append(picks, pick{ep, link})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	pack, havePack := r.seasonPack(ctx, e, season, found.releases, picks)
	if havePack {
		for _, ep := range e.Episodes {
			if ep.Season != season || ep.Status != wanted.Wanted || !ep.Aired(now) ||
				slices.ContainsFunc(picks, func(p pick) bool { return p.ep.Episode == ep.Episode }) {
				continue
			}
			picks = append(picks, pick{ep, pack})
		}
	}
	added := map[ed2k.Hash]bool{} // downloads started: true if it worked
	for _, p := range picks {
		if ok, seen := added[p.link.Hash]; seen { // the season file, for another episode
			if ok {
				r.wanted.EpisodeFound(e.ID, p.ep.Season, p.ep.Episode, p.link.Hash.String(), p.link.Name)
			}
			continue
		}
		episode := p.ep.Episode
		if havePack && p.link.Hash == pack.Hash {
			episode = 0 // organised as the whole season
		}
		_, err := r.addMedia(ctx, p.link, e.WorkID, e.Lang, p.ep.Season, episode)
		added[p.link.Hash] = err == nil
		if err != nil {
			r.logf("wish list: %s S%02dE%02d: cannot download %s: %v", e.Title, p.ep.Season, p.ep.Episode, p.link.Name, err)
			continue
		}
		r.wanted.EpisodeFound(e.ID, p.ep.Season, p.ep.Episode, p.link.Hash.String(), p.link.Name)
		r.logf("wish list: %s S%02dE%02d: found %s, downloading", e.Title, p.ep.Season, p.ep.Episode, p.link.Name)
		if r.OnWantedFound != nil {
			r.OnWantedFound(fmt.Sprintf("%s S%02dE%02d", e.Title, p.ep.Season, p.ep.Episode), p.link.Name)
		}
	}
	r.wanted.SeasonSearched(e.ID, season, time.Now())
}

// seasonPack is a file with the whole season, for the aired episodes that
// found no file of their own; ok is false when they all did, or no such
// file is good enough.
func (r *REST) seasonPack(ctx context.Context, e wanted.Entry, season int, files []Release, picks []pick) (ed2k.Link, bool) {
	now := time.Now()
	missing := false
	var skip []string
	for _, ep := range e.Episodes {
		if ep.Season != season || ep.Status != wanted.Wanted || !ep.Aired(now) {
			continue
		}
		skip = append(skip, ep.Skip...)
		if !slices.ContainsFunc(picks, func(p pick) bool { return p.ep.Episode == ep.Episode }) {
			missing = true
		}
	}
	if !missing {
		return ed2k.Link{}, false
	}
	var packs []Release
	for _, x := range files {
		if x.Info.Episode == 0 && (x.Info.Season == 0 || x.Info.Season == season) {
			packs = append(packs, x)
		}
	}
	label := wanted.Entry{Title: fmt.Sprintf("%s season %d", e.Title, season), Skip: skip}
	return r.chooseWanted(ctx, packs, label)
}
