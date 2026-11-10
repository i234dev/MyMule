package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// After a crash api.json points at a port anything may be using now,
	// even another user's program. Make whatever answers there prove it
	// knows the token before sending the token to it.
	if !c.verify() {
		return nil, ErrNotRunning
	}
	if err := c.do("GET", "/v1/ping", nil, nil); err != nil {
		return nil, ErrNotRunning // stale api.json from a crashed daemon
	}
	return c, nil
}

// verify asks the daemon to prove it knows the token (see Proof).
func (c *Client) verify() bool {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return false
	}
	nonce := hex.EncodeToString(b)
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Get("http://" + c.info.Addr + "/v1/hello?nonce=" + nonce)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Proof string `json:"proof"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out) != nil {
		return false
	}
	return hmac.Equal([]byte(out.Proof), []byte(Proof(c.info.Token, nonce)))
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

// SetPriority sets a download's priority (low, normal, high).
func (c *Client) SetPriority(query, priority string) (string, error) {
	var r NameResponse
	return r.Name, c.do("POST", "/v1/priority", PriorityRequest{Query: query, Priority: priority}, &r)
}

// SetPreview makes a download take its first and last parts first.
func (c *Client) SetPreview(query string, on bool) (string, error) {
	var r NameResponse
	return r.Name, c.do("POST", "/v1/priority", PriorityRequest{Query: query, Preview: &on}, &r)
}

func (c *Client) named(path, query string) (string, error) {
	var r NameResponse
	return r.Name, c.do("POST", path, MatchRequest{Query: query}, &r)
}

func (c *Client) Clear() (int, error) {
	var r CountResponse
	return r.Count, c.do("POST", "/v1/clear", nil, &r)
}

func (c *Client) Stop() error { return c.do("POST", "/v1/stop", nil, nil) }
