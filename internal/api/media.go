package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"mymule/internal/ed2k"
	"mymule/internal/meta"
	"mymule/internal/release"
	"mymule/internal/server"
)

// Movies and series: look a work up (Wikidata), then search eD2K by all
// its titles and return the files that belong to it, scored. Used by the
// web interface; only enabled together with it.

// EnableMedia adds the movie/series endpoints, using src for lookups.
func (r *REST) EnableMedia(src meta.Source) {
	r.media = src
	r.mux.HandleFunc("GET /api/v1/media/search", r.mediaSearch)
	r.mux.HandleFunc("GET /api/v1/media/{id}/releases", r.mediaReleases)
	r.mux.HandleFunc("GET /api/v1/media/{id}", r.mediaGet)
	r.mux.HandleFunc("GET /api/v1/media/{id}/season/{n}", r.mediaSeason)
}

// mediaGet returns a work with its details (seasons of a series).
func (r *REST) mediaGet(w http.ResponseWriter, req *http.Request) {
	work, err := r.media.Get(req.Context(), req.PathValue("id"), reqLang(req))
	if err != nil {
		fail(w, http.StatusBadGateway, errors.New("movie database: "+err.Error()))
		return
	}
	reply(w, map[string]any{"work": work})
}

// mediaSeason lists the episodes of a season ([] when unknown).
func (r *REST) mediaSeason(w http.ResponseWriter, req *http.Request) {
	n, err := strconv.Atoi(req.PathValue("n"))
	if err != nil || n < 0 {
		fail(w, http.StatusBadRequest, errors.New("season must be a number"))
		return
	}
	eps := []meta.Episode{}
	if sl, ok := r.media.(meta.SeasonLister); ok {
		list, err := sl.SeasonEpisodes(req.Context(), req.PathValue("id"), n, reqLang(req))
		if err != nil {
			fail(w, http.StatusBadGateway, errors.New("movie database: "+err.Error()))
			return
		}
		if list != nil {
			eps = list
		}
	}
	reply(w, map[string]any{"episodes": eps})
}

func reqLang(req *http.Request) string {
	l := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("lang")))
	if l == "" {
		return "en"
	}
	return l
}

func (r *REST) mediaSearch(w http.ResponseWriter, req *http.Request) {
	q := strings.TrimSpace(req.URL.Query().Get("q"))
	if q == "" {
		fail(w, http.StatusBadRequest, errors.New("missing q"))
		return
	}
	works, err := r.media.Search(req.Context(), q, reqLang(req))
	if err != nil {
		fail(w, http.StatusBadGateway, errors.New("movie database: "+err.Error()))
		return
	}
	if works == nil {
		works = []meta.Work{}
	}
	reply(w, map[string]any{"works": works})
}

// Release is a file found for a work.
type Release struct {
	Hash string `json:"hash"`
	Link string `json:"link"`
	release.Candidate
	Recommended bool `json:"recommended,omitempty"`
}

const maxTitleSearches = 4

func (r *REST) mediaReleases(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	season, _ := strconv.Atoi(q.Get("season"))
	episode, _ := strconv.Atoi(q.Get("episode"))
	found, err := r.findReleases(req.Context(), req.PathValue("id"), reqLang(req), season, episode)
	if err != nil {
		var me mediaError
		if errors.As(err, &me) {
			fail(w, http.StatusBadGateway, err)
		} else {
			fail(w, errCode(err), err)
		}
		return
	}
	out := map[string]any{"work": found.work, "queries": found.queries, "releases": found.releases}
	// Not out at home yet: the web interface offers the wish list.
	if a, ok := r.availability(req.Context(), found.work.ID, season, episode); ok && a.Date.After(time.Now()) {
		out["available"] = a
	}
	reply(w, out)
}

// mediaError is a failure of the movie database (not of eD2K).
type mediaError struct{ err error }

func (e mediaError) Error() string { return "movie database: " + e.err.Error() }
func (e mediaError) Unwrap() error { return e.err }

type foundReleases struct {
	work     meta.Work
	queries  []string
	releases []Release // best first
}

