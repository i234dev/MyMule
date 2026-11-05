package arr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestIDsFromQuery(t *testing.T) {
	got := idsFromQuery(url.Values{"imdbid": {"63350"}, "tmdbid": {"10331"}}, false)
	if got.IMDb != "tt0063350" || got.TMDB != "10331" || got.TV {
		t.Fatalf("%+v", got)
	}
	if got := idsFromQuery(url.Values{"imdbid": {"tt1136608"}, "tvdbid": {"0"}}, true); got.IMDb != "tt1136608" || got.TVDB != "" {
		t.Fatalf("%+v", got)
	}
	if !idsFromQuery(url.Values{"imdbid": {"abc"}}, false).empty() {
		t.Fatal("bad id accepted")
	}
}

func TestSearchText(t *testing.T) {
	for in, want := range map[string]string{
		"La Nuit des morts-vivants": "La Nuit des morts vivants",
		"Example: The Movie!":       "Example The Movie",
		"¿Qué fue de Example?":      "Qué fue de Example",
		"Example's Day  2009":       "Example's Day 2009",
	} {
		if got := searchText(in); got != want {
			t.Errorf("searchText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalizedSearch(t *testing.T) {
	fx := newFixture(t)
	base := fx.srv.URL + "/torznab/api"

	// Off by default: ids are not offered and nothing is looked up.
	if _, caps := get(t, http.DefaultClient, base+"?t=caps&apikey="+testKey); strings.Contains(caps, "imdbid") {
		t.Fatalf("ids offered without search_languages: %s", caps)
	}

	var asked []string
	fx.a.cfg.Languages = []string{"es"}
	fx.a.lookup = func(ctx context.Context, ids mediaIDs, langs []string) ([]string, error) {
		asked = append(asked, ids.IMDb+":"+strings.Join(langs, ","))
		titles := map[string]string{"en": "Example Movie", "es": "Película de Ejemplo"}
		var out []string
		for _, l := range langs {
			out = append(out, titles[l])
		}
		return out, nil
	}
	if _, caps := get(t, http.DefaultClient, base+"?t=caps&apikey="+testKey); !strings.Contains(caps, `supportedParams="q,imdbid,tmdbid"`) {
		t.Fatalf("caps: %s", caps)
	}

	// Title + id: the app's title, then the Spanish one.
	fx.searches = nil
	get(t, http.DefaultClient, base+"?t=movie&q=Example+Movie&imdbid=1234567&apikey="+testKey)
	if got := keywords(fx); !reflect.DeepEqual(got, []string{"Example Movie", "Película de Ejemplo"}) {
		t.Fatalf("searches: %v", got)
	}

	// Id only: English is looked up too. The lookup is cached per languages.
	fx.searches = nil
	_, body := get(t, http.DefaultClient, base+"?t=movie&imdbid=tt1234567&apikey="+testKey)
	if got := keywords(fx); !reflect.DeepEqual(got, []string{"Example Movie", "Película de Ejemplo"}) {
		t.Fatalf("searches: %v", got)
	}
	if strings.Contains(body, "indexer test") || !strings.Contains(body, "Example Movie 2024") {
		t.Fatalf("id-only search: %s", body)
	}
	get(t, http.DefaultClient, base+"?t=movie&imdbid=tt1234567&apikey="+testKey)
	if !reflect.DeepEqual(asked, []string{"tt1234567:es", "tt1234567:en,es"}) {
		t.Fatalf("lookups: %v", asked)
	}
}

func keywords(fx *fixture) []string {
	var out []string
	for _, q := range fx.searches {
		out = append(out, q.Keywords)
	}
	return out
}

func TestWikidataTitles(t *testing.T) {
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		q := r.URL.Query()
		switch q.Get("action") {
		case "query":
			if q.Get("srsearch") != "haswbstatement:P345=tt0000001" {
				json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"search": []any{}}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"search": []any{map[string]string{"title": "Q1"}}}})
		case "wbgetentities":
			json.NewEncoder(w).Encode(map[string]any{"entities": map[string]any{"Q1": map[string]any{
				"labels": map[string]any{"en": map[string]string{"value": "Example Movie"},
					"es": map[string]string{"value": "Película de Ejemplo"}},
				"aliases": map[string]any{"es": []map[string]string{{"value": "Ejemplo"}, {"value": "Otra"}, {"value": "Tercera"}}},
			}}})
		}
	}))
	defer srv.Close()
	w := newWikidata("MyMule/test")
	w.endpoint = srv.URL
	got, err := w.Titles(context.Background(), mediaIDs{IMDb: "tt0000001"}, []string{"en", "es"})
	if err != nil || !reflect.DeepEqual(got, []string{"Example Movie", "Película de Ejemplo", "Ejemplo", "Otra"}) {
		t.Fatalf("%v %v", got, err)
	}
	if agent != "MyMule/test" {
		t.Fatalf("user agent %q", agent)
	}
	if got, err := w.Titles(context.Background(), mediaIDs{IMDb: "tt0000002"}, []string{"es"}); err != nil || got != nil {
		t.Fatalf("unknown id: %v %v", got, err)
	}
}
