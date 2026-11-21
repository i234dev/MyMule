package arr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The apps move a finished download and remove it only once it has
// reached the share limit in app/preferences (state pausedUP, ratio 0).
// That limit depends on whether finished files stay shared.
func TestShareLimitFollowsSharing(t *testing.T) {
	for _, shares := range []bool{false, true} {
		fx := newFixture(t)
		fx.a.cfg.SharesFinished = shares
		mux := http.NewServeMux()
		fx.a.Routes(mux)
		srv := httptest.NewServer(mux)
		_, body := get(t, http.DefaultClient, srv.URL+"/api/v2/app/preferences?apikey="+testKey)
		srv.Close()
		var prefs map[string]any
		if err := json.Unmarshal([]byte(body), &prefs); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		reached := prefs["max_ratio_enabled"] == true && prefs["max_ratio"] == 0.0
		if reached == shares {
			t.Errorf("SharesFinished=%v: max_ratio_enabled=%v max_ratio=%v", shares, prefs["max_ratio_enabled"], prefs["max_ratio"])
		}
	}
}
