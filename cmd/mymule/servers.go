package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"mymule/internal/config"
	"mymule/internal/server"
)

// Default server.met sources. The user can pass any other URL.
var defaultServerMetURLs = []string{
	"http://upd.emule-security.org/server.met",
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

// updateServerList downloads server.met files and merges them into the
// known list. It returns the number of servers and how many are new.
func updateServerList(urls []string, report func(format string, a ...any)) (total, added int, err error) {
	list, err := loadServers()
	if err != nil {
		return 0, 0, err
	}
	var lastErr error
	for _, u := range urls {
		fetched, err := fetchServerMet(u)
		if err != nil {
			report("%s: %v", u, err)
			lastErr = err
			continue
		}
		var n int
		list, n = mergeServers(list, fetched)
		added += n
		report("%s: %d servers (%d new)", u, len(fetched), n)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
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
	return server.ParseServerMet(data)
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
