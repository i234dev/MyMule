package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProjectTMDBKey is MyMule's own TMDB API key (a v3 "API key", meant to be
// embedded in apps, as Jellyfin and Kodi do). Users can set their own with
// tmdb_api_key in mymule.conf, or turn TMDB off.
const ProjectTMDBKey = "88b98470185a4c4f6126eaa7acd191ad"

// TMDBAttribution must be shown wherever TMDB data is used.
const TMDBAttribution = "This product uses the TMDB API but is not endorsed or certified by TMDB."

// TMDB is a client for The Movie Database: titles per country, posters,
// seasons. https://developer.themoviedb.org
type TMDB struct {
	key    string
	client *http.Client
	API    string // https://api.themoviedb.org/3

	mu    sync.Mutex
	cache map[string]cached
}

// NewTMDB creates a client with an API key.
func NewTMDB(key string) *TMDB {
	return &TMDB{key: key, client: &http.Client{Timeout: 15 * time.Second},
		API: "https://api.themoviedb.org/3", cache: map[string]cached{}}
}

func (t *TMDB) get(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("api_key", t.key)
	req, err := http.NewRequestWithContext(ctx, "GET", t.API+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		// Don't let the URL (with the key) end up in logs or replies.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("tmdb: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tmdb: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *TMDB) cached(key string) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.cache[key]
	if !ok || time.Since(c.at) > cacheTTL {
		return nil, false
	}
	return c.val, true
}

func (t *TMDB) store(key string, v any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.cache) > 2000 {
		t.cache = map[string]cached{}
	}
	t.cache[key] = cached{v, time.Now()}
}

// tmdbLang turns "es", "es-ES" or "pt-br" into TMDB's "es-ES" form.
func tmdbLang(lang string) string {
	l, region, _ := strings.Cut(lang, "-")
	l = strings.ToLower(l)
	if !langRe(l) {
		return "en-US"
	}
	if region == "" {
		return l
	}
	return l + "-" + strings.ToUpper(region)
}

type tmdbResult struct {
	MediaType     string  `json:"media_type"`
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	Popularity    float64 `json:"popularity"`
}

func (r tmdbResult) work(kind string) Work {
	w := Work{Kind: kind, Description: shorten(r.Overview, 400), Poster: r.PosterPath,
		IDs: IDs{TMDB: strconv.Itoa(r.ID)}, Popularity: int(r.Popularity)}
	date := r.ReleaseDate
	w.Title, w.OriginalTitle = r.Title, r.OriginalTitle
	if kind == Series {
		w.Title, w.OriginalTitle, date = r.Name, r.OriginalName, r.FirstAirDate
		w.ID = "tmdb-tv-" + strconv.Itoa(r.ID)
	} else {
		w.ID = "tmdb-movie-" + strconv.Itoa(r.ID)
	}
	if len(date) >= 4 {
		w.Year, _ = strconv.Atoi(date[:4])
	}
	if w.Title == "" {
		w.Title = w.OriginalTitle
	}
	return w
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n])) + "…"
	}
	return s
}

// Search finds movies and series matching text, in the given language.
func (t *TMDB) Search(ctx context.Context, text, lang string) ([]Work, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	lang = tmdbLang(lang)
	key := "search|" + lang + "|" + strings.ToLower(text)
	if v, ok := t.cached(key); ok {
		return v.([]Work), nil
	}
	var r struct {
		Results []tmdbResult `json:"results"`
	}
	if err := t.get(ctx, "/search/multi", url.Values{"query": {text}, "language": {lang},
		"include_adult": {"false"}}, &r); err != nil {
		return nil, err
	}
	works := []Work{}
	for _, x := range r.Results {
		switch x.MediaType {
		case "movie":
			works = append(works, x.work(Movie))
		case "tv":
			works = append(works, x.work(Series))
		}
		if len(works) == 20 {
			break
		}
	}
	t.store(key, works)
	return works, nil
}

