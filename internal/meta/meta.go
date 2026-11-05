// Package meta looks up movies and TV series (titles in several languages,
// year, ids) on Wikidata, so searches can use every title a work is known
// by. Only search words and ids are sent; nothing about the user.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind of work.
const (
	Movie  = "movie"
	Series = "series"
)

// IDs identify a work in other databases.
type IDs struct {
	IMDb string `json:"imdb,omitempty"` // tt0084787
	TMDB string `json:"tmdb,omitempty"`
	TVDB string `json:"tvdb,omitempty"`
}

// Empty reports whether no id is set.
func (i IDs) Empty() bool { return i.IMDb == "" && i.TMDB == "" && i.TVDB == "" }

// Work is a movie or a series.
type Work struct {
	ID            string   `json:"id"` // Wikidata item, e.g. Q210756
	Kind          string   `json:"kind"`
	Title         string   `json:"title"`          // in the requested language when known
	OriginalTitle string   `json:"original_title"` // as released
	Year          int      `json:"year,omitempty"`
	Description   string   `json:"description,omitempty"`
	Aliases       []string `json:"aliases,omitempty"` // other titles in the requested language
	IDs           IDs      `json:"ids"`
	Popularity    int      `json:"-"` // number of Wikipedia articles
}

// Titles returns every distinct title of the work, the display title first.
func (w Work) Titles() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range append([]string{w.Title, w.OriginalTitle}, w.Aliases...) {
		if k := strings.ToLower(strings.TrimSpace(t)); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, strings.TrimSpace(t))
		}
	}
	return out
}

// Wikidata properties and classes.
const (
	propIMDb     = "P345"
	propTMDBFilm = "P4947"
	propTMDBTV   = "P4983"
	propTVDB     = "P4835"
)

// Classes ("instance of") that count as movies or series.
var (
	movieClasses  = []string{"Q11424", "Q202866", "Q506240", "Q24869", "Q226730", "Q29168811", "Q93204"} // film, animated film, TV film, feature film, silent film, animated feature, documentary
	seriesClasses = []string{"Q5398426", "Q581714", "Q117467246", "Q1259759", "Q526877", "Q63952888"}    // TV series, animated series (old and new class), miniseries, web series, anime
)

// Wikidata is a small client for the Wikidata APIs.
type Wikidata struct {
	client    *http.Client
	userAgent string
	API       string // https://www.wikidata.org/w/api.php
	SPARQL    string // https://query.wikidata.org/sparql

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	val any
	at  time.Time
}

const cacheTTL = 24 * time.Hour

// NewWikidata creates a client. userAgent should name the program and a
// URL, as Wikimedia asks.
func NewWikidata(userAgent string) *Wikidata {
	return &Wikidata{client: &http.Client{Timeout: 15 * time.Second}, userAgent: userAgent,
		API: "https://www.wikidata.org/w/api.php", SPARQL: "https://query.wikidata.org/sparql",
		cache: map[string]cached{}}
}

func (w *Wikidata) cached(key string) (any, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.cache[key]
	if !ok || time.Since(c.at) > cacheTTL {
		return nil, false
	}
	return c.val, true
}

func (w *Wikidata) store(key string, v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.cache) > 2000 {
		w.cache = map[string]cached{}
	}
	w.cache[key] = cached{v, time.Now()}
}

