package api

import (
	"context"
	"net/http"
	"time"

	"mymule/internal/mediaserver"
	"mymule/internal/notify"
)

// EnableNotifyTest adds POST /api/v1/notify/test: it sends a test
// notification and asks the media server for a scan, with the settings
// MyMule started with, and says how each went.
func (r *REST) EnableNotifyTest(n *notify.Notifier, ms *mediaserver.Refresher) {
	r.mux.HandleFunc("POST /api/v1/notify/test", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		out := map[string]string{"notify": "off", "media_server": "off"}
		if n.Enabled() {
			out["notify"] = "sent"
			if err := n.SendNow(ctx, notify.Event{Kind: notify.Test, Title: "MyMule",
				Message: "Test notification: notifications work."}); err != nil {
				out["notify"] = err.Error()
			}
		}
		if ms != nil {
			out["media_server"] = "scanning"
			if err := ms.RefreshNow(ctx); err != nil {
				out["media_server"] = err.Error()
			}
		}
		reply(w, out)
	})
}
