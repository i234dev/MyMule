// Package web serves MyMule's optional web interface: static pages
// embedded in the binary that use the REST API (/api/v1) from the browser.
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	key     string // the api_key: login password and session signing key
	version string
	now     func() time.Time
}

// New creates the web interface. key must not be empty.
func New(key, version string) *UI {
	return &UI{key: key, version: version, now: time.Now}
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

// sign returns the signature of a session that expires at exp.
func (u *UI) sign(exp int64) string {
	m := hmac.New(sha256.New, []byte(u.key))
	m.Write([]byte("mymule-web-session|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// Session reports whether r carries a valid session cookie and the UI
// header. Sessions are signed with the api_key, so they survive restarts
// and changing api_key logs everybody out.
func (u *UI) Session(r *http.Request) bool {
	if u.key == "" || r.Header.Get(uiHeader) == "" {
		return false
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || u.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(u.sign(exp)))
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
	if u.key == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Key)), []byte(u.key)) != 1 {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"wrong key"}` + "\n"))
		return
	}
	exp := u.now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: strconv.FormatInt(exp, 10) + "." + u.sign(exp),
		Path: "/", MaxAge: int(sessionTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}` + "\n"))
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}` + "\n"))
}
