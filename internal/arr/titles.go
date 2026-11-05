package arr

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
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