// findReleases looks a work up and searches eD2K for its files (an
// episode, a season or any of a series when season/episode are 0),
// scored with the user's search preferences.
func (r *REST) findReleases(ctx context.Context, id, fullLang string, season, episode int) (foundReleases, error) {
	lang, _, _ := strings.Cut(fullLang, "-") // e.g. es-ES: TMDB has titles per country
	work, err := r.media.Get(ctx, id, fullLang)
	if err != nil {
		return foundReleases{}, mediaError{err}
	}
	target := release.Target{Year: work.Year, Series: work.Kind == meta.Series,
		TitleLanguage: map[string]string{}, Season: season, Episode: episode}
	target.Titles = work.Titles()
	// Titles in the requested language suggest the file is in it.
	if !strings.EqualFold(work.Title, work.OriginalTitle) {
		target.TitleLanguage[work.Title] = lang
	}
	for _, a := range work.Aliases {
		if !strings.EqualFold(a, work.OriginalTitle) {
			target.TitleLanguage[a] = lang
		}
	}
	prefs := release.Prefs{Languages: []string{lang}, Subs: true, Resolution: "1080p", NoCam: true}
	if r.prefs != nil {
		sp := r.prefs.Get().Search
		if len(sp.Languages) > 0 {
			prefs.Languages = sp.Languages
		}
		prefs.Subs, prefs.Resolution, prefs.NoCam = sp.Subs, sp.Resolution, sp.HideCam
		prefs.MaxSize = int64(sp.MaxSizeGB * (1 << 30))
	}

	// One eD2K search per title, at the same time. Results from the
	// different searches may get mixed, which doesn't matter: every
	// result is checked against the work's titles afterwards.
	var queries []string
	seen := map[string]bool{}
	for _, t := range target.Titles {
		k := searchWords(t)
		if k != "" && !seen[strings.ToLower(k)] && len(queries) < maxTitleSearches {
			seen[strings.ToLower(k)] = true
			queries = append(queries, k)
		}
	}
	type found struct {
		res []SearchResult
		err error
	}
	out := make([]found, len(queries))
	var wg sync.WaitGroup
	for i, kw := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := server.Query{Keywords: kw, Type: server.TypeVideo}
			if r.testSearch != nil {
				out[i].res, out[i].err = r.testSearch(ctx, q)
			} else {
				out[i].res, out[i].err = r.Search(ctx, q, true)
			}
		}()
	}
	wg.Wait()

	byHash := map[ed2k.Hash]*SearchResult{}
	var order []ed2k.Hash
	var firstErr error
	for _, f := range out {
		if f.err != nil {
			firstErr = f.err
			continue
		}
		for _, x := range f.res {
			if have, ok := byHash[x.Hash]; ok {
				have.Sources = max(have.Sources, x.Sources)
				have.Complete = max(have.Complete, x.Complete)
				continue
			}
			cp := x
			byHash[x.Hash] = &cp
			order = append(order, x.Hash)
		}
	}
	if len(order) == 0 && firstErr != nil {
		return foundReleases{}, firstErr
	}

	var cands []release.Candidate
	links := map[string]SearchResult{}
	for _, h := range order {
		x := byHash[h]
		c := release.Candidate{Name: x.Name, Size: int64(x.Size), Sources: int(x.Sources), Complete: int(x.Complete)}
		if release.Evaluate(&c, target, prefs) {
			cands = append(cands, c)
			links[x.Name+"|"+strconv.FormatUint(x.Size, 10)] = *x
		}
	}
	release.Rank(cands)
	list := []Release{}
	for i, c := range cands {
		x := links[c.Name+"|"+strconv.FormatInt(c.Size, 10)]
		list = append(list, Release{Hash: x.Hash.String(), Link: x.Link, Candidate: c,
			Recommended: i == 0 && !c.Hidden})
	}
	return foundReleases{work: work, queries: queries, releases: list}, nil
}

// searchWords turns a title into eD2K search words: punctuation becomes a
// space (a leading '-' would exclude a word).
func searchWords(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '\'' {
			return r
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
