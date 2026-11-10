package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mymule/internal/download"
	"mymule/internal/library"
	"mymule/internal/prefs"
)

func TestCategories(t *testing.T) {
	// Folders under the Windows per-user application folders are refused,
	// and that is where test temporary folders live there.
	t.Setenv("LOCALAPPDATA", `Z:\no-such-appdata-for-tests`)
	t.Setenv("APPDATA", `Z:\no-such-appdata-for-tests`)
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp})
	rest := NewREST(m, restKey, "test")
	data := t.TempDir()
	ps := prefs.Open(filepath.Join(data, "preferences.json"), data)
	ps.Protect(tmp) // as the daemon does
	items := library.OpenStore(filepath.Join(data, "library.json"))
	rest.EnableMedia(fakeSource{})
	rest.EnableLibrary(ps, items, nil)
	srv := httptest.NewServer(rest)
	t.Cleanup(func() {
		srv.Close()
		_, files := m.Status()
		for _, f := range files {
			m.Cancel(f.Hash)
		}
	})

	music := filepath.Join(t.TempDir(), "Music")
	for _, bad := range []string{
		`{"categories":[{"name":"","dir":` + jsonString(music) + `}]}`,
		`{"categories":[{"name":"Music","dir":""}]}`,
		`{"categories":[{"name":"Music","dir":` + jsonString(music) + `},{"name":"music","dir":` + jsonString(music) + `}]}`,
		`{"categories":[{"name":"Temp","dir":` + jsonString(tmp) + `}]}`,
	} {
		if code := call(t, srv, "PUT", "/api/v1/preferences", "application/json", bad, nil); code != 400 {
			t.Errorf("saved %s: %d", bad, code)
		}
	}
	if code := call(t, srv, "PUT", "/api/v1/preferences", "application/json",
		`{"categories":[{"name":" Music ","dir":`+jsonString(music)+`,"share":true}]}`, nil); code != 200 {
		t.Fatalf("save: %d", code)
	}
	if c, ok := ps.Category("MUSIC"); !ok || c.Name != "Music" || c.Dir != music || !c.Share {
		t.Fatalf("category: %+v %v", c, ok)
	}

	// An unknown category adds nothing.
	const song = "ed2k://|file|Made-up Band - Song.mp3|5000000|C0C0456789ABCDEF0123456789ABCDEF|/"
	if code := call(t, srv, "POST", "/api/v1/downloads", "application/json",
		`{"links":[`+jsonString(song)+`],"category":"Books"}`, nil); code != 400 {
		t.Fatalf("unknown category: %d", code)
	}
	if _, files := m.Status(); len(files) != 0 {
		t.Fatalf("added with an unknown category: %d", len(files))
	}
	if code := call(t, srv, "POST", "/api/v1/downloads", "application/json",
		`{"links":[`+jsonString(song)+`],"category":"music"}`, nil); code != 200 {
		t.Fatalf("add: %d", code)
	}
	const hash = "C0C0456789ABCDEF0123456789ABCDEF"
	if it, ok := items.Get(hash); !ok || it.Kind != library.KindCategory || it.Category != "Music" {
		t.Fatalf("item: %+v %v", it, ok)
	}
	var d RDownload
	call(t, srv, "GET", "/api/v1/downloads/"+hash, "", "", &d)
	if d.Category != "Music" || d.Media {
		t.Fatalf("listed: %+v", d)
	}
	var st RStatus // what the web interface reads
	call(t, srv, "GET", "/api/v1/status", "", "", &st)
	if len(st.Downloads) != 1 || st.Downloads[0].Category != "Music" {
		t.Fatalf("status: %+v", st.Downloads)
	}

	// Changed, then taken off.
	if code := call(t, srv, "POST", "/api/v1/downloads/"+hash+"/category", "application/json", `{"category":"Books"}`, nil); code != 400 {
		t.Fatalf("unknown category set: %d", code)
	}
	if code := call(t, srv, "POST", "/api/v1/downloads/"+hash+"/category", "application/json", `{"category":""}`, nil); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if _, ok := items.Get(hash); ok {
		t.Fatal("still in a category")
	}

	// A movie keeps its own folder.
	call(t, srv, "POST", "/api/v1/media/download", "application/json",
		`{"link":"ed2k://|file|Some.Movie.1999.mkv|20000000|DDDD456789ABCDEF0123456789ABCDEF|/","work_id":"tmdb-movie-1","lang":"es-ES"}`, nil)
	if code := call(t, srv, "POST", "/api/v1/downloads/DDDD456789ABCDEF0123456789ABCDEF/category", "application/json",
		`{"category":"Music"}`, nil); code != 400 {
		t.Fatalf("movie sent to a category: %d", code)
	}
	var movie RDownload
	call(t, srv, "GET", "/api/v1/downloads/DDDD456789ABCDEF0123456789ABCDEF", "", "", &movie)
	if !movie.Media || movie.Category != "" {
		t.Fatalf("movie listed: %+v", movie)
	}
}
