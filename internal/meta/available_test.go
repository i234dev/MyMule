package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A movie is available at its first digital or disc release anywhere; with
// only cinema dates, an estimate after them; an episode when it airs, a
// season when its last episode does, and one not dated yet is not.
func TestAvailable(t *testing.T) {
	mux := http.NewServeMux()
	dates := func(list ...any) map[string]any { return map[string]any{"results": list} }
	country := func(c string, ds ...any) map[string]any {
		return map[string]any{"iso_3166_1": c, "release_dates": ds}
	}
	d := func(typ int, at string) map[string]any { return map[string]any{"type": typ, "release_date": at} }
	mux.HandleFunc("/movie/1/release_dates", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(dates(
			country("ES", d(3, "2026-07-17T00:00:00.000Z"), d(4, "2026-11-20T00:00:00.000Z")),
			country("US", d(3, "2026-07-17T00:00:00.000Z"), d(5, "2026-12-01T00:00:00.000Z"), d(4, "2026-10-14T00:00:00.000Z"))))
	})
	mux.HandleFunc("/movie/2/release_dates", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(dates(country("FR", d(1, "2026-05-01T00:00:00.000Z"), d(3, "2026-09-01T00:00:00.000Z"))))
	})
	mux.HandleFunc("/movie/3/release_dates", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(dates())
	})
	mux.HandleFunc("/tv/7/season/1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"episodes": []any{
			map[string]any{"episode_number": 1, "air_date": "2026-01-05"},
			map[string]any{"episode_number": 2, "air_date": "2026-01-12"},
			map[string]any{"episode_number": 3}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tm := NewTMDB("k")
	tm.API = srv.URL
	ctx := context.Background()
	day := func(s string) time.Time { at, _ := time.Parse(time.DateOnly, s); return at }

	for _, c := range []struct {
		id      string
		season  int
		episode int
		want    Availability
		known   bool
		undated bool
	}{
		{"tmdb-movie-1", 0, 0, Availability{day("2026-10-14"), "digital"}, true, false},
		{"tmdb-movie-2", 0, 0, Availability{day("2026-09-01").Add(homeAfterCinema), "estimated"}, true, false},
		{"tmdb-movie-3", 0, 0, Availability{}, false, false},
		{"tmdb-tv-7", 1, 2, Availability{day("2026-01-12"), "aired"}, true, false},
		{"tmdb-tv-7", 1, 3, Availability{}, true, true},
		{"tmdb-tv-7", 1, 0, Availability{}, true, true}, // its last episode has no date yet
		{"tmdb-tv-7", 0, 0, Availability{}, false, false},
		{"Q42", 0, 0, Availability{}, false, false},
	} {
		a, ok, err := tm.Available(ctx, c.id, c.season, c.episode)
		if err != nil || ok != c.known || a.Undated() != c.undated || (!c.undated && !a.Date.Equal(c.want.Date)) || (!c.undated && a.Kind != c.want.Kind) {
			t.Errorf("%s %d/%d: %+v %v %v", c.id, c.season, c.episode, a, ok, err)
		}
	}
}
