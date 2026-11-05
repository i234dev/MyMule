package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"mymule/internal/server"
)

// ServerControl lets the web interface see and choose eD2K servers.
type ServerControl interface {
	Servers() ([]server.Info, error)
	ConnectTo(addr string)
	UpdateServers() (total, added int, err error)
}

// EnableServers adds the server list endpoints. The preferred server is
// kept in the preferences (EnableLibrary must be called too).
func (r *REST) EnableServers(sc ServerControl) {
	r.servers = sc
	r.mux.HandleFunc("GET /api/v1/servers", r.listServers)
	r.mux.HandleFunc("POST /api/v1/servers/connect", r.connectServer)
	r.mux.HandleFunc("PUT /api/v1/servers/preferred", r.preferServer)
	r.mux.HandleFunc("POST /api/v1/servers/update", r.updateServers)
}

// RServerInfo is a known server.
type RServerInfo struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	Description string `json:"description,omitempty"`
	Users       uint32 `json:"users"`
	Files       uint32 `json:"files"`
	Fails       uint32 `json:"fails"`
}

func (r *REST) listServers(w http.ResponseWriter, req *http.Request) {
	list, err := r.servers.Servers()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []RServerInfo{}
	for _, s := range list {
		out = append(out, RServerInfo{Name: s.Name, Address: s.Addr(), Description: s.Description,
			Users: s.Users, Files: s.Files, Fails: s.Fails})
	}
	ss, _ := r.m.Status()
	current := ""
	if ss.Connected {
		current = ss.Addr
	}
	preferred := ""
	if r.prefs != nil {
		preferred = r.prefs.Get().Server
	}
	reply(w, map[string]any{"servers": out, "current": current, "preferred": preferred})
}

type serverRequest struct {
	Address string `json:"address"`
}

// knownServer checks that addr is ip:port of a known server, so the web
// interface can't be used to make MyMule connect anywhere.
func (r *REST) knownServer(addr string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || net.ParseIP(host) == nil {
		return errors.New("address must be ip:port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("bad port")
	}
	list, err := r.servers.Servers()
	if err != nil {
		return err
	}
	for _, s := range list {
		if s.Addr() == net.JoinHostPort(host, port) {
			return nil
		}
	}
	return errors.New("unknown server; update the server list first")
}

func (r *REST) connectServer(w http.ResponseWriter, req *http.Request) {
	var sr serverRequest
	if !decode(w, req, &sr) {
		return
	}
	if err := r.knownServer(sr.Address); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	r.servers.ConnectTo(strings.TrimSpace(sr.Address))
	reply(w, map[string]any{"ok": true})
}

// preferServer sets the server to always use ("" = automatic) and
// switches to it.
func (r *REST) preferServer(w http.ResponseWriter, req *http.Request) {
	var sr serverRequest
	if !decode(w, req, &sr) {
		return
	}
	addr := strings.TrimSpace(sr.Address)
	if addr != "" {
		if err := r.knownServer(addr); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	p := r.prefs.Get()
	p.Server = addr
	if err := r.prefs.Set(p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ss, _ := r.m.Status()
	if addr != "" && (!ss.Connected || ss.Addr != addr) {
		r.servers.ConnectTo(addr)
	}
	reply(w, map[string]any{"preferred": addr})
}

func (r *REST) updateServers(w http.ResponseWriter, req *http.Request) {
	total, added, err := r.servers.UpdateServers()
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	reply(w, map[string]int{"servers": total, "new": added})
}
