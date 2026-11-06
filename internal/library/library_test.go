package library

import (
	"os"
	"path/filepath"
	"testing"
)

var movie = Item{Kind: "movie", Title: "La Película", OriginalTitle: "The Movie", Year: 1982, Quality: "1080p"}
var episode = Item{Kind: "series", Title: "La Serie", OriginalTitle: "The Show", Season: 2, Episode: 5, EpisodeTitle: "El Regreso"}

func p(parts ...string) string { return filepath.Join(parts...) }

func TestRender(t *testing.T) {
	cases := []struct {
		tmpl string
		it   Item
		orig bool
		want string
	}{
		{MoviePresets[0], movie, false, p("La Película (1982)", "La Película (1982)")},
		{MoviePresets[1], movie, true, "The Movie (1982)"},
		{MoviePresets[2], movie, false, "1982 - La Película"},
		{MoviePresets[3], movie, false, "La Película (1982) [1080p]"},
		{SeriesPresets[0], episode, false, p("La Serie", "Season 02", "La Serie - S02E05")},
		{SeriesPresets[1], episode, false, p("La Serie", "Temporada 02", "La Serie - 2x05")},
		{SeriesPresets[2], episode, false, p("La Serie", "Season 02", "La Serie - S02E05 - El Regreso")},
		// Missing values leave no empty brackets or dangling dashes.
		{MoviePresets[3], Item{Kind: "movie", Title: "Sin Datos"}, false, "Sin Datos"},
		{SeriesPresets[2], Item{Kind: "series", Title: "La Serie", Season: 1, Episode: 3}, false, p("La Serie", "Season 01", "La Serie - S01E03")},
		// Characters not allowed in file names, and slashes in values.
		{"{title} ({year})", Item{Title: `Qué: "Él"? <Sí>/No*`, Year: 2001}, false, "Qué Él Sí -No (2001)"},
		{"{title}", Item{Title: "Fin..."}, false, "Fin"},
		{"{title}", Item{Title: "CON"}, false, "_CON"},
	}
	for _, c := range cases {
		if got := Render(c.tmpl, c.it, c.orig); got != c.want {
			t.Errorf("Render(%q)\n got  %q\n want %q", c.tmpl, got, c.want)
		}
	}
}

func TestCheckTemplate(t *testing.T) {
	for _, tmpl := range MoviePresets {
		if err := CheckTemplate(tmpl, false); err != nil {
			t.Errorf("%q: %v", tmpl, err)
		}
	}
	for _, tmpl := range SeriesPresets {
		if err := CheckTemplate(tmpl, true); err != nil {
			t.Errorf("%q: %v", tmpl, err)
		}
	}
	for _, bad := range []string{"", "{year}", "{title}/../{title}", "{title} {nope}", "../{title}"} {
		if CheckTemplate(bad, false) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if CheckTemplate("{title} ({year})", true) == nil {
		t.Error("series template without episode accepted")
	}
}

func TestDestinationAndMove(t *testing.T) {
	movies, series, incoming := t.TempDir(), t.TempDir(), t.TempDir()
	st := DefaultSettings()
	st.MoviesDir, st.SeriesDir = movies, series
	if Destination(st, movie, filepath.Join(incoming, "x.mkv")) != "" {
		t.Fatal("organised while organising is off (the default)")
	}
	st.Organize = true

	src := filepath.Join(incoming, "the.movie.1982.1080p.MKV")
	os.WriteFile(src, []byte("video"), 0o644)
	dst := Destination(st, movie, src)
	if dst != filepath.Join(movies, "La Película (1982)", "La Película (1982).mkv") {
		t.Fatalf("destination: %s", dst)
	}
	got, err := Move(src, dst)
	if err != nil || got != dst {
		t.Fatalf("move: %s %v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source still there")
	}
	// The same movie again: a second copy, not overwritten.
	src2 := filepath.Join(incoming, "other.mkv")
	os.WriteFile(src2, []byte("other"), 0o644)
	got2, err := Move(src2, dst)
	if err != nil || got2 != filepath.Join(movies, "La Película (1982)", "La Película (1982) (2).mkv") {
		t.Fatalf("second copy: %s %v", got2, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "video" {
		t.Fatal("first file overwritten")
	}

	// A whole season in one file keeps its name, in the season folder.
	season := Item{Kind: "series", Title: "La Serie", Season: 3}
	if d := Destination(st, season, p(incoming, "La Serie T3 completa.avi")); d != p(series, "La Serie", "Season 03", "La Serie T3 completa.avi") {
		t.Fatalf("season pack: %s", d)
	}
	// No folder for the kind: renamed where it is, without subfolders (the
	// download folder is shared without them).
	st.SeriesDir = ""
	if d := Destination(st, episode, src); d != filepath.Join(incoming, "La Serie - S02E05.mkv") {
		t.Fatalf("no series folder: %s", d)
	}
	// Never outside the chosen folder, whatever the title says.
	evil := Item{Kind: "movie", Title: "..", OriginalTitle: ".."}
	if d := Destination(st, evil, src); d != "" && filepath.Dir(filepath.Dir(d)) != movies && filepath.Dir(d) != movies {
		t.Fatalf("escaped: %s", d)
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.json")
	s := OpenStore(path)
	s.Put("abcdef", movie)
	s2 := OpenStore(path) // survives restarts
	if it, ok := s2.Get("ABCDEF"); !ok || it.Title != movie.Title {
		t.Fatalf("get: %+v %v", it, ok)
	}
	if _, ok := s2.Take("abcdef"); !ok {
		t.Fatal("take")
	}
	if _, ok := OpenStore(path).Get("abcdef"); ok {
		t.Fatal("still there after take")
	}
}

// Only media is moved into the library: a download named like a script or
// a shortcut stays where it is, whatever folder the library points to.
func TestDestinationOnlyMedia(t *testing.T) {
	st := Settings{Organize: true, MoviesDir: t.TempDir(), MovieTemplate: MoviePresets[0]}
	it := Item{Kind: "movie", Title: "Example Movie", Year: 2001}
	for _, name := range []string{"x.bat", "x.cmd", "x.exe", "x.lnk", "x.ps1", "x.sh", "x.desktop", "x.url", "x"} {
		if d := Destination(st, it, filepath.Join("in", name)); d != "" {
			t.Errorf("%s would be moved to %s", name, d)
		}
	}
	for _, name := range []string{"x.mkv", "X.MP4", "x.avi", "x.srt"} {
		if Destination(st, it, filepath.Join("in", name)) == "" {
			t.Errorf("%s would not be moved", name)
		}
	}
}
