package arr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/download"
	"mymule/internal/ed2k"
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

// Shared, a finished download reaches its seeding goal after ShareFor:
// the preferences give that time (in minutes), and each finished download
// how long it has been shared (in seconds), so the apps remove it then.
func TestShareForSeedingTime(t *testing.T) {
	for _, tc := range []struct {
		shares bool
		for_   time.Duration
		want   bool
	}{{true, 7 * 24 * time.Hour, true}, {true, 0, false}, {false, 7 * 24 * time.Hour, false}} {
		fx := newFixture(t)
		fx.a.cfg.SharesFinished, fx.a.cfg.ShareFor = tc.shares, tc.for_
		mux := http.NewServeMux()
		fx.a.Routes(mux)
		srv := httptest.NewServer(mux)
		_, body := get(t, http.DefaultClient, srv.URL+"/api/v2/app/preferences?apikey="+testKey)
		srv.Close()
		var prefs map[string]any
		json.Unmarshal([]byte(body), &prefs)
		enabled := prefs["max_seeding_time_enabled"] == true
		if enabled != tc.want || (tc.want && prefs["max_seeding_time"] != float64(7*24*60)) {
			t.Errorf("shares %v for %v: enabled=%v time=%v", tc.shares, tc.for_, prefs["max_seeding_time_enabled"], prefs["max_seeding_time"])
		}
	}

	// A finished download says how long it has been shared.
	fx := newFixture(t)
	f := download.FileStatus{Hash: ed2k.Hash{1}, Name: "Example Show S01E01.mkv", Size: 10, Done: 10, Finished: true,
		Path: filepath.Join(fx.downloads, "Example Show S01E01.mkv")}
	fx.a.mu.Lock()
	fx.a.store.Downloads[f.Hash.String()] = &storeEntry{Category: "tv-sonarr", Completed: time.Now().Add(-3 * time.Hour).Unix()}
	got := fx.a.torrentLocked(f)
	fx.a.mu.Unlock()
	if s, _ := got["seeding_time"].(int64); s < 3*3600-5 || s > 3*3600+5 {
		t.Fatalf("seeding_time %v, want about 3 hours", got["seeding_time"])
	}
}
