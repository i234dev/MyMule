package arr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mymule/internal/authlimit"
	"mymule/internal/download"
	"mymule/internal/ed2k"
)

// The subset of the qBittorrent Web API v2 used by Radarr, Sonarr,
// Lidarr and Readarr. Username: anything. Password: MyMule's api_key.

const (
	qbitVersion    = "v4.6.7"
	qbitAPIVersion = "2.9.3"
	sessionTTL     = 24 * time.Hour
	infiniteETA    = 8640000 // what qBittorrent reports when unknown
)

func (a *Adapter) qbit() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/auth/login", a.qLogin)
	mux.HandleFunc("POST /api/v2/auth/logout", a.qLogout)
	mux.HandleFunc("/api/v2/app/version", text(qbitVersion))
	mux.HandleFunc("/api/v2/app/webapiVersion", text(qbitAPIVersion))
	mux.HandleFunc("/api/v2/app/buildInfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"qt": "6", "libtorrent": "2", "bitness": 64, "app": "MyMule"})
	})
	mux.HandleFunc("/api/v2/app/preferences", a.qPreferences)
	mux.HandleFunc("POST /api/v2/app/setPreferences", ok)
	mux.HandleFunc("/api/v2/app/defaultSavePath", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, a.cfg.DownloadDir) })
	mux.HandleFunc("/api/v2/torrents/info", a.qInfo)
	mux.HandleFunc("/api/v2/torrents/properties", a.qProperties)
	mux.HandleFunc("/api/v2/torrents/files", a.qFiles)
	mux.HandleFunc("/api/v2/torrents/trackers", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, []any{}) })
	mux.HandleFunc("/api/v2/torrents/categories", a.qCategories)
	// Everything that changes something needs POST, as in qBittorrent:
	// a GET (a link, an image) must never add, delete or pause anything.
	mux.HandleFunc("POST /api/v2/torrents/createCategory", a.qCreateCategory)
	mux.HandleFunc("POST /api/v2/torrents/editCategory", a.qCreateCategory)
	mux.HandleFunc("POST /api/v2/torrents/setCategory", a.qSetCategory)
	mux.HandleFunc("POST /api/v2/torrents/add", a.qAdd)
	mux.HandleFunc("POST /api/v2/torrents/delete", a.qDelete)
	for _, p := range []string{"pause", "stop"} {
		mux.HandleFunc("POST /api/v2/torrents/"+p, a.qPause(true))
	}
	for _, p := range []string{"resume", "start"} {
		mux.HandleFunc("POST /api/v2/torrents/"+p, a.qPause(false))
	}
	// Accepted and ignored: queue priority, share limits, tags, rechecks.
	for _, p := range []string{"topPrio", "bottomPrio", "increasePrio", "decreasePrio", "setForceStart",
		"setShareLimits", "setSuperSeeding", "recheck", "reannounce", "addTags", "removeTags",
		"setAutoManagement", "setLocation", "createTags", "deleteTags"} {
		mux.HandleFunc("POST /api/v2/torrents/"+p, ok)
	}
	mux.HandleFunc("/api/v2/torrents/tags", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, []string{}) })
	mux.HandleFunc("/api/v2/sync/maindata", a.qMaindata)
	mux.HandleFunc("/api/v2/transfer/info", a.qTransfer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/auth/login" && !a.qAuthorized(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func text(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, s) }
}

func ok(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "Ok.") }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- auth ----

func (a *Adapter) qLogin(w http.ResponseWriter, r *http.Request) {
	// The only route open to anyone: read a small form only (Go would
	// otherwise take up to 10 MB per request into memory).
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pass := r.Form.Get("password")
	// A form on another web site is counted apart (see CheckQuiet).
	ok := false
	if authlimit.CrossSite(r) {
		ok = authlimit.Default.CheckQuiet(r, pass, a.cfg.Key)
	} else {
		ok = authlimit.Default.Check(r, pass, a.cfg.Key)
	}
	if !ok {
		io.WriteString(w, "Fails.")
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	sid := hex.EncodeToString(b)
	a.mu.Lock()
	now := time.Now()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[sid] = now.Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: overHTTPS(r)})
	io.WriteString(w, "Ok.")
}

// qLogout ends the session of the request's cookie.
func (a *Adapter) qLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("SID"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "SID", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: overHTTPS(r)})
	io.WriteString(w, "Ok.")
}

