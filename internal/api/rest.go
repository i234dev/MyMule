package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mymule/internal/authlimit"
	"mymule/internal/config"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/library"
	"mymule/internal/meta"
	"mymule/internal/prefs"
	"mymule/internal/ratings"
	"mymule/internal/server"
	"mymule/internal/share"
	"mymule/internal/stats"
	"mymule/internal/update"
	"mymule/internal/upload"
	"mymule/internal/wanted"
)

// REST is the public HTTP API for scripts and other programs (PHP, cron,
// home automation...). Unlike the CLI control API it has a fixed address
// and a user-visible key, and a stable JSON format documented in
// docs/API.md. Every request must carry the key.
type REST struct {
	m           *download.Manager
	key         string
	version     string
	mux         *http.ServeMux
	also        func(*http.Request) bool // extra check, e.g. the web interface session
	newer       func() *update.Release   // a newer MyMule that is out (see EnableUpdates)
	media       meta.Source              // movie/series lookups; nil = disabled
	prefs       *prefs.Store             // web preferences; nil = disabled
	items       *library.Store           // downloads that are movies or episodes
	onPrefs     func()                   // called after preferences change
	servers     ServerControl            // server list and switching; nil = disabled
	folderCheck config.FolderCheck       // new folders in mymule.conf; nil = refused
	wanted      *wanted.Store            // the wish list; nil = disabled
	nudge       chan struct{}            // wakes RunWanted
	logf        func(string, ...any)     // MyMule's log (wish list)
	// OnWantedFound is told when the wish list starts a download.
	OnWantedFound func(title, file string)
	// testSearch replaces the eD2K searches of movies and series in tests.
	testSearch func(context.Context, server.Query) ([]SearchResult, error)
	notes      notesCache                          // what users say about files
	ratings    *ratings.Store                      // the user's ratings of files; nil = disabled
	testNotes  func(ed2k.Hash) ([]kad.Note, error) // replaces Kad in tests
}

// AlsoAllow accepts requests for which f returns true, in addition to the
// API key. The web interface uses it for its session cookie.
func (r *REST) AlsoAllow(f func(*http.Request) bool) { r.also = f }

// EnableUpdates says in the status (update) whether a newer MyMule is
// out, as newer tells.
func (r *REST) EnableUpdates(newer func() *update.Release) { r.newer = newer }

// EnableStartLink adds POST /api/v1/web/link: a one-time login link for
// the web interface (see web.UI.StartLink). A second copy of MyMule
// started with a double-click asks the running one for it.
func (r *REST) EnableStartLink(link func() string) {
	r.mux.HandleFunc("POST /api/v1/web/link", func(w http.ResponseWriter, req *http.Request) {
		reply(w, map[string]string{"path": link()})
	})
}

// NewREST builds the API handler. key must not be empty.
func NewREST(m *download.Manager, key, version string) *REST {
	r := &REST{m: m, key: key, version: version, mux: http.NewServeMux()}
	r.mux.HandleFunc("GET /api/v1/version", r.getVersion)
	r.mux.HandleFunc("GET /api/v1/status", r.getStatus)
	r.mux.HandleFunc("GET /api/v1/downloads", r.listDownloads)
	r.mux.HandleFunc("POST /api/v1/downloads", r.addDownloads)
	r.mux.HandleFunc("POST /api/v1/downloads/clear", r.clearFinished)
	r.mux.HandleFunc("GET /api/v1/downloads/{hash}", r.getDownload)
	r.mux.HandleFunc("POST /api/v1/downloads/collection", r.addCollection)
	r.mux.HandleFunc("POST /api/v1/downloads/{hash}/pause", r.pause(true))
	r.mux.HandleFunc("POST /api/v1/downloads/{hash}/priority", r.setPriority)
	r.mux.HandleFunc("POST /api/v1/downloads/{hash}/preview", r.setPreview)
	r.mux.HandleFunc("POST /api/v1/downloads/{hash}/resume", r.pause(false))
	r.mux.HandleFunc("DELETE /api/v1/downloads/{hash}", r.cancel)
	r.mux.HandleFunc("GET /api/v1/search", r.search)
	r.mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, req *http.Request) {
		fail(w, http.StatusNotFound, errors.New("unknown endpoint; see docs/API.md"))
	})
	return r
}