func (w *Wikidata) get(ctx context.Context, endpoint string, params url.Values, accept string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", w.userAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
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

// searchItems runs a text search restricted to statements, returning
// item ids by relevance.
func (w *Wikidata) searchItems(ctx context.Context, query string, limit int) ([]string, error) {
	var r struct {
		Query struct {
			Search []struct {
				Title string `json:"title"`
			} `json:"search"`
		} `json:"query"`
	}
	err := w.get(ctx, w.API, url.Values{"action": {"query"}, "list": {"search"}, "format": {"json"},
		"srlimit": {strconv.Itoa(limit)}, "srsearch": {query}}, "", &r)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range r.Query.Search {
		ids = append(ids, s.Title)
	}
	return ids, nil
}

// Search finds movies and series matching text, with titles and
// descriptions in lang (e.g. "es"), most relevant first.
func (w *Wikidata) Search(ctx context.Context, text, lang string) ([]Work, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	key := "search|" + lang + "|" + strings.ToLower(text)
	if v, ok := w.cached(key); ok {
		return v.([]Work), nil
	}
	var classes []string
	for _, c := range append(append([]string{}, movieClasses...), seriesClasses...) {
		classes = append(classes, "P31="+c)
	}
	// Quotes and other search syntax in the user's text are neutralised.
	clean := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`"*?~:\[](){}|`, r) {
			return ' '
		}
		return r
	}, text)
	ids, err := w.searchItems(ctx, clean+" haswbstatement:"+strings.Join(classes, "|"), 20)
	if err != nil {
		return nil, err
	}
	works, err := w.details(ctx, ids, lang)
	if err != nil {
		return nil, err
	}
	// Keep the search engine's order, but let well-known works (with many
	// Wikipedia articles) come first among the top results.
	rank := map[string]int{}
	for i, id := range ids {
		rank[id] = i
	}
	sort.SliceStable(works, func(i, j int) bool {
		si := works[i].Popularity*2 - rank[works[i].ID]*3
		sj := works[j].Popularity*2 - rank[works[j].ID]*3
		return si > sj
	})
	w.store(key, works)
	return works, nil
}

// Get returns one work by Wikidata id.
func (w *Wikidata) Get(ctx context.Context, id, lang string) (Work, error) {
	if !validQID(id) {
		return Work{}, fmt.Errorf("not a Wikidata id: %q", id)
	}
	key := "get|" + lang + "|" + id
	if v, ok := w.cached(key); ok {
		return v.(Work), nil
	}
	works, err := w.details(ctx, []string{id}, lang)
	if err != nil {
		return Work{}, err
	}
	if len(works) == 0 {
		return Work{}, fmt.Errorf("%s is not a movie or series", id)
	}
	w.store(key, works[0])
	return works[0], nil
}

