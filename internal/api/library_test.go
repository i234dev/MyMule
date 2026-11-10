package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mymule/internal/download"
	"mymule/internal/library"
	"mymule/internal/meta"
	"mymule/internal/prefs"
)

// fakeSource knows one made-up series.
type fakeSource struct{}

func (fakeSource) Search(context.Context, string, string) ([]meta.Work, error) { return nil, nil }
func (fakeSource) Get(_ context.Context, id, _ string) (meta.Work, error) {
	if strings.HasPrefix(id, "tmdb-movie-") {
		return meta.Work{ID: id, Kind: meta.Movie, Title: "La Película", OriginalTitle: "The Film", Year: 1999}, nil
	}
	return meta.Work{ID: id, Kind: meta.Series, Title: "La Serie", OriginalTitle: "The Show", Year: 2001}, nil
}
func (fakeSource) EpisodeTitle(_ context.Context, _ string, s, e int, _ string) (string, error) {
	return "Episodio Inventado", nil
}

func TestLibraryAPI(t *testing.T) {
	// Library folders under the Windows per-user application folders are
	// refused, and that is where test temporary folders live there.
	t.Setenv("LOCALAPPDATA", `Z:\no-such-appdata-for-tests`)
	t.Setenv("APPDATA", `Z:\no-such-appdata-for-tests`)
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp})
	rest := NewREST(m, restKey, "test")
	data := t.TempDir()
	ps := prefs.Open(filepath.Join(data, "preferences.json"), data)
	items := library.OpenStore(filepath.Join(data, "library.json"))
	changed := 0
	rest.EnableMedia(fakeSource{})
	rest.EnableLibrary(ps, items, func() { changed++ })
	srv := httptest.NewServer(rest)
	t.Cleanup(func() {
		srv.Close()
		_, files := m.Status()
		for _, f := range files {
			m.Cancel(f.Hash)
		}
	})

	var got struct {
		Preferences prefs.Preferences   `json:"preferences"`
		Presets     map[string][]string `json:"presets"`
	}
	if code := call(t, srv, "GET", "/api/v1/preferences", "", "", &got); code != 200 || len(got.Presets["movie"]) == 0 {
		t.Fatalf("get: %d %+v", code, got)
	}

	var prev map[string]string
	call(t, srv, "POST", "/api/v1/preferences/preview", "application/json",
		`{"movie_template":"{year} - {title}","series_template":"{title} {year}"}`, &prev)
	if prev["movie"] != "1999 - La Película de Ejemplo.mkv" || prev["series_error"] == "" {
		t.Fatalf("preview: %+v", prev)
	}

	series := filepath.Join(t.TempDir(), "Series")
	// Organising is off by default: a download says so and keeps its name.
	var off mediaDownloadResponse
	call(t, srv, "POST", "/api/v1/media/download", "application/json",
		`{"link":"ed2k://|file|Some.Movie.1999.1080p.mkv|20000000|AAAA456789ABCDEF0123456789ABCDEF|/","work_id":"tmdb-movie-1","lang":"es-ES"}`, &off)
	if off.Organize || off.File != "" {
		t.Fatalf("organising reported while off: %+v", off)
	}

	body := `{"library":{"organize":true,"series_dir":` + jsonString(series) + `,"movie_template":"{title} ({year})",` +
		`"series_template":"{title}/S{season:2}/{title} - {season}x{episode:2} - {episode_title}","share":true}}`
	if code := call(t, srv, "PUT", "/api/v1/preferences", "application/json", body, nil); code != 200 || changed != 1 {
		t.Fatalf("put: %d, changed %d", code, changed)
	}
	if code := call(t, srv, "PUT", "/api/v1/preferences", "application/json",
		`{"library":{"movie_template":"{year}","series_template":"{title}"}}`, nil); code != 400 {
		t.Fatalf("bad templates saved: %d", code)
	}

	link := "ed2k://|file|The.Show.S02E05.720p.mkv|20000000|0123456789ABCDEF0123456789ABCDEF|/"
	var got2 mediaDownloadResponse
	if code := call(t, srv, "POST", "/api/v1/media/download", "application/json",
		`{"link":`+jsonString(link)+`,"work_id":"tmdb-tv-1","lang":"es-ES","season":2}`, &got2); code != 200 {
		t.Fatalf("download: %d", code)
	}
	// The response says where it will go.
	if !got2.Organize || got2.Folder != series || got2.File != "La Serie/S02/La Serie - 2x05 - Episodio Inventado.mkv" {
		t.Fatalf("planned destination: %+v", got2)
	}
	// No movies folder: renamed inside the download folder, no subfolders.
	var inPlace mediaDownloadResponse
	call(t, srv, "POST", "/api/v1/media/download", "application/json",
		`{"link":"ed2k://|file|Other.Movie.2001.mkv|20000000|BBBB456789ABCDEF0123456789ABCDEF|/","work_id":"tmdb-movie-2","lang":"es-ES"}`, &inPlace)
	if !inPlace.Organize || inPlace.Folder != "" || inPlace.File != "La Película (1999).mkv" {
		t.Fatalf("rename in place: %+v", inPlace)
	}
	it, ok := items.Get("0123456789ABCDEF0123456789ABCDEF")
	if !ok || it.Season != 2 || it.Episode != 5 || it.EpisodeTitle != "Episodio Inventado" || it.Quality != "720p" {
		t.Fatalf("item: %+v %v", it, ok)
	}
	dst := library.Destination(ps.Library(), it, "/tmp/The.Show.S02E05.720p.mkv")
	if want := filepath.Join(series, "La Serie", "S02", "La Serie - 2x05 - Episodio Inventado.mkv"); dst != want {
		t.Fatalf("destination %s, want %s", dst, want)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