// Authorized reports whether req carries the API key, in the X-Api-Key
// header, as a bearer token, or as the apikey query parameter.
func (r *REST) Authorized(req *http.Request) bool {
	got := req.Header.Get("X-Api-Key")
	if got == "" {
		// Only a bearer token is a key. Other schemes (Basic, sent by a
		// password-protected reverse proxy in front of MyMule) are not
		// meant for us and must not count as wrong keys.
		if a := req.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
			got = strings.TrimSpace(a[7:])
		}
	}
	if got == "" {
		got = req.URL.Query().Get("apikey")
	}
	// The web interface's session first: it is already proven, and a
	// key sent alongside it must not count as a wrong one.
	if r.also != nil && r.also(req) {
		return true
	}
	if got == "" || r.key == "" {
		return false
	}
	if authlimit.CrossSite(req) {
		return authlimit.Default.CheckQuiet(req, got, r.key)
	}
	// A key was sent: a wrong one counts against the guessing limit.
	return authlimit.Default.Check(req, got, r.key)
}

func (r *REST) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.Authorized(req) {
		fail(w, http.StatusUnauthorized, errors.New("missing or wrong API key (X-Api-Key header or ?apikey=)"))
		return
	}
	r.mux.ServeHTTP(w, req)
}

// ---- JSON format (stable, snake_case) ----

// RDownload is a download as returned by the REST API.
type RDownload struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	Done        int64   `json:"done"`
	Progress    float64 `json:"progress"` // 0..1
	Speed       float64 `json:"speed"`    // bytes/s
	ETA         int64   `json:"eta"`      // seconds, -1 = unknown
	Sources     int     `json:"sources"`
	Downloading int     `json:"downloading_from"`
	Queued      int     `json:"queued_at"`
	BestRank    int     `json:"best_queue_rank,omitempty"`
	State       string  `json:"state"` // downloading, queued, stalled, paused, finished
	Path        string  `json:"path,omitempty"`
	Link        string  `json:"link"`
	Priority    string  `json:"priority"`           // low, normal or high
	Preview     bool    `json:"preview"`            // first and last parts first
	Category    string  `json:"category,omitempty"` // the category it goes to
	Media       bool    `json:"media,omitempty"`    // a movie or episode (goes to the library)
}

// priorityNames are the API's names of download priorities.
var priorityNames = map[int]string{download.Low: "low", download.Normal: "normal", download.High: "high"}

// ParsePriority reads a priority name (low, normal, high).
func ParsePriority(s string) (int, error) {
	for p, n := range priorityNames {
		if strings.EqualFold(s, n) {
			return p, nil
		}
	}
	return 0, errors.New("priority must be low, normal or high")
}

// RServer describes the server connection.
type RServer struct {
	Connected bool   `json:"connected"`
	Name      string `json:"name,omitempty"`
	Address   string `json:"address,omitempty"`
	HighID    bool   `json:"high_id"`
	Port      uint16 `json:"port"`
}

// RStatus is the reply to GET /api/v1/status.
type RStatus struct {
	Version       string          `json:"version"`
	Server        RServer         `json:"server"`
	DownloadSpeed float64         `json:"download_speed"`
	Downloads     []RDownload     `json:"downloads"`
	Upload        *upload.Status  `json:"upload,omitempty"`
	Shared        *share.Status   `json:"shared,omitempty"`
	Kad           *KadInfo        `json:"kad,omitempty"`
	Totals        *stats.Totals   `json:"totals,omitempty"` // all time
	Router        *RouterInfo     `json:"router,omitempty"` // ports opened on the router; absent: upnp off
	Update        *update.Release `json:"update,omitempty"` // a newer MyMule that is out
}

