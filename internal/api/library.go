package api

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/library"
	"mymule/internal/meta"
	"mymule/internal/prefs"
	"mymule/internal/release"
)

// Preferences and movie/series downloads that get organised into folders.
// Enabled together with the web interface.

// EnableLibrary adds the preferences endpoints and "download as a movie or
// episode". onChange is called after preferences are saved.
func (r *REST) EnableLibrary(p *prefs.Store, items *library.Store, onChange func()) {
	r.prefs, r.items, r.onPrefs = p, items, onChange
	r.mux.HandleFunc("GET /api/v1/preferences", r.getPrefs)
	r.mux.HandleFunc("PUT /api/v1/preferences", r.putPrefs)
	r.mux.HandleFunc("POST /api/v1/preferences/preview", r.previewPrefs)
	r.mux.HandleFunc("POST /api/v1/media/download", r.mediaDownload)
}

func (r *REST) getPrefs(w http.ResponseWriter, req *http.Request) {
	reply(w, map[string]any{
		"preferences": r.prefs.Get(),
		"presets":     map[string][]string{"movie": library.MoviePresets, "series": library.SeriesPresets},
	})
}

func (r *REST) putPrefs(w http.ResponseWriter, req *http.Request) {
	// Start from the current preferences: a request may send only one
	// section (e.g. {"library": {...}}).
	before := r.prefs.Get()
	p := before
	if !decode(w, req, &p) {
		return
	}
	// The preferred server is chosen from the known list (as in the
	// server endpoints), not any address a client sends.
	if p.Server != "" && p.Server != before.Server && r.servers != nil {
		if err := r.knownServer(p.Server); err != nil {
			fail(w, http.StatusBadRequest, errors.New("server: "+err.Error()))
			return
		}
	}
	if err := r.prefs.Set(p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if r.onPrefs != nil {
		r.onPrefs()
	}
	reply(w, map[string]any{"preferences": r.prefs.Get()})
}

// Example items for previews (made-up titles).
var (
	exampleMovie = library.Item{Kind: "movie", Title: "La Película de Ejemplo", OriginalTitle: "The Example Movie",
		Year: 1999, Quality: "1080p", Language: "ES"}
	exampleEpisode = library.Item{Kind: "series", Title: "La Serie de Ejemplo", OriginalTitle: "The Example Show",
		Year: 2005, Season: 1, Episode: 2, EpisodeTitle: "El Segundo Episodio", Quality: "720p", Language: "ES"}
)

// previewPrefs shows where an example movie and episode would go.
func (r *REST) previewPrefs(w http.ResponseWriter, req *http.Request) {
	var p library.Settings
	if !decode(w, req, &p) {
		return
	}
	out := map[string]string{}
	if err := library.CheckTemplate(p.MovieTemplate, false); err != nil {
		out["movie_error"] = err.Error()
	} else {
		out["movie"] = library.Render(p.MovieTemplate, exampleMovie, p.OriginalTitles) + ".mkv"
	}
	if err := library.CheckTemplate(p.SeriesTemplate, true); err != nil {
		out["series_error"] = err.Error()
	} else {
		out["series"] = library.Render(p.SeriesTemplate, exampleEpisode, p.OriginalTitles) + ".mkv"
	}
	reply(w, out)
}

type mediaDownloadRequest struct {
	Link    string `json:"link"`
	WorkID  string `json:"work_id"`
	Lang    string `json:"lang"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

// mediaDownload adds a file found for a movie or series and remembers
// what it is, so it can be renamed and moved when it finishes.
func (r *REST) mediaDownload(w http.ResponseWriter, req *http.Request) {
	var mr mediaDownloadRequest
	if !decode(w, req, &mr) {
		return
	}
	link, err := ed2k.ParseLink(mr.Link)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if mr.Lang == "" {
		mr.Lang = "en"
	}
	work, err := r.media.Get(req.Context(), mr.WorkID, mr.Lang)
	if err != nil {
		fail(w, http.StatusBadGateway, errors.New("movie database: "+err.Error()))
		return
	}
	info := release.Parse(link.Name)
	it := library.Item{Kind: work.Kind, Title: work.Title, OriginalTitle: work.OriginalTitle, Year: work.Year,
		Quality: info.Resolution, WorkID: work.ID}
	var langs []string
	for _, l := range info.Languages {
		langs = append(langs, strings.ToUpper(l))
	}
	it.Language = strings.Join(langs, "+")
	if work.Kind == meta.Series {
		// The file name says best which episode it is.
		it.Season, it.Episode = info.Season, info.Episode
		if it.Season == 0 {
			it.Season = mr.Season
		}
		if it.Episode == 0 && info.Season == 0 {
			it.Episode = mr.Episode
		}
		if et, ok := r.media.(meta.EpisodeTitler); ok && it.Season > 0 && it.Episode > 0 {
			it.EpisodeTitle, _ = et.EpisodeTitle(req.Context(), work.ID, it.Season, it.Episode, mr.Lang)
		}
	}
	if err := r.m.AddLink(link); err != nil {
		fail(w, errCode(err), err)
		return
	}
	r.items.Put(link.Hash.String(), it)
	resp := mediaDownloadResponse{Added: []string{link.Name}}
	// Tell the user now what will happen to the file when it finishes.
	const downloads = "downloads"
	if base, rel, ok := library.Plan(r.prefs.Library(), it, filepath.Join(downloads, download.SanitizeName(link.Name))); ok {
		resp.Organize, resp.File = true, filepath.ToSlash(rel)
		if base != downloads {
			resp.Folder = base
		}
	}
	reply(w, resp)
}

// mediaDownloadResponse says what was added and where it will go: File
// (under Folder, or renamed inside the download folder when Folder is
// empty), or nothing when Organize is false.
type mediaDownloadResponse struct {
	Added    []string `json:"added"`
	Organize bool     `json:"organize"`
	Folder   string   `json:"folder,omitempty"`
	File     string   `json:"file,omitempty"`
}
