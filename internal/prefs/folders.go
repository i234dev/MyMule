package prefs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Library folders are shared on the eD2K network and receive files whose
// names come from the network, and anyone holding the api_key can change
// them. A folder like the user's home, a drive root, the data folder or
// a system/startup folder would expose private files or let a download
// land where the system runs it, so those are refused.

// systemDirs are never acceptable, nor anything inside them.
func systemDirs() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for _, v := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
			"ProgramData", "APPDATA", "LOCALAPPDATA"} {
			if d := os.Getenv(v); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	out := []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
		"/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib", "/var/log", "/var/spool"}
	if runtime.GOOS == "darwin" {
		// Not all of /private: /var/folders (users' temporary files) is there.
		out = append(out, "/System", "/Library", "/Applications", "/cores",
			"/private/etc", "/private/var/db", "/private/var/root")
	}
	return out
}

// samePath compares cleaned absolute paths (case-insensitively on Windows
// and macOS, whose usual file systems ignore case).
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// errNotInRoots: see Store.AllowRoots.
var errNotInRoots = errors.New("it must be inside MyMule's folder, download_dir or a folder listed in folder_roots in mymule.conf, because files from the network land there with the names peers give them")

// inRoots reports whether abs, as written and as it really is (links
// followed), is one of roots or inside one.
func inRoots(abs string, roots []string) error {
	paths := []string{filepath.Clean(abs)}
	if real := resolveExisting(paths[0]); real != "" && !samePath(real, paths[0]) {
		paths = append(paths, real)
	}
	for _, p := range paths {
		ok := false
		for _, r := range roots {
			r = filepath.Clean(r)
			if within(p, r) {
				ok = true
				break
			}
			if real := resolveExisting(r); real != "" && within(p, real) {
				ok = true
				break
			}
		}
		if !ok {
			return errNotInRoots
		}
	}
	return nil
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	if runtime.GOOS == "darwin" {
		// filepath.Rel compares case-sensitively there; the disk does not.
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.VolumeName(p), filepath.VolumeName(dir)) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// checkFolder reports why abs cannot be a library folder, or nil.
// noContain lists folders the library folder may not be or contain (the
// data folder: sharing it would publish mymule.conf). noInside lists
// folders it may not be, contain or be inside (the temporary folder).
// Symbolic links are resolved when the folder exists, so a link cannot
// point around these rules. netHosts are the computers whose Windows
// network folders are accepted (see windowsPathProblem).
func checkFolder(abs string, noContain, noInside, netHosts []string) error {
	if !filepath.IsAbs(abs) {
		return errors.New("it must be a full path")
	}
	if runtime.GOOS == "windows" {
		if err := windowsPathProblem(abs, netHosts); err != nil {
			return err
		}
	}
	paths := []string{filepath.Clean(abs)}
	if real := resolveExisting(paths[0]); real != "" && !samePath(real, paths[0]) {
		paths = append(paths, real)
	}
	home, _ := os.UserHomeDir()
	for _, p := range paths {
		if filepath.Dir(p) == p || (runtime.GOOS == "windows" && strings.TrimRight(p, `\/`) == strings.TrimRight(filepath.VolumeName(p), `\/`)) {
			return errors.New("a whole drive cannot be used; choose a folder")
		}
		for _, part := range strings.Split(strings.TrimPrefix(p, filepath.VolumeName(p)), string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") {
				return errors.New("hidden folders cannot be used")
			}
		}
		if home != "" && within(home, p) {
			return errors.New("it would contain your whole user folder; choose a folder inside it, such as Videos")
		}
		for _, d := range noContain {
			if d != "" && within(filepath.Clean(d), p) {
				return errors.New("it would contain MyMule's own data folder")
			}
		}
		for _, d := range noInside {
			if d != "" && (within(filepath.Clean(d), p) || within(p, filepath.Clean(d))) {
				return errors.New("it overlaps MyMule's temporary folder")
			}
		}
		for _, d := range systemDirs() {
			if within(p, filepath.Clean(d)) {
				return errors.New("system folders cannot be used")
			}
			// Nor a folder holding one (/var holds /var/lib, /private
			// holds /private/etc): it would be shared with it.
			if within(filepath.Clean(d), p) {
				return errors.New("it would contain system folders; choose a folder inside it")
			}
		}
		// Every user's folder is in these: sharing one would share the
		// other users' files (and, run as root, the user's own home is
		// /root, so the check above does not see them).
		if runtime.GOOS != "windows" {
			for _, d := range []string{"/home", "/Users"} {
				if within(d, p) {
					return errors.New("it would contain the users' folders; choose a folder inside yours")
				}
			}
		}
	}
	return nil
}
