// Package mediaserver asks Jellyfin, Emby or Plex to scan its libraries
// again when MyMule has put new files in them, so they show up at once.
package mediaserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Kinds of media servers.
var Kinds = []string{"jellyfin", "emby", "plex"}

// Check validates the media_server settings.
func Check(kind, address, token string) error {
	if kind == "" {
		return nil
	}
	ok := false
	for _, k := range Kinds {
		ok = ok || k == kind
	}
	if !ok {
		return fmt.Errorf("media_server must be jellyfin, emby, plex or empty")
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("media_server_url must be the server's http:// or https:// address")
	}
	if token == "" {
		return errors.New("media_server_token is needed (an API key, or Plex's token)")
	}
	return nil
}

// Refresher asks the server for a scan, at most once every Gap: files
// finishing close together make a single scan.
type Refresher struct {
	kind, base, token string
	client            *http.Client
	logf              func(string, ...any)
	Gap               time.Duration

	mu      sync.Mutex
	pending bool
	last    time.Time
	timer   *time.Timer
}

// New makes a Refresher, or returns nil when kind is empty.
func New(kind, address, token string, logf func(string, ...any)) (*Refresher, error) {
	if kind == "" {
		return nil, nil
	}
	if err := Check(kind, address, token); err != nil {
		return nil, err
	}
	return &Refresher{kind: kind, base: strings.TrimRight(address, "/"), token: token,
		client: &http.Client{Timeout: 30 * time.Second}, logf: logf, Gap: 2 * time.Minute}, nil
}

// Refresh asks for a scan soon.
func (r *Refresher) Refresh() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending {
		return
	}
	r.pending = true
	wait := max(0, r.Gap-time.Since(r.last))
	r.timer = time.AfterFunc(wait, func() {
		r.mu.Lock()
		r.pending, r.last = false, time.Now()
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.RefreshNow(ctx); err != nil && r.logf != nil {
			r.logf("%s: library scan not started: %v", r.kind, err)
		}
	})
}

// Stop drops a scan not started yet.
func (r *Refresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.pending = false
}

// RefreshNow asks for a scan and waits for the answer.
func (r *Refresher) RefreshNow(ctx context.Context) error {
	var req *http.Request
	var err error
	switch r.kind {
	case "plex":
		req, err = http.NewRequestWithContext(ctx, "GET", r.base+"/library/sections/all/refresh", nil)
		if err == nil {
			req.Header.Set("X-Plex-Token", r.token)
		}
	default: // jellyfin, emby
		req, err = http.NewRequestWithContext(ctx, "POST", r.base+"/Library/Refresh", nil)
		if err == nil {
			req.Header.Set("Authorization", `MediaBrowser Client="MyMule", Token="`+r.token+`"`)
			req.Header.Set("X-Emby-Token", r.token)
		}
	}
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}