// ToRDownload converts a manager status to the REST format.
func ToRDownload(f download.FileStatus) RDownload {
	d := RDownload{
		Hash: f.Hash.String(), Name: f.Name, Size: f.Size, Done: f.Done, Speed: f.Speed, ETA: -1,
		Sources: f.Sources, Downloading: f.Downloading, Queued: f.Queued, BestRank: f.BestRank,
		Path: f.Path, Link: ed2k.Link{Name: f.Name, Size: f.Size, Hash: f.Hash}.String(),
		Priority: priorityNames[f.Priority], Preview: f.Preview,
	}
	if f.Size > 0 {
		d.Progress = float64(f.Done) / float64(f.Size)
	}
	switch {
	case f.Finished:
		d.State, d.Progress, d.ETA = "finished", 1, 0
	case f.Paused:
		d.State = "paused"
	case f.Downloading > 0 || f.Speed > 0:
		d.State = "downloading"
		if f.Speed > 512 {
			d.ETA = int64(float64(f.Size-f.Done) / f.Speed)
		}
	case f.Queued > 0:
		d.State = "queued"
	default:
		d.State = "stalled"
	}
	return d
}

func (r *REST) getVersion(w http.ResponseWriter, req *http.Request) {
	reply(w, map[string]string{"name": "MyMule", "version": r.version})
}

func (r *REST) getStatus(w http.ResponseWriter, req *http.Request) {
	ss, files := r.m.Status()
	st := RStatus{
		Version:   r.version,
		Server:    RServer{Connected: ss.Connected, Name: ss.Name, Address: ss.Addr, HighID: ss.HighID, Port: ss.Port},
		Downloads: []RDownload{},
	}
	for _, f := range files {
		st.Downloads = append(st.Downloads, r.withCategory(ToRDownload(f)))
		st.DownloadSpeed += f.Speed
	}
	st.Upload, st.Shared = r.m.SharingStatus()
	st.Kad = KadInfoOf(r.m.KadStatus(), r.m.KadProblem())
	st.Totals = r.m.Totals()
	st.Router = RouterInfoOf(r.m.Router())
	if r.newer != nil {
		st.Update = r.newer()
	}
	reply(w, st)
}

func (r *REST) listDownloads(w http.ResponseWriter, req *http.Request) {
	_, files := r.m.Status()
	out := []RDownload{}
	for _, f := range files {
		out = append(out, r.withCategory(ToRDownload(f)))
	}
	reply(w, map[string]any{"downloads": out})
}

func (r *REST) find(w http.ResponseWriter, req *http.Request) (download.FileStatus, bool) {
	h, err := ed2k.ParseHash(req.PathValue("hash"))
	if err != nil {
		fail(w, http.StatusBadRequest, errors.New("hash must be the 32-character ed2k hash"))
		return download.FileStatus{}, false
	}
	_, files := r.m.Status()
	for _, f := range files {
		if f.Hash == h {
			return f, true
		}
	}
	fail(w, http.StatusNotFound, errors.New("no download with that hash"))
	return download.FileStatus{}, false
}

func (r *REST) getDownload(w http.ResponseWriter, req *http.Request) {
	if f, ok := r.find(w, req); ok {
		reply(w, r.withCategory(ToRDownload(f)))
	}
}

// addRequest accepts JSON ({"links": [...], "sources": [...]}) or a form
// field "link" (repeatable), which is the easiest from PHP or curl -d.
// Category (optional) is where the files go when they finish.
type addRequest struct {
	Links    []string `json:"links"`
	Link     string   `json:"link"`
	Sources  []string `json:"sources"`
	Category string   `json:"category"`
}

func (r *REST) addDownloads(w http.ResponseWriter, req *http.Request) {
	var ar addRequest
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if !decode(w, req, &ar) {
			return
		}
	} else {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		if err := req.ParseForm(); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		ar.Links = req.PostForm["link"]
		ar.Sources = req.PostForm["source"]
		ar.Category = req.PostForm.Get("category")
	}
	if ar.Link != "" {
		ar.Links = append(ar.Links, ar.Link)
	}
	if len(ar.Links) == 0 {
		fail(w, http.StatusBadRequest, errors.New(`send {"links": ["ed2k://|file|...|/"]} or a form field "link"`))
		return
	}
	cat, err := r.category(ar.Category)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	resp := AddResponse{Added: []string{}}
	for _, l := range ar.Links {
		link, err := ed2k.ParseLink(l)
		if err == nil {
			err = r.m.AddLink(link, ar.Sources...)
		}
		if err == nil && cat != "" {
			err = r.setItemCategory(link.Hash, cat)
		}
		if err != nil {
			name := link.Name
			if name == "" {
				name = l
			}
			resp.Errors = append(resp.Errors, name+": "+err.Error())
			continue
		}
		resp.Added = append(resp.Added, link.Name)
	}
	if len(resp.Added) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(resp)
		return
	}
	reply(w, resp)
}

