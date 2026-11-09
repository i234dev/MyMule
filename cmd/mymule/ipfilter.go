package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mymule/internal/ipfilter"
)

// ipFilterFile is the IP filter list in the data folder (eMule's name).
const ipFilterFile = "ipfilter.dat"

// ipFilterMaxAge is how old a downloaded list may get before it is
// downloaded again.
const ipFilterMaxAge = 7 * 24 * time.Hour

// ipFilterRetry is how long after a failed download it is tried again.
const ipFilterRetry = 6 * time.Hour

// ipFilter keeps the IP filter in use up to date with ipfilter.dat in the
// data folder, which it downloads from url (when set) once a week. It
// reads the file again when it changes, so a list put there by hand
// takes effect without a restart.
type ipFilter struct {
	holder ipfilter.Holder
	path   string
	url    string
	log    func(format string, args ...any)

	mod     time.Time // of the file last read
	size    int64
	lastTry time.Time // last download
}

func openIPFilter(dir, url string, log func(string, ...any)) *ipFilter {
	f := &ipFilter{path: filepath.Join(dir, ipFilterFile), url: url, log: log}
	f.reload()
	return f
}

// reload reads the file if it changed since last read. A file that cannot
// be read leaves the filter as it was; a file removed, none.
func (f *ipFilter) reload() {
	fi, err := os.Stat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		if f.holder.Get() != nil {
			f.log("IP filter: %s removed; no address is blocked now", ipFilterFile)
		}
		f.holder.Set(nil)
		f.mod, f.size = time.Time{}, 0
		return
	}
	if err != nil || (fi.ModTime().Equal(f.mod) && fi.Size() == f.size) {
		return
	}
	f.mod, f.size = fi.ModTime(), fi.Size()
	flt, bad, err := ipfilter.Load(f.path)
	if err != nil {
		f.log("IP filter: cannot read %s: %v", ipFilterFile, err)
		return
	}
	f.holder.Set(flt)
	msg := fmt.Sprintf("IP filter: %d ranges blocked (from %s)", flt.Len(), ipFilterFile)
	if bad > 0 {
		msg += fmt.Sprintf("; %d lines not understood", bad)
	}
	f.log("%s", msg)
}

// due reports whether the list should be downloaded now.
func (f *ipFilter) due(now time.Time) bool {
	if f.url == "" || now.Sub(f.lastTry) < ipFilterRetry {
		return false
	}
	fi, err := os.Stat(f.path)
	return err != nil || now.Sub(fi.ModTime()) > ipFilterMaxAge
}

// run downloads the list when due and rereads the file when it changes,
// until ctx is done.
func (f *ipFilter) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if f.due(time.Now()) {
			f.lastTry = time.Now()
			if err := fetchIPFilter(ctx, f.url, f.path); err != nil {
				f.log("IP filter: could not download %s: %v", f.url, err)
			}
		}
		f.reload()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fetchIPFilter downloads a list to path, if it reads as one. The file is
// stored as downloaded (plain, gzip or zip).
func fetchIPFilter(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	client := *nodesClient // same redirect rules
	client.Timeout = 2 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, ipfilter.MaxFile+1))
	if err != nil {
		return err
	}
	if len(b) > ipfilter.MaxFile {
		return errors.New("list too big")
	}
	flt, _, err := ipfilter.Decode(b)
	if err != nil {
		return err
	}
	if flt.Len() == 0 {
		return errors.New("no IP range in it")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
