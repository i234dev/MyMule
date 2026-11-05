package release

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]Info{
		"Example Movie (2009).(Spanish.English.Subs).BDrip.1080p.x264-AC3.mkv": {
			Languages: []string{"es", "en"}, Subs: true, Resolution: "1080p", Source: "bluray", Year: 2009, Video: true},
		"Example.Movie.(2009).BDremux.1080p.AC3.DTS.Spa.Eng.mkv": {
			Languages: []string{"es", "en"}, Resolution: "1080p", Source: "remux", Year: 2009, Video: true},
		"[kad example.org] Example Movie (2009) [MicroHD 1080p][AC3 2.0-Castellano].mkv": {
			Languages: []string{"es"}, Resolution: "1080p", Year: 2009, Video: true, Bridged: true},
		"Example Movie 2009 Latino DVDRip.avi": {
			Languages: []string{"es-419"}, Resolution: "480p", Source: "dvd", Year: 2009, Video: true},
		"Example Movie (V.O.Sub.Español) 2009 Telesync HQ Xvid.avi": {
			Languages: []string{"es"}, Subs: true, Resolution: "480p", Source: "cam", Year: 2009, Video: true},
		"Example Show S02E05 720p WEB-DL.mkv": {
			Resolution: "720p", Source: "web", Season: 2, Episode: 5, Video: true},
		"Example Show - 3x07 - Episode Title.avi":   {Season: 3, Episode: 7, Video: true},
		"Example Show Temporada 2 Cap.205 HDTV.avi": {Source: "hdtv", Season: 2, Episode: 5, Video: true},
		"Example Show Season 4 Complete.mkv":        {Season: 4, Video: true},
		"Example 2001 - 1080p - Dual.mkv":           {Resolution: "1080p", Dual: true, Year: 2001, Video: true},
		"Example Book.epub":                         {},
	}
	for name, want := range cases {
		if got := Parse(name); !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q)\n got  %+v\n want %+v", name, got, want)
		}
	}
}

func TestMatchTitle(t *testing.T) {
	titles := []string{"La Película", "The Movie"}
	for name, want := range map[string]string{
		"La.Pelicula.(1982).1080p.mkv":       "La Película",
		"[kad x] THE MOVIE 1982.avi":         "The Movie",
		"The Movies of 1982.avi":             "",
		"Otra película de 1982.mkv":          "",
		"The Movie - La Película (1982).mkv": "La Película",
		// The title must stand on its own, not be part of a longer text.
		"La.Pelicula.del.Lago.(1982).1080p.mkv":          "",
		"Some.Show.3x23.El.de.la.pelicula.de.Fulano.mkv": "",
		"S03E03 Fulanito y la película [dual 1080p].mkv": "",
		"(1982) La película (The Movie).DVDrip.avi":      "La Película",
		"Otro Título; The Movie (1982) _ [eng+spa].mkv":  "The Movie",
		"La Pelicula - The Movie (BDrip 1080p ENG-SPA)":  "La Película",
		"La Película Dual 1080p.mkv":                     "La Película",
	} {
		if got := MatchTitle(name, titles); got != want {
			t.Errorf("MatchTitle(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEvaluateMovie(t *testing.T) {
	target := Target{Titles: []string{"La Película", "The Movie"}, Year: 1982,
		TitleLanguage: map[string]string{"La Película": "es"}}
	prefs := Prefs{Languages: []string{"es"}, Subs: true, Resolution: "1080p", NoCam: true}
	in := []Candidate{
		{Name: "The Movie 1982 1080p BluRay x264.mkv", Size: 8e9, Sources: 40, Complete: 35},
		{Name: "La Película (1982) [BluRay 1080p][Castellano AC3].mkv", Size: 4e9, Sources: 6, Complete: 5},
		{Name: "La Película (1982).avi", Size: 7e8, Sources: 3, Complete: 3},         // Spanish guessed from title
		{Name: "The Movie 2011 1080p WEB.mkv", Size: 4e9, Sources: 50, Complete: 50}, // the remake
		{Name: "The Movie 1982 TS.avi", Size: 7e8, Sources: 9, Complete: 9},          // cam
		{Name: "[kad x] La Película (1982) 1080p Castellano.mkv", Size: 4e9, Sources: 1, Complete: 1},
		{Name: "La Película (1982) 1080p Castellano.mkv", Size: 4e9, Sources: 0}, // no sources
		{Name: "Another Film 1982.mkv", Size: 4e9, Sources: 9, Complete: 9},
	}
	var out []Candidate
	for _, c := range in {
		if Evaluate(&c, target, prefs) {
			out = append(out, c)
		}
	}
	if len(out) != 6 {
		t.Fatalf("kept %d, want 6 (not the remake, not the other film)", len(out))
	}
	Rank(out)
	if out[0].Name != "La Película (1982) [BluRay 1080p][Castellano AC3].mkv" {
		t.Fatalf("best: %q (%d)", out[0].Name, out[0].Score)
	}
	visible := 0
	for _, c := range out {
		if !c.Hidden {
			visible++
		}
		if c.Name == "La Película (1982).avi" && c.GuessedLanguage != "es" {
			t.Errorf("language not guessed from title")
		}
	}
	if visible != 3 {
		t.Fatalf("visible %d, want 3 (cam, kad-only and no-source hidden): %+v", visible, out)
	}
}

func TestEvaluateEpisode(t *testing.T) {
	target := Target{Titles: []string{"Example Show"}, Series: true, Season: 2, Episode: 5}
	prefs := Prefs{Languages: []string{"en"}}
	for name, want := range map[string]bool{
		"Example Show S02E05 720p.mkv":    true,
		"Example Show 2x05.avi":           true,
		"Example Show S02E06 720p.mkv":    false,
		"Example Show S01E05 720p.mkv":    false,
		"Example Show Temporada 2.mkv":    true, // a season pack contains it
		"Example Show the movie 2010.mkv": false,
	} {
		c := Candidate{Name: name, Sources: 1, Complete: 1}
		if got := Evaluate(&c, target, prefs); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}
