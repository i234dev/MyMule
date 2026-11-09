package api

import (
	"errors"
	"net/http"

	"mymule/internal/config"
)

// EnableSettings adds the endpoints that show and change mymule.conf
// (the web interface's advanced settings). Changes take effect when
// MyMule restarts; who can reach the API, its key and the network
// folders allowed stay for mymule.conf itself. check judges new folders
// outside the data folder (nil: they cannot be changed here).
func (r *REST) EnableSettings(check config.FolderCheck) {
	r.folderCheck = check
	r.mux.HandleFunc("GET /api/v1/settings", r.getSettings)
	r.mux.HandleFunc("PUT /api/v1/settings", r.putSettings)
}

func (r *REST) getSettings(w http.ResponseWriter, req *http.Request) {
	list, err := config.ReadSettingsForEdit()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	path, _ := config.SettingsPath()
	reply(w, map[string]any{"file": path, "settings": list})
}

func (r *REST) putSettings(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Values map[string]string `json:"values"`
	}
	if !decode(w, req, &body) {
		return
	}
	if len(body.Values) == 0 {
		fail(w, http.StatusBadRequest, errors.New("nothing to change"))
		return
	}
	if err := config.UpdateSettings(body.Values, r.folderCheck); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, config.ErrLockedSetting) {
			code = http.StatusForbidden
		}
		fail(w, code, err)
		return
	}
	reply(w, map[string]any{"saved": true, "restart": true})
}
