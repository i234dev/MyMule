package authlimit

import (
	"net"
	"net/http"
	"os"
	"strings"
)

// HostCheck refuses requests whose Host header is a name MyMule was not
// told about. A web page on a malicious site can make its own domain
// resolve to 127.0.0.1 ("DNS rebinding") and then talk to the local API
// from the user's browser as if it were the same site; the browser still
// sends the attacker's name as Host, which this check rejects.
//
// Accepted: IP addresses, localhost, this computer's name (also with
// .local, .lan and .home), and the names in extra (from api_hosts).
func HostCheck(extra []string, next http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true}
	if h, err := os.Hostname(); err == nil && h != "" {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		short := strings.SplitN(h, ".", 2)[0]
		for _, n := range []string{h, short, short + ".local", short + ".lan", short + ".home"} {
			allowed[n] = true
		}
	}
	for _, h := range extra {
		allowed[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, allowed) {
			http.Error(w, "MyMule does not answer to the name "+r.Host+
				"; add it to api_hosts in mymule.conf if it is yours", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostAllowed(host string, allowed map[string]bool) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return allowed[host] || strings.HasSuffix(host, ".localhost")
}
