package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"mymule/internal/config"
	"mymule/internal/netutil"
	"mymule/internal/server"
)

// Default server.met sources. The user can pass any other URL.
//
// HTTPS, and never a fallback to plain HTTP: on an open Wi-Fi or a
// tampered connection the list could otherwise be swapped for servers
// run by an attacker, who would see every search and the user's IP.
var defaultServerMetURLs = []string{
	"https://upd.emule-security.org/server.met",
}

// maxFetchedServers caps how many servers one downloaded list may add.
const maxFetchedServers = 1000

// serverListClient fetches server lists. It does not follow a redirect
// from HTTPS to plain HTTP, which would undo the point of using HTTPS.
var serverListClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refusing a redirect from HTTPS to " + req.URL.Scheme)
		}
		return nil
	},
}

// usableServers drops entries a downloaded list should never contain:
// addresses that are not public IPv4 ones (they would point MyMule at the
// user's own machine or network), port 0 and repeats; and it keeps at most
// maxFetchedServers.
func usableServers(in []server.Info) []server.Info {
	seen := map[string]bool{}
	var out []server.Info
	for _, s := range in {
		if s.Port == 0 || !netutil.IsPublicIPv4(s.IP) || seen[s.Addr()] {
			continue
		}
		seen[s.Addr()] = true
		out = append(out, s)
		if len(out) == maxFetchedServers {
			break
		}
	}
	return out
}

func serverMetPath() (string, error) { return config.Path("server.met") }

func loadServers() ([]server.Info, error) {
	p, err := serverMetPath()
	if err != nil {
		return nil, err
	}
	list, err := server.LoadServerMet(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return list, err
}

// serversMu serialises the changes to server.met made by this process
// (connection results, updates, additions), each read-modify-write.
var serversMu sync.Mutex

func saveServers(list []server.Info) error {
	p, err := serverMetPath()
	if err != nil {
		return err
	}
	return server.SaveServerMet(p, list)
}

// mergeServers adds servers from add into list, keyed by address.
func mergeServers(list, add []server.Info) ([]server.Info, int) {
	seen := map[string]int{}
	for i, s := range list {
		seen[s.Addr()] = i
	}
	added := 0
	for _, s := range add {
		if i, ok := seen[s.Addr()]; ok {
			// Refresh metadata but keep our failure counter.
			fails := list[i].Fails
			if s.Name == "" {
				s.Name = list[i].Name
			}
			list[i] = s
			list[i].Fails = fails
			continue
		}
		seen[s.Addr()] = len(list)
		list = append(list, s)
		added++
	}
	return list, added
}

func cmdServers(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		list, err := loadServers()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no servers known; run 'mymule servers update'")
			return nil
		}
		server.SortByQuality(list)
		fmt.Printf("%-21s %8s %10s %5s  %s\n", "ADDRESS", "USERS", "FILES", "FAILS", "NAME")
		for _, s := range list {
			fmt.Printf("%-21s %8d %10d %5d  %s\n", s.Addr(), s.Users, s.Files, s.Fails, s.Name)
		}
		return nil

	case "update":
		urls := defaultServerMetURLs
		if len(args) > 1 {
			urls = args[1:]
		}
		_, _, err := updateServerList(urls, func(format string, a ...any) { fmt.Printf(format+"\n", a...) })
		return err

	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: mymule servers add <ip:port>")
		}
		info, err := parseServerAddr(args[1])
		if err != nil {
			return err
		}
		serversMu.Lock()
		defer serversMu.Unlock()
		list, err := loadServers()
		if err != nil {
			return err
		}
		list, _ = mergeServers(list, []server.Info{info})
		fmt.Println("added", info.Addr())
		return saveServers(list)
	}
	return fmt.Errorf("unknown servers subcommand %q", args[0])
}

// autoFetch remembers when the daemon last fetched a server list by itself.
var autoFetch struct {
	sync.Mutex
	last time.Time
}

// fetchServersIfNone fetches a server list, as 'mymule servers update'
// does, when none is known: on the first run the daemon then finds a
// server by itself. It tries at most every 10 minutes.
func fetchServersIfNone(report func(format string, a ...any)) {
	if list, err := loadServers(); err != nil || len(list) > 0 {
		return
	}
	autoFetch.Lock()
	defer autoFetch.Unlock()
	if !autoFetch.last.IsZero() && time.Since(autoFetch.last) < 10*time.Minute {
		return
	}
	autoFetch.last = time.Now()
	report("no servers known yet: fetching a server list")
	if total, _, err := updateServerList(defaultServerMetURLs, report); err == nil {
		report("%d servers known", total)
	}
}

// updateServerList downloads server.met files and merges them into the
// known list. It returns the number of servers and how many are new.
func updateServerList(urls []string, report func(format string, a ...any)) (total, added int, err error) {
	var fetchedAll [][]server.Info
	var lastErr error
	for _, u := range urls {
		fetched, err := fetchServerMet(u)
		if err != nil {
			report("%s: %v", u, err)
			lastErr = err
			continue
		}
		fetchedAll = append(fetchedAll, fetched)
		report("%s: %d servers", u, len(fetched))
	}
	// Read, merge and save at once, after the downloads: the list must
	// not change in between (see updateServerStats).
	serversMu.Lock()
	defer serversMu.Unlock()
	list, err := loadServers()
	if err != nil {
		return 0, 0, err
	}
	for _, fetched := range fetchedAll {
		var n int
		list, n = mergeServers(list, fetched)
		added += n
	}
	if len(list) == 0 {
		return 0, 0, fmt.Errorf("no servers fetched: %v", lastErr)
	}
	return len(list), added, saveServers(list)
}

func fetchServerMet(url string) ([]server.Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mymule/"+version)
	resp, err := serverListClient.Do(req)
	if err != nil {
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return nil, fmt.Errorf("%w (if this system has no CA certificates, install them, e.g. the ca-certificates package)", err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	list, err := server.ParseServerMet(data)
	if err != nil {
		return nil, err
	}
	return usableServers(list), nil
}

func parseServerAddr(s string) (server.Info, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return server.Info{}, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return server.Info{}, fmt.Errorf("bad port %q", portStr)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil {
			return server.Info{}, err
		}
		for _, cand := range ips {
			if ip = cand.To4(); ip != nil {
				break
			}
		}
		if ip == nil {
			return server.Info{}, fmt.Errorf("%s has no IPv4 address", host)
		}
	}
	return server.Info{IP: ip, Port: uint16(port)}, nil
}
