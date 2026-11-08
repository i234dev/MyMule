package authlimit

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
)

// StrongKeyLen is the length from which a key is treated as impossible to
// guess (the generated api_key has 32 random hex characters). The right
// strong key is accepted even from a blocked address: blocking it would
// protect nothing and would only let anyone who can make requests from
// that address (any web page the user visits, everyone behind the same
// reverse proxy) lock the user and the *arr apps out.
const StrongKeyLen = 24

// Check reports whether got is the right key, counting a wrong one
// against r's address. While the address is blocked only a right strong
// key gets through.
func (l *Limiter) Check(r *http.Request, got, key string) bool {
	right := key != "" && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
	if right && len(key) >= StrongKeyLen {
		return true
	}
	if !l.Allowed(r) {
		return false
	}
	if !right {
		l.Failed(r)
	}
	return right
}

// CheckQuiet is Check for requests a browser sent on behalf of another
// site (see CrossSite). Such a page makes up whatever key it likes, so
// its wrong keys must not block the user's own address; but anyone can
// make a request look like that, so they cannot go uncounted either (a
// short key could then be guessed without limit). They are counted
// apart: too many block only further requests of this kind from the
// address, and a right strong key still gets through (a link with
// ?apikey= opened from a dashboard).
func (l *Limiter) CheckQuiet(r *http.Request, got, key string) bool {
	right := key != "" && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
	if right && len(key) >= StrongKeyLen {
		return true
	}
	if !l.Allowed(r) || !l.crossSite.Allowed(r) {
		return false
	}
	if !right {
		l.crossSite.Failed(r)
	}
	return right
}

// CrossSite reports whether a browser sent r on behalf of a page from
// another site: an <img> or a hidden form on a web page the user opened.
// Programs (curl, the *arr apps, scripts) send none of these headers.
func CrossSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site":
		return true
	case "same-origin", "same-site", "none":
		return false
	}
	// Browsers without Sec-Fetch-* headers: the page's address.
	for _, h := range []string{"Origin", "Referer"} {
		v := r.Header.Get(h)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return true // "null" (sandboxed page) or garbage
		}
		return !strings.EqualFold(u.Host, r.Host)
	}
	return false
}
