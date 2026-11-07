package prefs

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// resolveExisting resolves symbolic links (and, on Windows, short 8.3
// names such as PROGRA~1) in the longest part of p that exists, and
// appends the rest. "" when nothing can be resolved.
func resolveExisting(p string) string {
	rest := ""
	for dir := p; ; {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if fp, err := finalPath(real); err == nil {
				real = fp // junctions too (Windows)
			}
			return filepath.Clean(filepath.Join(real, rest))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// windowsPathProblem refuses Windows path forms that name a folder in a
// way the other checks would not recognise: device paths (\\?\C:\...,
// \\.\C:\...), network paths back to this same computer
// (\\localhost\C$\Users\...), administrative shares (C$), alternate data
// streams, and names ending in dots or spaces, which Windows silently
// drops ("C:\Windows." is C:\Windows). Network paths to other computers
// are refused too unless the computer is in hosts (network_hosts in
// mymule.conf): Windows logs in to any computer named in a path it opens,
// sending the user's login hash, so a folder such as \\attacker\x or
// \\attacker@SSL@443\DavWWWRoot (WebDAV) set through the API would hand
// it to whoever runs that computer.
func windowsPathProblem(p string, hosts []string) error {
	s := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(s, `\\?\`) || strings.HasPrefix(s, `\\.\`) || strings.HasPrefix(s, `\??\`) {
		return errors.New(`use a normal path such as D:\Movies`)
	}
	vol := uncVolume(s)
	if vol != "" {
		parts := strings.SplitN(strings.TrimPrefix(vol, `\\`), `\`, 2)
		if len(parts) != 2 || parts[1] == "" || strings.HasSuffix(parts[1], "$") || isThisComputer(parts[0]) {
			return errors.New("use the folder's normal path, not a network path to this computer or an administrative share")
		}
		if !networkHostAllowed(parts[0], hosts) {
			return errors.New(`network folders are only accepted from the computers listed in network_hosts in mymule.conf (opening one sends your Windows login hash to that computer); a mapped drive such as Z:\Movies also works`)
		}
	} else if len(s) >= 2 && s[1] == ':' {
		vol = s[:2]
	}
	for _, part := range strings.Split(strings.TrimPrefix(s, vol), `\`) {
		if part == "" || part == "." || part == ".." {
			continue
		}
		if strings.Contains(part, ":") {
			return errors.New("folder names cannot contain ':'")
		}
		if strings.TrimRight(part, ". ") != part {
			return errors.New("folder names cannot end with a dot or a space")
		}
	}
	return nil
}

// networkHostAllowed reports whether host, from a network path, is one of
// hosts. A WebDAV host (nas@SSL@443, nas@8080) counts as its name.
func networkHostAllowed(host string, hosts []string) bool {
	if i := strings.IndexByte(host, '@'); i >= 0 {
		host = host[:i]
	}
	for _, h := range hosts {
		if h != "" && strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// uncVolume returns the \\host\share part of a network path, or "".
// (filepath.VolumeName only understands these on Windows; this works the
// same everywhere, so the tests run on every system.)
func uncVolume(s string) string {
	if !strings.HasPrefix(s, `\\`) {
		return ""
	}
	rest := s[2:]
	i := strings.Index(rest, `\`)
	if i < 0 {
		return s
	}
	j := strings.Index(rest[i+1:], `\`)
	if j < 0 {
		return s
	}
	return s[:2+i+1+j]
}

// isThisComputer reports whether a network path's host names this
// computer.
func isThisComputer(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || host == "localhost" || host == "." || host == "?" {
		return true
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		name = strings.ToLower(name)
		short := strings.SplitN(name, ".", 2)[0]
		if host == name || host == short || strings.SplitN(host, ".", 2)[0] == short {
			return true
		}
	}
	// IPv6 addresses are written like fe80--1.ipv6-literal.net in network
	// paths.
	if strings.HasSuffix(host, ".ipv6-literal.net") {
		host = strings.ReplaceAll(strings.TrimSuffix(host, ".ipv6-literal.net"), "-", ":")
		if i := strings.IndexByte(host, 's'); i >= 0 {
			host = host[:i] // zone
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
