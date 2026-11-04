package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/server"
)

// Server exposes a download.Manager over HTTP on localhost.
type Server struct {
	m     *download.Manager
	stop  func()
	token string
	srv   *http.Server
	info  string
}

// Listen starts the API on 127.0.0.1 (random port) and writes the
// discovery file into dataDir. stop is called when a client asks the
// daemon to shut down.
func Listen(m *download.Manager, dataDir string, stop func()) (*Server, error) {
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{m: m, stop: stop, token: hex.EncodeToString(tok), info: filepath.Join(dataDir, InfoFile)}

	data, _ := json.Marshal(Info{Addr: ln.Addr().String(), Token: s.token, PID: os.Getpid()})
	tmp := s.info + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Rename(tmp, s.info); err != nil {
		ln.Close()
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { reply(w, struct{}{}) })
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("POST /v1/add", s.add)
	mux.HandleFunc("POST /v1/search", s.search)
	mux.HandleFunc("POST /v1/pause", s.pause(true))
	mux.HandleFunc("POST /v1/resume", s.pause(false))
	mux.HandleFunc("POST /v1/cancel", s.cancel)
	mux.HandleFunc("POST /v1/clear", s.clear)
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		reply(w, struct{}{})
		go s.stop()
	})

	s.srv = &http.Server{Handler: s.auth(mux), ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

// Close stops the API and removes the discovery file.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
	os.Remove(s.info)
}

func (s *Server) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			fail(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(ErrorResponse{Error: err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func errCode(err error) int {
	switch {
	case errors.Is(err, download.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, download.ErrAmbiguous), errors.Is(err, download.ErrExists),
		errors.Is(err, download.ErrBadSource), errors.Is(err, download.ErrFinishedYet):
		return http.StatusConflict
	case errors.Is(err, download.ErrNoServer):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ss, files := s.m.Status()
	if files == nil {
		files = []download.FileStatus{}
	}
	up, shared := s.m.SharingStatus()
	reply(w, Status{Server: ss, Files: files, Upload: up, Shared: shared})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var req AddRequest
	if !decode(w, r, &req) {
		return
	}
	resp := AddResponse{Added: []string{}}
	for _, l := range req.Links {
		link, err := ed2k.ParseLink(l)
		if err == nil {
			err = s.m.AddLink(link, req.Sources...)
		}
		if err != nil {
			resp.Errors = append(resp.Errors, link.Name+": "+err.Error())
			continue
		}
		resp.Added = append(resp.Added, link.Name)
	}
	reply(w, resp)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest
	if !decode(w, r, &req) {
		return
	}
	ftype, err := server.ParseFileType(req.Type)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	q := server.Query{Keywords: req.Query, Type: ftype, MinSize: req.Min, MaxSize: req.Max, Extension: req.Ext}
	var res []server.Result
	if req.Global {
		res, _, err = s.m.SearchGlobal(ctx, q, 6*time.Second)
	} else {
		res, err = s.m.Search(ctx, q, req.More)
	}
	if err != nil {
		fail(w, errCode(err), err)
		return
	}
	seen := map[ed2k.Hash]int{}
	out := SearchResponse{Results: []SearchResult{}}
	for _, x := range res {
		if i, ok := seen[x.Hash]; ok {
			out.Results[i].Sources = max(out.Results[i].Sources, x.Sources)
			continue
		}
		seen[x.Hash] = len(out.Results)
		out.Results = append(out.Results, SearchResult{
			Hash: x.Hash, Name: x.Name, Size: x.Size, Type: x.Type,
			Sources: x.Sources, Complete: x.CompleteSources, Link: x.Link().String(),
		})
	}
	sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].Sources > out.Results[j].Sources })
	reply(w, out)
}

func (s *Server) pause(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req MatchRequest
		if !decode(w, r, &req) {
			return
		}
		h, err := s.m.Find(req.Query)
		if err == nil {
			var name string
			name, err = s.m.SetPaused(h, paused)
			if err == nil {
				reply(w, NameResponse{Name: name})
				return
			}
		}
		fail(w, errCode(err), err)
	}
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	var req MatchRequest
	if !decode(w, r, &req) {
		return
	}
	h, err := s.m.Find(req.Query)
	if err == nil {
		var name string
		name, err = s.m.Cancel(h)
		if err == nil {
			reply(w, NameResponse{Name: name})
			return
		}
	}
	fail(w, errCode(err), err)
}

func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	reply(w, CountResponse{Count: s.m.ClearFinished()})
}
