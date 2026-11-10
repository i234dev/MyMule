// Package api is the local control API between the mymule daemon and the
// command-line client.
//
// The daemon listens on 127.0.0.1 only, on a random port, and requires a
// random bearer token. Both are written to <datadir>/api.json (readable
// only by the user), which is how clients find and authenticate to it.
package api

import (
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/share"
	"mymule/internal/stats"
	"mymule/internal/upload"
)

// InfoFile is the name of the discovery file in the data directory.
const InfoFile = "api.json"

// Info is the content of api.json.
type Info struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// Status is the reply to GET /v1/status.
type Status struct {
	Server download.ServerStatus `json:"server"`
	Files  []download.FileStatus `json:"files"`
	Upload *upload.Status        `json:"upload,omitempty"`
	Shared *share.Status         `json:"shared,omitempty"`
	Kad    *KadInfo              `json:"kad,omitempty"`
	Totals *stats.Totals         `json:"totals,omitempty"`
	Router *RouterInfo           `json:"router,omitempty"` // nil: upnp off
}

// AddRequest is the body of POST /v1/add.
type AddRequest struct {
	Links   []string `json:"links"`
	Sources []string `json:"sources,omitempty"` // extra ip:port sources for every link
}

// AddResponse lists what was added and what failed.
type AddResponse struct {
	Added  []string `json:"added"`
	Errors []string `json:"errors,omitempty"`
}

// SearchRequest is the body of POST /v1/search.
type SearchRequest struct {
	Query string `json:"query"`
	Type  string `json:"type,omitempty"`
	Min   uint64 `json:"min,omitempty"`
	Max   uint64 `json:"max,omitempty"`
	Ext   string `json:"ext,omitempty"`
	More  int    `json:"more,omitempty"`
	// Global also asks every known server over UDP (takes a few seconds).
	Global bool `json:"global,omitempty"`
}

// SearchResult is one file found by a search.
type SearchResult struct {
	Hash     ed2k.Hash `json:"hash"`
	Name     string    `json:"name"`
	Size     uint64    `json:"size"`
	Type     string    `json:"type,omitempty"`
	Sources  uint32    `json:"sources"`
	Complete uint32    `json:"complete"`
	Link     string    `json:"link"`
}

// SearchResponse is the reply to POST /v1/search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// MatchRequest selects one download by hash prefix or part of its name.
type MatchRequest struct {
	Query string `json:"query"`
}

// PriorityRequest sets the priority or the preview of a download.
type PriorityRequest struct {
	Query    string `json:"query"`
	Priority string `json:"priority,omitempty"` // low, normal, high
	Preview  *bool  `json:"preview,omitempty"`
}

// NameResponse returns the name of the affected download.
type NameResponse struct {
	Name string `json:"name"`
}

// CountResponse returns a number of affected items.
type CountResponse struct {
	Count int `json:"count"`
}

// ErrorResponse is returned with a non-2xx status.
type ErrorResponse struct {
	Error string `json:"error"`
}
