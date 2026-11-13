// Package update tells whether a newer MyMule is out: once a day it asks
// GitHub for the latest release. It only asks for the version: nothing is
// downloaded or installed.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LatestURL is where the latest release is described (GitHub's API).
const LatestURL = "https://api.github.com/repos/i234dev/MyMule/releases/latest"

// releasesPage is what a release's page address must start with.
const releasesPage = "https://github.com/i234dev/MyMule/releases/"

// Every is how often it asks.
const Every = 24 * time.Hour

// Release is a newer version than the one running.
type Release struct {
	Version string    `json:"version"` // e.g. 1.4.0
	URL     string    `json:"url"`     // its page: what is new, downloads
	At      time.Time `json:"published"`
}

// Checker asks for the latest release and keeps what it found.
type Checker struct {
	Current   string // the running version (1.3.5 or v1.3.5)
	URL       string // LatestURL unless tests change it
	UserAgent string
	Log       func(format string, args ...any)
	Client    *http.Client

	mu       sync.Mutex
	newer    *Release
	reported string // the version already logged
}

// Newer returns the newer release found, or nil (none, or not asked yet).
func (c *Checker) Newer() *Release {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.newer == nil {
		return nil
	}
	r := *c.newer
	return &r
}

// Run asks a minute after it starts, then every day, until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = Every
		if err := c.Check(ctx); err != nil && ctx.Err() == nil && c.Log != nil {
			c.Log("update check: %v", err)
		}
	}
}

// Check asks once.
func (c *Checker) Check(ctx context.Context) error {
	r, err := Latest(ctx, c.client(), c.url(), c.UserAgent)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !Newer(r.Version, c.Current) {
		c.newer = nil
		return nil
	}
	c.newer = &r
	if c.reported != r.Version && c.Log != nil {
		c.reported = r.Version
		c.Log("a new version of MyMule is out: %s (you have %s): %s", r.Version, strings.TrimPrefix(c.Current, "v"), r.URL)
	}
	return nil
}

func (c *Checker) url() string {
	if c.URL != "" {
		return c.URL
	}
	return LatestURL
}

func (c *Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// versionRe is a release version: v1.2.3 (no pre-releases).
var versionRe = regexp.MustCompile(`^v?(\d{1,4})\.(\d{1,4})\.(\d{1,4})$`)

// Latest asks url for the latest release. What comes back is checked: a
// plain version, and a page among MyMule's releases.
func Latest(ctx context.Context, client *http.Client, url, userAgent string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel struct {
		Tag        string    `json:"tag_name"`
		URL        string    `json:"html_url"`
		Draft      bool      `json:"draft"`
		Prerelease bool      `json:"prerelease"`
		Published  time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("unexpected answer: %w", err)
	}
	if rel.Draft || rel.Prerelease || !versionRe.MatchString(rel.Tag) {
		return Release{}, errors.New("unexpected answer: no release version")
	}
	if !strings.HasPrefix(rel.URL, releasesPage) || strings.ContainsAny(rel.URL, "\"<> ") {
		rel.URL = releasesPage + "latest"
	}
	return Release{Version: strings.TrimPrefix(rel.Tag, "v"), URL: rel.URL, At: rel.Published}, nil
}

// Newer reports whether version a is newer than b (both like 1.2.3 or
// v1.2.3). A version that is not one (a development build) is never newer
// than anything, nor older.
func Newer(a, b string) bool {
	x, okA := parse(a)
	y, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}
