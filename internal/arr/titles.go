package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The *arr apps search by the original or English title, but files on
// eD2K are often named in the uploader's language. With search_languages
// set, MyMule asks Wikidata for the title in those languages, using the
// IMDb/TMDB/TVDB id the app sends, and searches for each title too.

// mediaIDs are the ids an app sends with a movie or TV search.
type mediaIDs struct {
	IMDb, TMDB, TVDB string
	TV               bool
}

func (m mediaIDs) empty() bool { return m.IMDb == "" && m.TMDB == "" && m.TVDB == "" }

func (m mediaIDs) key() string {
	return fmt.Sprintf("%s|%s|%s|%t", m.IMDb, m.TMDB, m.TVDB, m.TV)
}

// idsFromQuery reads imdbid ("tt0063350" or "63350"), tmdbid and tvdbid.
func idsFromQuery(q url.Values, tv bool) mediaIDs {
	digits := func(s string) string {
		s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "tt")
		for _, r := range s {
			if r < '0' || r > '9' {
				return ""
			}
		}
		if strings.Trim(s, "0") == "" {
			return ""
		}
		return s
	}
	ids := mediaIDs{TMDB: digits(q.Get("tmdbid")), TVDB: digits(q.Get("tvdbid")), TV: tv}
	if d := digits(q.Get("imdbid")); d != "" {
		if len(d) < 7 {
			d = strings.Repeat("0", 7-len(d)) + d
		}
		ids.IMDb = "tt" + d
	}
	return ids
}

// titleLookup finds titles for a movie or show in the given languages.
type titleLookup func(ctx context.Context, ids mediaIDs, langs []string) ([]string, error)

// titleCache remembers lookups; titles don't change.
type titleCache struct {
	mu   sync.Mutex
	data map[string][]string
}

func (c *titleCache) get(k string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[k]
	return v, ok
}

func (c *titleCache) put(k string, v []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = map[string][]string{}
	}
	if len(c.data) > 5000 {
		c.data = map[string][]string{}
	}
	c.data[k] = v
}

const maxTitles = 4 // searches per request, each takes several seconds

// searchTitles returns the eD2K searches to run: the app's own query and
// the titles in the configured languages (plus English when the app sent
// only ids), without duplicates.
func (a *Adapter) searchTitles(ctx context.Context, text string, ids mediaIDs) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		t = searchText(t)
		if k := strings.ToLower(t); t != "" && !seen[k] && len(out) < maxTitles {
			seen[k] = true
			out = append(out, t)
		}
	}
	add(text)
	if len(a.cfg.Languages) == 0 || ids.empty() || a.lookup == nil {
		return out
	}
	langs := a.cfg.Languages
	if text == "" {
		langs = append([]string{"en"}, langs...)
	}
	key := ids.key() + "|" + strings.Join(langs, ",")
	titles, ok := a.cache.get(key)
	if !ok {
		var err error
		titles, err = a.lookup(ctx, ids, langs)
		if err != nil {
			a.cfg.Log("title lookup for %s failed: %v", ids.key(), err)
		} else {
			a.cache.put(key, titles)
		}
	}
	for _, t := range titles {
		add(t)
	}
	return out
}

// searchText turns a title into eD2K search words: punctuation such as
// ':' or '-' becomes a space (a leading '-' would exclude a word).
func searchText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '\'' {
			return r
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// ---- Wikidata ----

// Wikidata properties holding each kind of id.
const (
	propIMDb     = "P345"
	propTMDBFilm = "P4947"
	propTMDBTV   = "P4983"
	propTVDB     = "P4835"
)

type wikidata struct {
	client    *http.Client
	userAgent string
	endpoint  string // https://www.wikidata.org/w/api.php
}

func newWikidata(userAgent string) *wikidata {
	return &wikidata{client: &http.Client{Timeout: 10 * time.Second}, userAgent: userAgent,
		endpoint: "https://www.wikidata.org/w/api.php"}
}

func (w *wikidata) get(ctx context.Context, params url.Values, out any) error {
	params.Set("format", "json")
	req, err := http.NewRequestWithContext(ctx, "GET", w.endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", w.userAgent)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wikidata: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// entity finds the Wikidata item with the given id statement.
func (w *wikidata) entity(ctx context.Context, prop, value string) (string, error) {
	var r struct {
		Query struct {
			Search []struct {
				Title string `json:"title"`
			} `json:"search"`
		} `json:"query"`
	}
	err := w.get(ctx, url.Values{"action": {"query"}, "list": {"search"}, "srlimit": {"1"},
		"srsearch": {"haswbstatement:" + prop + "=" + value}}, &r)
	if err != nil || len(r.Query.Search) == 0 {
		return "", err
	}
	return r.Query.Search[0].Title, nil
}

// Titles returns the labels (and a couple of aliases) of the item in each
// language, in the order of langs.
func (w *wikidata) Titles(ctx context.Context, ids mediaIDs, langs []string) ([]string, error) {
	tmdbProp := propTMDBFilm
	if ids.TV {
		tmdbProp = propTMDBTV
	}
	var q string
	for _, c := range []struct{ prop, val string }{{propIMDb, ids.IMDb}, {tmdbProp, ids.TMDB}, {propTVDB, ids.TVDB}} {
		if c.val == "" {
			continue
		}
		var err error
		if q, err = w.entity(ctx, c.prop, c.val); err != nil {
			return nil, err
		}
		if q != "" {
			break
		}
	}
	if q == "" {
		return nil, nil // not on Wikidata: nothing to add
	}
	var r struct {
		Entities map[string]struct {
			Labels  map[string]struct{ Value string }   `json:"labels"`
			Aliases map[string][]struct{ Value string } `json:"aliases"`
		} `json:"entities"`
	}
	err := w.get(ctx, url.Values{"action": {"wbgetentities"}, "ids": {q},
		"props": {"labels|aliases"}, "languages": {strings.Join(langs, "|")}}, &r)
	if err != nil {
		return nil, err
	}
	e := r.Entities[q]
	var out []string
	for _, l := range langs {
		if v := e.Labels[l].Value; v != "" {
			out = append(out, v)
		}
	}
	for _, l := range langs {
		for i, a := range e.Aliases[l] {
			if i == 2 {
				break
			}
			out = append(out, a.Value)
		}
	}
	return out, nil
}
