package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ErrNotRunning means no daemon is reachable for this data directory.
var ErrNotRunning = errors.New("mymule daemon is not running")

// Client talks to a running daemon.
type Client struct {
	info Info
	http *http.Client
}

// Connect finds the daemon for dataDir and checks that it answers.
func Connect(dataDir string) (*Client, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, InfoFile))
	if err != nil {
		return nil, ErrNotRunning
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil || info.Addr == "" {
		return nil, ErrNotRunning
	}
	c := &Client{info: info, http: &http.Client{Timeout: 60 * time.Second}}
	if err := c.do("GET", "/v1/ping", nil, nil); err != nil {
		return nil, ErrNotRunning // stale api.json from a crashed daemon
	}
	return c, nil
}

// PID returns the daemon's process ID.
func (c *Client) PID() int { return c.info.PID }

func (c *Client) do(method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, "http://"+c.info.Addr+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.info.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e ErrorResponse
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("daemon returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Status() (Status, error) {
	var s Status
	return s, c.do("GET", "/v1/status", nil, &s)
}

func (c *Client) Add(req AddRequest) (AddResponse, error) {
	var r AddResponse
	return r, c.do("POST", "/v1/add", req, &r)
}

func (c *Client) Search(req SearchRequest) (SearchResponse, error) {
	var r SearchResponse
	return r, c.do("POST", "/v1/search", req, &r)
}

func (c *Client) Pause(query string) (string, error)  { return c.named("/v1/pause", query) }
func (c *Client) Resume(query string) (string, error) { return c.named("/v1/resume", query) }
func (c *Client) Cancel(query string) (string, error) { return c.named("/v1/cancel", query) }

func (c *Client) named(path, query string) (string, error) {
	var r NameResponse
	return r.Name, c.do("POST", path, MatchRequest{Query: query}, &r)
}

func (c *Client) Clear() (int, error) {
	var r CountResponse
	return r.Count, c.do("POST", "/v1/clear", nil, &r)
}

func (c *Client) Stop() error { return c.do("POST", "/v1/stop", nil, nil) }
