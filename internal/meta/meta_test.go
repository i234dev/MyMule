package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakeWikidata answers the API and SPARQL requests the client makes.
func fakeWikidata(t *testing.T) (*Wikidata, *[]string) {
	t.Helper()
	var agents []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.UserAgent())
		q := r.URL.Query()
		switch q.Get("action") {
		case "query":
			s := q.Get("srsearch")
			var hits []any
			switch {
			case s == "haswbstatement:P345=tt0000001":
				hits = []any{map[string]string{"title": "Q1"}}
			case strings.HasPrefix(s, "example movie haswbstatement:P31=Q11424|"):
				hits = []any{map[string]string{"title": "Q2"}, map[string]string{"title": "Q1"}, map[string]string{"title": "Q3"}}
			}
			json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"search": hits}})
		case "wbgetentities":
			json.NewEncoder(w).Encode(map[string]any{"entities": map[string]any{"Q1": map[string]any{
				"labels": map[string]any{"en": map[string]string{"value": "Example Movie"},
					"es": map[string]string{"value": "Película de Ejemplo"}},
				"aliases": map[string]any{"es": []map[string]string{{"value": "Ejemplo"}, {"value": "Otra"}, {"value": "Tercera"}}},
			}}})
		}
	})
	mux.HandleFunc("/sparql", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if !strings.Contains(q, `LANG(?label) = "es"`) {
			t.Errorf("language not in query")
		}
		b := func(m map[string]string) map[string]any {
			out := map[string]any{}
			for k, v := range m {
				out[k] = map[string]string{"value": v}
			}
			return out
		}
		json.NewEncoder(w).Encode(map[string]any{"results": map[string]any{"bindings": []any{
			b(map[string]string{"item": "http://www.wikidata.org/entity/Q1", "types": "Q11424", "year": "1982",
				"imdbv": "tt0000001", "tmdbmv": "11", "linksv": "60", "origv": "Example Movie",
				"labelv": "Película de Ejemplo", "descv": "película de 1982", "alts": "Ejemplo\u001fOtra"}),
			b(map[string]string{"item": "http://www.wikidata.org/entity/Q2", "types": "Q5398426 Q7725634", "year": "2001",
				"tmdbtv": "22", "linksv": "2", "env": "Example Series"}),
			b(map[string]string{"item": "http://www.wikidata.org/entity/Q3", "types": "Q7725634", "env": "A Book"}),
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	w := NewWikidata("MyMule/test")
	w.API, w.SPARQL = srv.URL+"/api", srv.URL+"/sparql"
	return w, &agents
}

func TestSearch(t *testing.T) {
	w, agents := fakeWikidata(t)
	works, err := w.Search(context.Background(), `Example "Movie"`, "es")
	if err == nil && len(works) == 0 {
		// The quotes are removed; the fake only knows the lowercase query.
		works, err = w.Search(context.Background(), "example movie", "es")
	}
	if err != nil || len(works) != 2 {
		t.Fatalf("%+v %v", works, err)
	}
	// The popular movie comes before the obscure series; the book is left out.
	m, s := works[0], works[1]
	if m.ID != "Q1" || m.Kind != Movie || m.Title != "Película de Ejemplo" || m.OriginalTitle != "Example Movie" ||
		m.Year != 1982 || m.IDs.IMDb != "tt0000001" || m.IDs.TMDB != "11" {
		t.Fatalf("movie: %+v", m)
	}
	if got := m.Titles(); !reflect.DeepEqual(got, []string{"Película de Ejemplo", "Example Movie", "Ejemplo", "Otra"}) {
		t.Fatalf("titles: %v", got)
	}
	if s.Kind != Series || s.Title != "Example Series" || s.IDs.TMDB != "22" {
		t.Fatalf("series: %+v", s)
	}
	if (*agents)[0] != "MyMule/test" {
		t.Fatalf("user agent %q", (*agents)[0])
	}
	if _, err := w.Get(context.Background(), "Q1; DROP", "es"); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestTitlesByID(t *testing.T) {
	w, _ := fakeWikidata(t)
	got, err := w.TitlesByID(context.Background(), IDs{IMDb: "tt0000001"}, false, []string{"en", "es"})
	if err != nil || !reflect.DeepEqual(got, []string{"Example Movie", "Película de Ejemplo", "Ejemplo", "Otra"}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := w.TitlesByID(context.Background(), IDs{IMDb: "tt0000002"}, false, []string{"es"}); err != nil || got != nil {
		t.Fatalf("unknown id: %v %v", got, err)
	}
}

// TestLive queries the real Wikidata with your own search words:
// MYMULE_LIVE="some title" go test ./internal/meta -run TestLive -v
func TestLive(t *testing.T) {
	q := os.Getenv("MYMULE_LIVE")
	if q == "" {
		t.Skip("set MYMULE_LIVE to some search words to query the real Wikidata")
	}
	w := NewWikidata("MyMule-test (https://github.com/i234dev/MyMule)")
	works, err := w.Search(context.Background(), q, "es")
	if err != nil {
		t.Fatal(err)
	}
	for i, wk := range works {
		if i == 8 {
			break
		}
		t.Logf("%s %-6s %d %q orig=%q aliases=%q imdb=%s pop=%d", wk.ID, wk.Kind, wk.Year, wk.Title, wk.OriginalTitle, wk.Aliases, wk.IDs.IMDb, wk.Popularity)
	}
}

// An answer far larger than any real one is refused instead of read
// into memory.
func TestDecodeLimited(t *testing.T) {
	var v map[string]any
	big := strings.NewReader(`{"a":"` + strings.Repeat("x", maxJSONBytes) + `"}`)
	if err := decodeLimited(big, &v); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("huge answer: %v", err)
	}
	if err := decodeLimited(strings.NewReader(`{"a":1}`), &v); err != nil || v["a"] != 1.0 {
		t.Fatalf("small answer: %v %v", v, err)
	}
}