// qAuthorized accepts a session cookie from /auth/login, or the API key
// itself (handy for scripts). The cookie only counts when the request
// does not come from a web page of another site, or of another port on
// the same machine (a browser sends the cookie along with those too, as
// the cookie does not depend on the port): the *arr apps send no such
// page's address at all.
func (a *Adapter) qAuthorized(r *http.Request) bool {
	if c, err := r.Cookie("SID"); err == nil && !authlimit.CrossSite(r) && r.Header.Get("Sec-Fetch-Site") != "same-site" {
		a.mu.Lock()
		exp, ok := a.sessions[c.Value]
		a.mu.Unlock()
		if ok && time.Now().Before(exp) {
			return true
		}
	}
	return a.cfg.REST.Authorized(r)
}

// ---- info ----

func (a *Adapter) qPreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"save_path":                a.cfg.DownloadDir,
		"temp_path_enabled":        false,
		"max_ratio_enabled":        false,
		"max_ratio":                -1,
		"max_ratio_act":            0,
		"max_seeding_time_enabled": false,
		"max_seeding_time":         -1,
		"queueing_enabled":         false,
		"dht":                      false,
		"auto_tmm_enabled":         false,
		"create_subfolder_enabled": false,
	})
}

// torrent describes a download the way qBittorrent's torrents/info does.
func (a *Adapter) torrentLocked(f download.FileStatus) map[string]any {
	e := a.store.Downloads[f.Hash.String()]
	if e == nil {
		e = &storeEntry{}
	}
	now := time.Now().Unix()
	progress := 0.0
	if f.Size > 0 {
		progress = float64(f.Done) / float64(f.Size)
	}
	savePath := a.cfg.DownloadDir
	content := filepath.Join(savePath, download.SanitizeName(f.Name))
	state, eta := "stalledDL", int64(infiniteETA)
	completion := int64(-1)
	switch {
	case f.Finished:
		state, eta, progress = "pausedUP", 0, 1
		if f.Path != "" {
			content, savePath = f.Path, filepath.Dir(f.Path)
		}
		if e.Completed == 0 {
			e.Completed = now
			a.store.Downloads[f.Hash.String()] = e
			a.saveLocked()
		}
		completion = e.Completed
	case f.Paused:
		state = "pausedDL"
	case f.Downloading > 0 || f.Speed > 0:
		state = "downloading"
		if f.Speed > 512 {
			eta = int64(float64(f.Size-f.Done) / f.Speed)
		}
	}
	added := e.Added
	if added == 0 {
		added = now
	}
	return map[string]any{
		"hash":               a.infoHashLocked(f),
		"name":               f.Name,
		"size":               f.Size,
		"total_size":         f.Size,
		"progress":           progress,
		"downloaded":         f.Done,
		"amount_left":        f.Size - f.Done,
		"completed":          f.Done,
		"dlspeed":            int64(f.Speed),
		"upspeed":            0,
		"eta":                eta,
		"state":              state,
		"category":           e.Category,
		"tags":               "",
		"save_path":          savePath,
		"content_path":       content,
		"added_on":           added,
		"completion_on":      completion,
		"num_seeds":          f.Downloading,
		"num_complete":       f.Sources,
		"num_leechs":         0,
		"num_incomplete":     0,
		"ratio":              0,
		"ratio_limit":        -2,
		"seeding_time":       0,
		"seeding_time_limit": -2,
		"priority":           0,
		"force_start":        false,
		"auto_tmm":           false,
		"super_seeding":      false,
		"dl_limit":           -1,
		"up_limit":           -1,
		"availability":       -1,
		"tracker":            "",
		"magnet_uri":         "",
		"last_activity":      now,
	}
}

func (a *Adapter) qInfo(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	cat, hasCat := r.Form["category"]
	var want map[string]bool
	if h := r.Form.Get("hashes"); h != "" {
		want = map[string]bool{}
		for _, x := range strings.Split(strings.ToLower(h), "|") {
			want[x] = true
		}
	}
	_, files := a.cfg.Manager.Status()
	out := []map[string]any{}
	a.mu.Lock()
	for _, f := range files {
		t := a.torrentLocked(f)
		if hasCat && t["category"] != cat[0] {
			continue
		}
		if want != nil && !want[t["hash"].(string)] {
			continue
		}
		out = append(out, t)
	}
	a.mu.Unlock()
	writeJSON(w, out)
}

