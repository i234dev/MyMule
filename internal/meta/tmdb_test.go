package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeTMDB(t *testing.T) *TMDB {
	t.Helper()
	mux := http.NewServeMux()
	check := func(r *http.Request) {
		if r.URL.Query().Get("api_key") != "k" {
			t.Errorf("api key not sent")
		}
	}
	mux.HandleFunc("/search/multi", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		if r.URL.Query().Get("language") != "es-ES" {
			t.Errorf("language %q", r.URL.Query().Get("language"))
		}
		json.NewEncoder(w).Encode(map[string]any{"results": []any{
			map[string]any{"media_type": "movie", "id": 11, "title": "La Película", "original_title": "The Movie",
				"release_date": "1982-06-25", "overview": "Una película de ejemplo.", "poster_path": "/p11.jpg"},
			map[string]any{"media_type": "person", "id": 5, "name": "Someone"},
			map[string]any{"media_type": "tv", "id": 22, "name": "La Serie", "original_name": "The Show",
				"first_air_date": "2001-01-01"},
		}})
	})
	mux.HandleFunc("/tv/22", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		json.NewEncoder(w).Encode(map[string]any{"id": 22, "name": "La Serie", "original_name": "The Show",
			"first_air_date": "2001-01-01", "poster_path": "/p22.jpg",
			"external_ids":       map[string]any{"imdb_id": "tt0000022", "tvdb_id": 2222},
			"alternative_titles": map[string]any{"results": []any{map[string]string{"iso_3166_1": "ES", "title": "Serie de Ejemplo"}, map[string]string{"iso_3166_1": "FR", "title": "La Série"}}},
			"translations": map[string]any{"translations": []any{
				map[string]any{"iso_639_1": "es", "iso_3166_1": "MX", "data": map[string]string{"name": "La Serie Mexicana"}},
				map[string]any{"iso_639_1": "es", "iso_3166_1": "ES", "data": map[string]string{"name": "La Serie"}},
				map[string]any{"iso_639_1": "fr", "iso_3166_1": "FR", "data": map[string]string{"name": "La Série"}}}},
			"seasons": []any{map[string]any{"season_number": 0, "episode_count": 3}, map[string]any{"season_number": 1, "episode_count": 10, "name": "Temporada 1"}},
		})
	})
	mux.HandleFunc("/tv/22/season/1", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		json.NewEncoder(w).Encode(map[string]any{"episodes": []any{
			map[string]any{"episode_number": 1, "name": "Primer Episodio", "air_date": "2001-01-01"},
			map[string]any{"episode_number": 2, "name": "Segundo Episodio"}}})
	})
	mux.HandleFunc("/movie/404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tm := NewTMDB("k")
	tm.API = srv.URL
	return tm
}

func TestTMDB(t *testing.T) {
	tm := fakeTMDB(t)
	works, err := tm.Search(context.Background(), "la pelicula", "es-es")
	if err != nil || len(works) != 2 {
		t.Fatalf("%+v %v", works, err)
	}
	m := works[0]
	if m.ID != "tmdb-movie-11" || m.Kind != Movie || m.Title != "La Película" || m.OriginalTitle != "The Movie" ||
		m.Year != 1982 || m.Poster != "/p11.jpg" || m.IDs.TMDB != "11" {
		t.Fatalf("movie: %+v", m)
	}
	if works[1].ID != "tmdb-tv-22" || works[1].Kind != Series {
		t.Fatalf("series: %+v", works[1])
	}

	s, err := tm.Get(context.Background(), "tmdb-tv-22", "es-ES")
	if err != nil {
		t.Fatal(err)
	}
	if s.IDs.IMDb != "tt0000022" || s.IDs.TVDB != "2222" || s.Poster != "/p22.jpg" {
		t.Fatalf("ids: %+v", s)
	}
	// Spanish titles from other countries and Spain's alternative titles;
	// not the French ones.
	if !reflect.DeepEqual(s.Aliases, []string{"La Serie Mexicana", "Serie de Ejemplo"}) {
		t.Fatalf("aliases: %q", s.Aliases)
	}
	if !reflect.DeepEqual(s.Seasons, []Season{{Number: 1, Episodes: 10, Name: "Temporada 1"}}) {
		t.Fatalf("seasons: %+v", s.Seasons)
	}
	eps, err := tm.SeasonEpisodes(context.Background(), "tmdb-tv-22", 1, "es-ES")
	if err != nil || len(eps) != 2 || eps[1].Name != "Segundo Episodio" {
		t.Fatalf("episodes: %+v %v", eps, err)
	}
	if name, _ := tm.EpisodeTitle(context.Background(), "tmdb-tv-22", 1, 1, "es-ES"); name != "Primer Episodio" {
		t.Fatalf("episode title from the season list: %q", name)
	}
	if _, err := tm.Get(context.Background(), "tmdb-movie-404", "es"); err == nil || strings.Contains(err.Error(), "api_key") {
		t.Fatalf("error should not reveal the key: %v", err)
	}
	for _, bad := range []string{"Q1", "tmdb-x-1", "tmdb-movie-abc", "tmdb-movie--3"} {
		if _, _, ok := ParseTMDBID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

type failing struct{}

func (failing) Search(context.Context, string, string) ([]Work, error) {
	return nil, os.ErrDeadlineExceeded
}
func (failing) Get(context.Context, string, string) (Work, error) {
	return Work{}, os.ErrDeadlineExceeded
}

func TestFallback(t *testing.T) {
	wd, _ := fakeWikidata(t)
	src := WithFallback(failing{}, wd)
	works, err := src.Search(context.Background(), "example movie", "es-ES")
	if err != nil || len(works) == 0 || works[0].ID != "Q1" {
		t.Fatalf("fallback search: %+v %v", works, err)
	}
	if w, err := src.Get(context.Background(), "Q1", "es"); err != nil || w.ID != "Q1" {
		t.Fatalf("Wikidata ids go to Wikidata: %+v %v", w, err)
	}
}

func TestPosters(t *testing.T) {
	fetched := 0
	img := strings.Repeat("x", 4000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched++
		if r.URL.Path == "/w185/notimage.jpg" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte(img))
	}))
	defer srv.Close()
	dir := t.TempDir()
	p := NewPosters(dir, 10000)
	p.BaseURL = srv.URL
	mux := http.NewServeMux()
	mux.Handle("GET /web/poster/{size}/{file}", p)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if rec := get("/web/poster/w185/abcd1.jpg"); rec.Code != 200 || rec.Body.String() != img || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("poster: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	get("/web/poster/w185/abcd1.jpg")
	if fetched != 1 {
		t.Fatalf("not cached: fetched %d times", fetched)
	}
	for _, bad := range []string{"/web/poster/w9999/abcd1.jpg", "/web/poster/w185/..%2Fsecret.jpg", "/web/poster/w185/abcd1.exe"} {
		if rec := get(bad); rec.Code != 404 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := get("/web/poster/w185/notimage.jpg"); rec.Code != 502 {
		t.Errorf("non-image accepted: %d", rec.Code)
	}
	// The cache stays under its limit (10000 bytes: two posters of 4000).
	for _, f := range []string{"abcd2.jpg", "abcd3.jpg", "abcd4.jpg"} {
		get("/web/poster/w185/" + f)
	}
	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	if total > 10000 {
		t.Fatalf("cache %d bytes, over the limit", total)
	}
	if _, err := os.Stat(filepath.Join(dir, "w185_abcd4.jpg")); err != nil {
		t.Fatalf("newest poster missing from cache")
	}
}