// ParseTMDBID splits "tmdb-movie-123" / "tmdb-tv-123".
func ParseTMDBID(id string) (kind string, n int, ok bool) {
	rest, found := strings.CutPrefix(id, "tmdb-")
	if !found {
		return "", 0, false
	}
	k, num, _ := strings.Cut(rest, "-")
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	switch k {
	case "movie":
		return Movie, n, true
	case "tv":
		return Series, n, true
	}
	return "", 0, false
}

// Get returns a work with its alternative titles, ids and seasons.
func (t *TMDB) Get(ctx context.Context, id, lang string) (Work, error) {
	kind, n, ok := ParseTMDBID(id)
	if !ok {
		return Work{}, fmt.Errorf("not a TMDB id: %q", id)
	}
	lang = tmdbLang(lang)
	key := "get|" + lang + "|" + id
	if v, ok := t.cached(key); ok {
		return v.(Work), nil
	}
	path := "/movie/"
	if kind == Series {
		path = "/tv/"
	}
	var r struct {
		tmdbResult
		ExternalIDs struct {
			IMDb string `json:"imdb_id"`
			TVDB int    `json:"tvdb_id"`
		} `json:"external_ids"`
		AlternativeTitles struct {
			Titles  []tmdbAltTitle `json:"titles"`  // movies
			Results []tmdbAltTitle `json:"results"` // series
		} `json:"alternative_titles"`
		Translations struct {
			Translations []struct {
				Lang    string `json:"iso_639_1"`
				Country string `json:"iso_3166_1"`
				Data    struct {
					Title string `json:"title"`
					Name  string `json:"name"`
				} `json:"data"`
			} `json:"translations"`
		} `json:"translations"`
		Seasons []struct {
			Number   int    `json:"season_number"`
			Episodes int    `json:"episode_count"`
			Name     string `json:"name"`
		} `json:"seasons"`
	}
	if err := t.get(ctx, path+strconv.Itoa(n), url.Values{"language": {lang},
		"append_to_response": {"external_ids,alternative_titles,translations"}}, &r); err != nil {
		return Work{}, err
	}
	r.ID = n
	w := r.work(kind)
	w.IDs.IMDb = r.ExternalIDs.IMDb
	if r.ExternalIDs.TVDB > 0 {
		w.IDs.TVDB = strconv.Itoa(r.ExternalIDs.TVDB)
	}
	// Other titles in the same language (other countries' translations,
	// e.g. es-MX next to es-ES) and the alternative titles of the country.
	wantLang, wantCountry, _ := strings.Cut(lang, "-")
	for _, tr := range r.Translations.Translations {
		if tr.Lang == wantLang {
			w.Aliases = appendTitle(w.Aliases, tr.Data.Title+tr.Data.Name)
		}
	}
	for _, a := range append(r.AlternativeTitles.Titles, r.AlternativeTitles.Results...) {
		if wantCountry != "" && a.Country == wantCountry {
			w.Aliases = appendTitle(w.Aliases, a.Title)
		}
	}
	w.Aliases = removeTitle(w.Aliases, w.Title)
	for _, s := range r.Seasons {
		if s.Number > 0 {
			w.Seasons = append(w.Seasons, Season{Number: s.Number, Episodes: s.Episodes, Name: s.Name})
		}
	}
	t.store(key, w)
	return w, nil
}

type tmdbAltTitle struct {
	Country string `json:"iso_3166_1"`
	Title   string `json:"title"`
}

func appendTitle(list []string, t string) []string {
	t = strings.TrimSpace(t)
	if t == "" {
		return list
	}
	for _, x := range list {
		if strings.EqualFold(x, t) {
			return list
		}
	}
	return append(list, t)
}

func removeTitle(list []string, t string) []string {
	var out []string
	for _, x := range list {
		if !strings.EqualFold(x, t) {
			out = append(out, x)
		}
	}
	return out
}