func (a *Adapter) one(w http.ResponseWriter, r *http.Request) (download.FileStatus, bool) {
	r.ParseForm()
	files := a.findByInfoHash(r.Form.Get("hash"))
	if len(files) != 1 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return download.FileStatus{}, false
	}
	return files[0], true
}

func (a *Adapter) qProperties(w http.ResponseWriter, r *http.Request) {
	f, found := a.one(w, r)
	if !found {
		return
	}
	a.mu.Lock()
	t := a.torrentLocked(f)
	a.mu.Unlock()
	writeJSON(w, map[string]any{
		"save_path": t["save_path"], "total_size": f.Size, "total_downloaded": f.Done, "dl_speed": t["dlspeed"],
		"eta": t["eta"], "addition_date": t["added_on"], "completion_date": t["completion_on"],
		"seeding_time": 0, "share_ratio": 0, "piece_size": pieceLength, "pieces_num": (f.Size + pieceLength - 1) / pieceLength,
		"seeds": f.Downloading, "seeds_total": f.Sources, "peers": 0, "peers_total": 0, "comment": "",
	})
}

func (a *Adapter) qFiles(w http.ResponseWriter, r *http.Request) {
	f, found := a.one(w, r)
	if !found {
		return
	}
	progress := 0.0
	if f.Size > 0 {
		progress = float64(f.Done) / float64(f.Size)
	}
	if f.Finished {
		progress = 1
	}
	writeJSON(w, []map[string]any{{"index": 0, "name": download.SanitizeName(f.Name), "size": f.Size,
		"progress": progress, "priority": 1, "is_seed": f.Finished, "availability": -1}})
}

// ---- categories ----

func (a *Adapter) qCategories(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	out := map[string]any{}
	for name, path := range a.store.Categories {
		out[name] = map[string]string{"name": name, "savePath": path}
	}
	a.mu.Unlock()
	writeJSON(w, out)
}

func (a *Adapter) qCreateCategory(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.Form.Get("category"))
	if name == "" {
		http.Error(w, "Category name is empty", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.store.Categories[name] = r.Form.Get("savePath")
	a.saveLocked()
	a.mu.Unlock()
	io.WriteString(w, "Ok.")
}

func (a *Adapter) qSetCategory(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	cat := r.Form.Get("category")
	files := a.findByInfoHash(r.Form.Get("hashes"))
	a.mu.Lock()
	for _, f := range files {
		a.entryLocked(f.Hash).Category = cat
	}
	a.saveLocked()
	a.mu.Unlock()
	io.WriteString(w, "Ok.")
}

func (a *Adapter) entryLocked(h ed2k.Hash) *storeEntry {
	e := a.store.Downloads[h.String()]
	if e == nil {
		e = &storeEntry{Added: time.Now().Unix()}
		a.store.Downloads[h.String()] = e
	}
	return e
}

// ---- add / delete / pause ----

// Limits for torrents added through the qBittorrent API.
const (
	maxAddBody     = 10 << 20 // a whole add request
	maxLoginBody   = 4096     // a login form
	maxTorrentSize = 1 << 20  // one torrent file
)

// qAdd accepts stand-in .torrent files (field "torrents") and URLs
// (field "urls", one per line): ed2k:// links or MyMule's own
// /torznab/download URLs.
func (a *Adapter) qAdd(w http.ResponseWriter, r *http.Request) {
	// Stand-in torrents are a few KB: cap the whole request, so a client
	// cannot fill the disk with temporary upload files.
	r.Body = http.MaxBytesReader(w, r.Body, maxAddBody)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		r.ParseForm()
	}
	var links []ed2k.Link
	var problems []string
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["torrents"] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(io.LimitReader(f, maxTorrentSize+1))
			f.Close()
			if len(data) > maxTorrentSize {
				problems = append(problems, fh.Filename+": too big for a MyMule torrent")
				continue
			}
			l, err := LinkFromTorrent(data)
			if err != nil {
				problems = append(problems, fh.Filename+": "+err.Error())
				continue
			}
			links = append(links, l)
		}
	}
	for _, u := range strings.Split(r.FormValue("urls"), "\n") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		l, err := linkFromURL(u)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		links = append(links, l)
	}

	cat := r.FormValue("category")
	paused := r.FormValue("paused") == "true" || r.FormValue("stopped") == "true"
	added := 0
	for _, l := range links {
		err := a.cfg.Manager.AddLink(l)
		if err != nil && !errors.Is(err, download.ErrExists) {
			problems = append(problems, l.Name+": "+err.Error())
			continue
		}
		added++
		a.mu.Lock()
		e := a.entryLocked(l.Hash)
		if cat != "" {
			e.Category = cat
		}
		a.saveLocked()
		a.mu.Unlock()
		if paused {
			a.cfg.Manager.SetPaused(l.Hash, true)
		}
		a.cfg.Log("added %s (category %q) from the qBittorrent API", l.Name, cat)
	}
	for _, p := range problems {
		a.cfg.Log("qBittorrent API: not added: %s", p)
	}
	if added == 0 {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		io.WriteString(w, "Fails.")
		return
	}
	io.WriteString(w, "Ok.")
}

