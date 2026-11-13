package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Started with a double-click (Windows; see startedByDoubleClick): MyMule
// runs as "mymule daemon" in its own window, with the web interface on
// (only for this computer unless mymule.conf says otherwise), and opens
// it in the browser, already logged in. Nothing is written to mymule.conf.
// The window stays open while MyMule runs; closing it stops MyMule.

// clicked is set when MyMule was started with a double-click.
var clicked bool

// defaultWebListen is where the web interface listens when started with a
// double-click and api_listen is empty: this computer only.
const defaultWebListen = "127.0.0.1:4711"

// clickedSettings turns the web interface on for this run.
func clickedSettings() {
	settings.WebUI = true
	if settings.APIListen == "" {
		settings.APIListen = defaultWebListen
	}
}

// showStarted tells what is going on in the window, and opens the web
// interface at base with the one-time login link path.
func showStarted(base, path string) {
	fmt.Fprintf(os.Stderr, "\n  MyMule is running. Its web interface opens in your browser:\n  %s/\n"+
		"  Keep this window open while you use MyMule; close it to stop MyMule.\n\n", base)
	if err := openBrowser(base + path); err != nil {
		fmt.Fprintf(os.Stderr, "  Could not open the browser (%v): open %s/ and log in with api_key from mymule.conf.\n\n", err, base)
	}
}

// openRunning handles a double-click while MyMule is already running: it
// opens that one's web interface, logged in, and reports true. With none
// running it reports false, and this one starts.
func openRunning() bool {
	if _, err := daemonClient(); err != nil {
		return false
	}
	base := "http://" + defaultWebListen
	if _, port, err := net.SplitHostPort(settings.APIListen); err == nil {
		base = "http://127.0.0.1:" + port
	}
	path, err := askStartLink(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MyMule is already running, but its web interface cannot be opened (%v).\n"+
			"Turn it on with web_ui = true in mymule.conf, and restart MyMule.\n", err)
		exitCode = 1 // keeps the window open to read it
		return true
	}
	fmt.Fprintf(os.Stderr, "MyMule is already running; opening its web interface: %s/\n", base)
	if err := openBrowser(base + path); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open the browser (%v): open %s/ and log in with api_key from mymule.conf.\n", err, base)
		exitCode = 1
	}
	return true
}

// askStartLink asks the running MyMule at base for a one-time login link.
func askStartLink(base string) (string, error) {
	req, err := http.NewRequest("POST", base+"/api/v1/web/link", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Api-Key", settings.APIKey)
	c := &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("its API answered %s", resp.Status)
	}
	var r struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&r); err != nil {
		return "", err
	}
	if !strings.HasPrefix(r.Path, "/web/start?t=") {
		return "", errors.New("unexpected answer")
	}
	return r.Path, nil
}

// waitIfFailed keeps the window of a double-clicked MyMule open after an
// error, so that it can be read.
func waitIfFailed() {
	if exitCode == 0 {
		return
	}
	fmt.Fprint(os.Stderr, "\nPress Enter to close this window.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
