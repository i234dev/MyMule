// Package web serves MyMule's optional web interface: static pages
// embedded in the binary that use the REST API (/api/v1) from the browser.
package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"mymule/internal/authlimit"
	"mymule/internal/fsutil"
)

//go:embed static
var static embed.FS

const (
	cookieName = "mymule_session"
	sessionTTL = 30 * 24 * time.Hour
	// uiHeader must accompany API calls authenticated by the cookie. Pages
	// on other sites can't add custom headers to cross-site requests
	// without CORS, which MyMule never allows, so this stops CSRF.
	uiHeader = "X-MyMule-UI"
)

// UI is the web interface.
type UI struct {
	key     string // the api_key: login password (and signing key unless signKey)
	signKey []byte // signs sessions; see SetSigningKey
	version string
	now     func() time.Time

	mu        sync.Mutex
	revoked   map[string]int64 // session id -> expiry, for sessions logged out
	statePath string           // where revoked is kept; empty = memory only
}

// New creates the web interface. key must not be empty.
func New(key, version string) *UI {
	return &UI{key: key, version: version, now: time.Now, revoked: map[string]int64{}}
}

// SetStateFile keeps logged-out sessions in path, so logging out still
// holds after a restart. Call it before serving.
func (u *UI) SetStateFile(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.statePath = path
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &u.revoked)
		if u.revoked == nil {
			u.revoked = map[string]int64{}
		}
	}
}

// revoke records that session id (valid until exp) was logged out.
func (u *UI) revoke(id string, exp int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now().Unix()
	for k, e := range u.revoked {
		if e < now {
			delete(u.revoked, k) // expired anyway
		}
	}
	u.revoked[id] = exp
	if u.statePath != "" {
		if b, err := json.Marshal(u.revoked); err == nil {
			fsutil.WriteFile(u.statePath, b, 0o600)
		}
	}
}

func (u *UI) isRevoked(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.revoked[id]
	return ok
}

// Routes mounts the pages and the login endpoints on mux.
func (u *UI) Routes(mux *http.ServeMux) {
	files, _ := fs.Sub(static, "static")
	fileServer := http.FileServer(http.FS(files))
	mux.Handle("GET /{$}", secure(fileServer))
	mux.Handle("GET /assets/", secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		fileServer.ServeHTTP(w, r)
	})))
	// The app (PWA): its manifest, and a service worker that must be served
	// from / to cover the whole interface.
	for path, ctype := range map[string]string{"/manifest.webmanifest": "application/manifest+json", "/sw.js": "text/javascript; charset=utf-8"} {
		mux.Handle("GET "+path, secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(files, path[1:])
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ctype)
			w.Write(b)
		})))
	}
	mux.HandleFunc("POST /web/login", u.login)
	mux.HandleFunc("POST /web/logout", u.logout)
}

// secure adds headers that keep the pages from being framed, sniffed or
// leaking the address, and only lets them load their own files.
func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// RequireLogin lets through requests with a session cookie, without the
// UI header: for images and links the browser loads by itself (<img>),
// which only read data.
func (u *UI) RequireLogin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.cookieOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// SetSigningKey makes sessions signed with k (config.SigningKey) instead
// of the api_key. Call it before serving.
func (u *UI) SetSigningKey(k []byte) { u.signKey = k }

// sign returns the signature of session id, which expires at exp.
func (u *UI) sign(exp int64, id string) string {
	key := u.signKey
	if key == nil {
		key = []byte(u.key)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("mymule-web-session|" + strconv.FormatInt(exp, 10) + "|" + id))
	return hex.EncodeToString(m.Sum(nil))
}

// parseCookie checks a session cookie's signature and expiry and returns
// its id and expiry. The value is "exp.id.signature".
func (u *UI) parseCookie(r *http.Request) (id string, exp int64, ok bool) {
	if u.key == "" {
		return "", 0, false
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", 0, false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || len(parts[1]) != 32 {
		return "", 0, false
	}
	exp, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || u.now().Unix() > exp {
		return "", 0, false
	}
	if !hmac.Equal([]byte(parts[2]), []byte(u.sign(exp, parts[1]))) {
		return "", 0, false
	}
	return parts[1], exp, true
}

// Session reports whether r carries a valid session cookie and the UI
// header. Sessions are signed with the api_key, so they survive restarts
// and changing api_key logs everybody out.
func (u *UI) Session(r *http.Request) bool {
	return r.Header.Get(uiHeader) != "" && u.cookieOK(r)
}

// cookieOK reports whether r carries a valid, unexpired session cookie
// that was not logged out.
func (u *UI) cookieOK(r *http.Request) bool {
	id, _, ok := u.parseCookie(r)
	return ok && !u.isRevoked(id)
}

// secureRequest reports whether the browser reached us over HTTPS,
// directly or through a reverse proxy that says so. (Trusting the proxy
// header can only make the cookie stricter.)
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(uiHeader) == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	json.NewDecoder(r.Body).Decode(&req)
	// (Only the page itself can send uiHeader, so no cross-site check.)
	if !authlimit.Default.Allowed(r) && len(u.key) < authlimit.StrongKeyLen {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"too many wrong keys; try again later"}` + "\n"))
		return
	}
	if !authlimit.Default.Check(r, strings.TrimSpace(req.Key), u.key) {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"wrong key"}` + "\n"))
		return
	}
	exp := u.now().Add(sessionTTL).Unix()
	idb := make([]byte, 16)
	rand.Read(idb)
	id := hex.EncodeToString(idb)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: strconv.FormatInt(exp, 10) + "." + id + "." + u.sign(exp, id),
		Path: "/", MaxAge: int(sessionTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: secureRequest(r),
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}` + "\n"))
}

// logout ends the session for good: the cookie is cleared in the browser
// and its id is refused from now on, so a copy of it stops working too.
// It needs the UI header, so another site cannot log the user out.
func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(uiHeader) == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if id, exp, ok := u.parseCookie(r); ok {
		u.revoke(id, exp)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r)})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}` + "\n"))
}
