package arr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/api"
	"mymule/internal/download"
	"mymule/internal/ed2k"
)

// adapterOn starts an adapter with an empty download list on the given
// state file, as MyMule after a restart.
func adapterOn(t *testing.T, store, downloads string) (*Adapter, string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp, IncomingDir: downloads, SkipFreeSpaceCheck: true})
	rest := api.NewREST(m, testKey, "test")
	a := New(Config{Manager: m, REST: rest, Key: testKey, DownloadDir: downloads, StorePath: store})
	mux := http.NewServeMux()
	a.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, srv.URL + "/api/v2"
}

// finish records a finished download as the adapter sees it when an app
// polls torrents/info while the download list still holds it.
func finish(a *Adapter, f download.FileStatus, category string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if category != "" {
		a.entryLocked(f.Hash).Category = category
	}
	a.torrentLocked(f)
}

func infoOf(t *testing.T, u string) []map[string]any {
	t.Helper()
	_, js := get(t, http.DefaultClient, u)
	var info []map[string]any
	if err := json.Unmarshal([]byte(js), &info); err != nil {
		t.Fatalf("%s: %v", js, err)
	}
	return info
}

// A finished download of an app is still reported after a restart, until
// the app deletes it: otherwise the app never imports it.
func TestFinishedDownloadsSurviveRestart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "arr.json")
	downloads := t.TempDir()
	link := ed2k.Link{Name: "Example Show 1x02 (demo).mkv", Size: 12, Hash: ed2k.Hash{7}}
	path := filepath.Join(downloads, link.Name)
	os.WriteFile(path, []byte("episode data"), 0o644)
	done := download.FileStatus{Hash: link.Hash, Name: link.Name, Size: link.Size, Done: link.Size, Finished: true, Path: path}
	other := download.FileStatus{Hash: ed2k.Hash{8}, Name: "Added by hand (demo).mkv", Size: 5, Done: 5, Finished: true,
		Path: filepath.Join(downloads, "Added by hand (demo).mkv")}

	before, _ := adapterOn(t, store, downloads)
	finish(before, done, "sonarr")
	finish(before, other, "") // not an app's download: not remembered

	_, api2 := adapterOn(t, store, downloads) // the restart
	key := "&apikey=" + testKey
	info := infoOf(t, api2+"/torrents/info?category=sonarr"+key)
	if len(info) != 1 {
		t.Fatalf("after restart: %+v", info)
	}
	ih := InfoHash(link)
	if info[0]["hash"] != ih || info[0]["state"] != "pausedUP" || info[0]["progress"] != 1.0 ||
		info[0]["content_path"] != path || info[0]["save_path"] != downloads || info[0]["category"] != "sonarr" {
		t.Fatalf("remembered torrent: %+v", info[0])
	}
	if all := infoOf(t, api2+"/torrents/info?apikey="+testKey); len(all) != 1 {
		t.Fatalf("a download not from an app is shown: %+v", all)
	}
	if code, files := get(t, http.DefaultClient, api2+"/torrents/files?hash="+ih+key); code != 200 || files == "" {
		t.Fatalf("files: %d %s", code, files)
	}

	// The app imports it and deletes it, with its file.
	if code, body := post(t, http.DefaultClient, api2+"/torrents/delete?apikey="+testKey,
		url.Values{"hashes": {ih}, "deleteFiles": {"true"}}); code != 200 || body != "Ok." {
		t.Fatalf("delete: %d %s", code, body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file not deleted: %v", err)
	}
	if info := infoOf(t, api2+"/torrents/info?apikey="+testKey); len(info) != 0 {
		t.Fatalf("after delete: %+v", info)
	}
	_, api3 := adapterOn(t, store, downloads)
	if info := infoOf(t, api3+"/torrents/info?apikey="+testKey); len(info) != 0 {
		t.Fatalf("deleted download back after another restart: %+v", info)
	}
}

// A download still in the list is shown once, not also as remembered.
func TestFinishedDownloadNotDuplicated(t *testing.T) {
	fx := newFixture(t)
	link := ed2k.Link{Name: "Example Movie 2024 1080p (demo).mkv", Size: 2_000_000_000, Hash: ed2k.Hash{3}}
	if err := fx.m.AddLink(link); err != nil {
		t.Fatal(err)
	}
	fx.a.mu.Lock()
	e := fx.a.entryLocked(link.Hash)
	e.Category, e.Completed, e.Name, e.Size, e.Path = "radarr", time.Now().Unix(), link.Name, link.Size, filepath.Join(fx.downloads, link.Name)
	fx.a.mu.Unlock()
	if info := infoOf(t, fx.srv.URL+"/api/v2/torrents/info?apikey="+testKey); len(info) != 1 {
		t.Fatalf("shown %d times: %+v", len(info), info)
	}
}

// Remembered downloads are forgotten after rememberFinished, so the state
// file stays small if an app never deletes them.
func TestRememberedDownloadsExpire(t *testing.T) {
	a, _ := adapterOn(t, filepath.Join(t.TempDir(), "arr.json"), t.TempDir())
	finish(a, download.FileStatus{Hash: ed2k.Hash{9}, Name: "old.mkv", Size: 1, Done: 1, Finished: true, Path: "/x/old.mkv"}, "radarr")
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.rememberedLocked(nil, time.Now()); len(got) != 1 {
		t.Fatalf("fresh: %+v", got)
	}
	if got := a.rememberedLocked(nil, time.Now().Add(rememberFinished+time.Hour)); len(got) != 0 {
		t.Fatalf("expired still shown: %+v", got)
	}
	if _, ok := a.store.Downloads[ed2k.Hash{9}.String()]; ok {
		t.Fatal("expired entry kept in the state file")
	}
}