func validQID(id string) bool {
	if len(id) < 2 || len(id) > 12 || id[0] != 'Q' {
		return false
	}
	for _, r := range id[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var langRe = func(l string) bool {
	if len(l) < 2 || len(l) > 10 {
		return false
	}
	for _, r := range l {
		if !(r >= 'a' && r <= 'z' || r == '-') {
			return false
		}
	}
	return true
}

// details fetches the data of several items with one SPARQL query.
func (w *Wikidata) details(ctx context.Context, ids []string, lang string) ([]Work, error) {
	var values []string
	for _, id := range ids {
		if validQID(id) {
			values = append(values, "wd:"+id)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	if !langRe(lang) {
		lang = "en"
	}
	q := `SELECT ?item
 (GROUP_CONCAT(DISTINCT STRAFTER(STR(?type), "entity/"); separator=" ") AS ?types)
 (MIN(?y) AS ?year) (SAMPLE(?imdb) AS ?imdbv) (SAMPLE(?tmdbm) AS ?tmdbmv) (SAMPLE(?tmdbt) AS ?tmdbtv)
 (SAMPLE(?tvdb) AS ?tvdbv) (SAMPLE(?links) AS ?linksv) (SAMPLE(?orig) AS ?origv) (SAMPLE(?label) AS ?labelv)
 (SAMPLE(?en) AS ?env) (SAMPLE(?desc) AS ?descv) (GROUP_CONCAT(DISTINCT ?alt; separator="\u001f") AS ?alts)
WHERE {
 VALUES ?item { ` + strings.Join(values, " ") + ` }
 ?item wdt:P31 ?type .
 OPTIONAL { ?item wdt:P577 ?d . BIND(YEAR(?d) AS ?y) }
 OPTIONAL { ?item wdt:P580 ?s . BIND(YEAR(?s) AS ?y) }
 OPTIONAL { ?item wdt:` + propIMDb + ` ?imdb }
 OPTIONAL { ?item wdt:` + propTMDBFilm + ` ?tmdbm }
 OPTIONAL { ?item wdt:` + propTMDBTV + ` ?tmdbt }
 OPTIONAL { ?item wdt:` + propTVDB + ` ?tvdb }
 OPTIONAL { ?item wikibase:sitelinks ?links }
 OPTIONAL { ?item wdt:P1476 ?orig }
 OPTIONAL { ?item rdfs:label ?label FILTER(LANG(?label) = "` + lang + `") }
 OPTIONAL { ?item rdfs:label ?en FILTER(LANG(?en) = "en") }
 OPTIONAL { ?item schema:description ?desc FILTER(LANG(?desc) = "` + lang + `") }
 OPTIONAL { ?item skos:altLabel ?alt FILTER(LANG(?alt) = "` + lang + `") }
} GROUP BY ?item`
	var r struct {
		Results struct {
			Bindings []map[string]struct {
				Value string `json:"value"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if err := w.get(ctx, w.SPARQL, url.Values{"query": {q}, "format": {"json"}},
		"application/sparql-results+json", &r); err != nil {
		return nil, err
	}
	var out []Work
	for _, b := range r.Results.Bindings {
		v := func(k string) string { return b[k].Value }
		kind := kindOf(strings.Fields(v("types")))
		if kind == "" {
			continue
		}
		wk := Work{
			ID: strings.TrimPrefix(v("item"), "http://www.wikidata.org/entity/"), Kind: kind,
			OriginalTitle: v("origv"), Description: v("descv"),
			IDs: IDs{IMDb: v("imdbv"), TVDB: v("tvdbv")},
		}
		wk.Year, _ = strconv.Atoi(v("year"))
		wk.Popularity, _ = strconv.Atoi(v("linksv"))
		if kind == Series {
			wk.IDs.TMDB = v("tmdbtv")
		} else {
			wk.IDs.TMDB = v("tmdbmv")
		}
		if a := v("alts"); a != "" {
			wk.Aliases = strings.Split(a, "\u001f")
		}
		// Display title: the label in the language; otherwise the first
		// alias in it; then the original title; then English.
		switch {
		case v("labelv") != "":
			wk.Title = v("labelv")
		case len(wk.Aliases) > 0:
			wk.Title = wk.Aliases[0]
		case wk.OriginalTitle != "":
			wk.Title = wk.OriginalTitle
		default:
			wk.Title = v("env")
		}
		if wk.OriginalTitle == "" {
			wk.OriginalTitle = v("env")
		}
		if wk.Title == "" {
			continue
		}
		out = append(out, wk)
	}
	return out, nil
}

func kindOf(types []string) string {
	has := func(list []string) bool {
		for _, t := range types {
			for _, c := range list {
				if t == c {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(seriesClasses):
		return Series
	case has(movieClasses):
		return Movie
	}
	return ""
}

// ---- titles by id (used by the *arr indexer) ----

// entityByStatement finds the item with the given id statement.
func (w *Wikidata) entityByStatement(ctx context.Context, prop, value string) (string, error) {
	ids, err := w.searchItems(ctx, "haswbstatement:"+prop+"="+value, 1)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

// TitlesByID returns the labels (and a couple of aliases) of the work with
// the given ids in each language, in the order of langs.
func (w *Wikidata) TitlesByID(ctx context.Context, ids IDs, tv bool, langs []string) ([]string, error) {
	tmdbProp := propTMDBFilm
	if tv {
		tmdbProp = propTMDBTV
	}
	var q string
	for _, c := range []struct{ prop, val string }{{propIMDb, ids.IMDb}, {tmdbProp, ids.TMDB}, {propTVDB, ids.TVDB}} {
		if c.val == "" {
			continue
		}
		var err error
		if q, err = w.entityByStatement(ctx, c.prop, c.val); err != nil {
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
	err := w.get(ctx, w.API, url.Values{"action": {"wbgetentities"}, "ids": {q}, "format": {"json"},
		"props": {"labels|aliases"}, "languages": {strings.Join(langs, "|")}}, "", &r)
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