func linkFromURL(s string) (ed2k.Link, error) {
	if strings.HasPrefix(strings.ToLower(s), "ed2k://") {
		return ed2k.ParseLink(s)
	}
	u, err := url.Parse(s)
	if err != nil || !strings.Contains(u.Path, "/torznab/download/") {
		return ed2k.Link{}, fmt.Errorf("unsupported URL (only ed2k:// links and MyMule's own results): %s", s)
	}
	return linkFromDownloadURL(u)
}

func (a *Adapter) qDelete(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	deleteFiles := r.Form.Get("deleteFiles") == "true"
	for _, f := range a.findByInfoHash(r.Form.Get("hashes")) {
		a.cfg.Manager.Cancel(f.Hash) // partial data is always deleted
		if f.Finished && deleteFiles && f.Path != "" && a.insideDownloads(f.Path) {
			os.Remove(f.Path)
		}
		a.mu.Lock()
		delete(a.store.Downloads, f.Hash.String())
		delete(a.hashes, f.Hash)
		a.saveLocked()
		a.mu.Unlock()
	}
	io.WriteString(w, "Ok.")
}

// insideDownloads guards file deletion: only files directly inside the
// download folder may be removed through the API.
func (a *Adapter) insideDownloads(p string) bool {
	dir, err1 := filepath.Abs(a.cfg.DownloadDir)
	file, err2 := filepath.Abs(p)
	return err1 == nil && err2 == nil && a.cfg.DownloadDir != "" &&
		strings.EqualFold(filepath.Clean(filepath.Dir(file)), filepath.Clean(dir))
}

func (a *Adapter) qPause(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		for _, f := range a.findByInfoHash(r.Form.Get("hashes")) {
			if !f.Finished {
				a.cfg.Manager.SetPaused(f.Hash, paused)
			}
		}
		io.WriteString(w, "Ok.")
	}
}

// ---- sync / transfer ----

func (a *Adapter) qMaindata(w http.ResponseWriter, r *http.Request) {
	_, files := a.cfg.Manager.Status()
	torrents := map[string]any{}
	var speed float64
	a.mu.Lock()
	for _, f := range files {
		t := a.torrentLocked(f)
		torrents[t["hash"].(string)] = t
		speed += f.Speed
	}
	cats := map[string]any{}
	for name, path := range a.store.Categories {
		cats[name] = map[string]string{"name": name, "savePath": path}
	}
	a.mu.Unlock()
	writeJSON(w, map[string]any{
		"rid": 1, "full_update": true, "torrents": torrents, "categories": cats,
		"server_state": map[string]any{"dl_info_speed": int64(speed), "up_info_speed": 0, "connection_status": "connected"},
	})
}

func (a *Adapter) qTransfer(w http.ResponseWriter, r *http.Request) {
	_, files := a.cfg.Manager.Status()
	var speed float64
	for _, f := range files {
		speed += f.Speed
	}
	writeJSON(w, map[string]any{"dl_info_speed": int64(speed), "up_info_speed": 0, "connection_status": "connected"})
}

// overHTTPS reports whether the client reached us over HTTPS, directly or
// through a reverse proxy that says so: then the SID cookie is Secure, so
// that the client never sends it over plain HTTP. (Trusting the proxy's
// header can only make the cookie stricter.) As the web interface's.
func overHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
