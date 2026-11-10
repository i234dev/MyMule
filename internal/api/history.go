package api

import (
	"net/http"

	"mymule/internal/stats"
)

// EnableHistory adds GET /api/v1/stats/history: the transfer speeds over
// time (a point a minute for a day, an hour for 30 days) and what was
// transferred each day for a year.
func (r *REST) EnableHistory(h *stats.History) {
	r.mux.HandleFunc("GET /api/v1/stats/history", func(w http.ResponseWriter, req *http.Request) {
		minutes, hours, days := h.Snapshot()
		reply(w, map[string]any{"minutes": minutes, "hours": hours, "days": days})
	})
}