// addCollection adds the files of an eMule collection: the body is the
// .emulecollection file (or text with ed2k:// links); ?category= sends
// them to a category.
func (r *REST) addCollection(w http.ResponseWriter, req *http.Request) {
	cat, err := r.category(req.URL.Query().Get("category"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, ed2k.MaxCollectionSize))
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	name, links, err := ed2k.ParseCollection(body)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	resp := AddResponse{Added: []string{}, Collection: name}
	for _, link := range links {
		err := r.m.AddLink(link)
		if err == nil && cat != "" {
			err = r.setItemCategory(link.Hash, cat)
		}
		if err != nil {
			resp.Errors = append(resp.Errors, link.Name+": "+err.Error())
			continue
		}
		resp.Added = append(resp.Added, link.Name)
	}
	reply(w, resp)
}

func (r *REST) pause(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		f, ok := r.find(w, req)
		if !ok {
			return
		}
		name, err := r.m.SetPaused(f.Hash, paused)
		if err != nil {
			fail(w, errCode(err), err)
			return
		}
		reply(w, map[string]any{"ok": true, "name": name})
	}
}

// setPriority: {"priority": "high"}.
func (r *REST) setPriority(w http.ResponseWriter, req *http.Request) {
	f, ok := r.find(w, req)
	if !ok {
		return
	}
	var body struct {
		Priority string `json:"priority"`
	}
	if !decode(w, req, &body) {
		return
	}
	p, err := ParsePriority(body.Priority)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	name, err := r.m.SetPriority(f.Hash, p)
	if err != nil {
		fail(w, errCode(err), err)
		return
	}
	reply(w, map[string]any{"ok": true, "name": name})
}

// setPreview: {"preview": true}.
func (r *REST) setPreview(w http.ResponseWriter, req *http.Request) {
	f, ok := r.find(w, req)
	if !ok {
		return
	}
	var body struct {
		Preview bool `json:"preview"`
	}
	if !decode(w, req, &body) {
		return
	}
	name, err := r.m.SetPreview(f.Hash, body.Preview)
	if err != nil {
		fail(w, errCode(err), err)
		return
	}
	reply(w, map[string]any{"ok": true, "name": name})
}

func (r *REST) cancel(w http.ResponseWriter, req *http.Request) {
	f, ok := r.find(w, req)
	if !ok {
		return
	}
	name, err := r.m.Cancel(f.Hash)
	if err != nil {
		fail(w, errCode(err), err)
		return
	}
	reply(w, map[string]any{"ok": true, "name": name, "data_deleted": !f.Finished})
}

func (r *REST) clearFinished(w http.ResponseWriter, req *http.Request) {
	reply(w, map[string]int{"removed": r.m.ClearFinished()})
}

func (r *REST) search(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		fail(w, http.StatusBadRequest, errors.New("missing q"))
		return
	}
	ftype, err := server.ParseFileType(q.Get("type"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	sq := server.Query{Keywords: query, Type: ftype, Extension: q.Get("ext")}
	if sq.MinSize, err = parseUint(q.Get("min")); err == nil {
		sq.MaxSize, err = parseUint(q.Get("max"))
	}
	if err != nil {
		fail(w, http.StatusBadRequest, errors.New("min and max are sizes in bytes"))
		return
	}
	global := q.Get("global") != "false" && q.Get("global") != "0"
	results, err := r.Search(req.Context(), sq, global)
	if err != nil {
		fail(w, errCode(err), err)
		return
	}
	if limit, err := strconv.Atoi(q.Get("limit")); err == nil && limit > 0 && limit < len(results) {
		results = results[:limit]
	}
	reply(w, SearchResponse{Results: results})
}

func parseUint(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// Search runs a search (all servers when global) and returns results
// merged by hash, most sources first. Shared with the Torznab adapter.
func (r *REST) Search(ctx context.Context, q server.Query, global bool) ([]SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var res []server.Result
	var err error
	if global {
		res, _, err = r.m.SearchGlobal(ctx, q, 6*time.Second)
	} else {
		res, err = r.m.Search(ctx, q, 1)
	}
	if err != nil {
		return nil, err
	}
	return toSearchResults(res), nil
}
