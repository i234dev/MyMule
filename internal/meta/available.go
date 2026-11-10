package meta

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// When a movie or an episode can be found on the network: good copies
// appear when it comes out at home (digital or on disc), not when it opens
// in cinemas; an episode, when it airs.

// Availability is the day a work (or an episode, or the last episode of a
// season) comes out at home.
type Availability struct {
	Date time.Time `json:"date"`
	// Kind: "digital", "disc", "aired" (an episode), or "estimated" (from
	// the cinema release: no home release date is known yet).
	Kind string `json:"kind"`
}

// AvailabilityDater says when a work comes out at home, when the source
// knows it (ok false: no date known).
type AvailabilityDater interface {
	Available(ctx context.Context, id string, season, episode int) (a Availability, ok bool, err error)
}

// homeAfterCinema is how long after a cinema release a movie usually comes
// out at home, used when no home release date is known.
const homeAfterCinema = 45 * 24 * time.Hour

// TMDB release types (https://developer.themoviedb.org/reference/movie-release-dates).
const (
	tmdbPremiere   = 1
	tmdbLimited    = 2
	tmdbTheatrical = 3
	tmdbDigital    = 4
	tmdbPhysical   = 5
)

// Available returns when a TMDB movie comes out at home (the first digital
// or disc release anywhere: copies travel), or when an episode airs (for a
// whole season, its last episode).
func (t *TMDB) Available(ctx context.Context, id string, season, episode int) (Availability, bool, error) {
	kind, n, ok := ParseTMDBID(id)
	if !ok {
		return Availability{}, false, nil
	}
	if kind == Series {
		return t.episodeAvailable(ctx, id, season, episode)
	}
	key := "available|" + strconv.Itoa(n)
	if v, ok := t.cached(key); ok {
		a := v.(Availability)
		return a, !a.Date.IsZero(), nil
	}
	var r struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Dates   []struct {
				Type int    `json:"type"`
				Date string `json:"release_date"`
			} `json:"release_dates"`
		} `json:"results"`
	}
	if err := t.get(ctx, fmt.Sprintf("/movie/%d/release_dates", n), url.Values{}, &r); err != nil {
		return Availability{}, false, err
	}
	var home, cinema, premiere Availability
	for _, c := range r.Results {
		for _, d := range c.Dates {
			at, err := time.Parse(time.RFC3339, d.Date)
			if err != nil {
				continue
			}
			switch d.Type {
			case tmdbDigital, tmdbPhysical:
				if home.Date.IsZero() || at.Before(home.Date) {
					home = Availability{Date: at, Kind: "digital"}
					if d.Type == tmdbPhysical {
						home.Kind = "disc"
					}
				}
			case tmdbLimited, tmdbTheatrical:
				if cinema.Date.IsZero() || at.Before(cinema.Date) {
					cinema = Availability{Date: at}
				}
			case tmdbPremiere:
				if premiere.Date.IsZero() || at.Before(premiere.Date) {
					premiere = Availability{Date: at}
				}
			}
		}
	}
	if cinema.Date.IsZero() {
		cinema = premiere // a festival, only when nothing else says
	}
	a := home
	if a.Date.IsZero() && !cinema.Date.IsZero() {
		a = Availability{Date: cinema.Date.Add(homeAfterCinema), Kind: "estimated"}
	}
	t.store(key, a)
	return a, !a.Date.IsZero(), nil
}

// episodeAvailable: when an episode airs, or a whole season's last one.
func (t *TMDB) episodeAvailable(ctx context.Context, id string, season, episode int) (Availability, bool, error) {
	if season <= 0 {
		return Availability{}, false, nil
	}
	eps, err := t.SeasonEpisodes(ctx, id, season, "en")
	if err != nil {
		return Availability{}, false, err
	}
	var a Availability
	for _, e := range eps {
		if episode > 0 && e.Number != episode {
			continue
		}
		at, err := time.Parse(time.DateOnly, e.AirDate)
		if err != nil {
			if episode > 0 || e.AirDate == "" {
				// Not dated yet: it has not aired.
				return Availability{Date: farFuture, Kind: "aired"}, true, nil
			}
			continue
		}
		if at.After(a.Date) {
			a = Availability{Date: at, Kind: "aired"}
		}
	}
	return a, !a.Date.IsZero(), nil
}

// farFuture stands for "announced, without a date yet".
var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Undated reports whether a is "announced, without a date yet".
func (a Availability) Undated() bool { return a.Date.Equal(farFuture) }
