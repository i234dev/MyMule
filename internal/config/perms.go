package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PrivateFiles are MyMule's own files in the data folder. They hold the
// api_key, the user hash, our private keys (cryptkey.dat for credits,
// kad.dat for Kad), the credits of other users, what Kad nodes published
// to us, the paths of everything shared, and the log: other users of the
// machine must not read them, and they are never shared.
var PrivateFiles = []string{SettingsFile, "api.json", "userhash.dat", "known.json",
	"preferences.json", "library.json", "arr.json", "mymule.log", "mymule.lock", "web_sessions.json",
	"cryptkey.dat", "credits.dat", "kad.dat", "kadindex.dat"}

// TightenPermissions makes the private files in dir readable by their
// owner only. Older versions created them readable by everyone; new
// files are created 0600 already. It returns the names it changed. On
// Windows, where these permission bits do not apply, it does nothing.
func TightenPermissions(dir string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	var changed []string
	for _, name := range PrivateFiles {
		p := filepath.Join(dir, name)
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o077 != 0 {
			if os.Chmod(p, st.Mode().Perm()&0o700) == nil {
				changed = append(changed, name)
			}
		}
	}
	return changed
}

// LocationWarning explains the risk when, on Windows, the data folder is
// outside the user's profile: folders such as C:\MyMule are usually
// readable and writable by every user of the machine, who could then read
// the api_key or replace mymule.exe. Empty when there is nothing to say.
func LocationWarning(dir string) string {
	return locationWarning(runtime.GOOS, dir, os.Getenv("USERPROFILE"))
}

func locationWarning(goos, dir, profile string) string {
	if goos != "windows" || profile == "" {
		return ""
	}
	// Plain string work on Windows paths (case-insensitive, either slash),
	// so it behaves the same whatever system runs it.
	norm := func(p string) string {
		return strings.TrimRight(strings.ToLower(strings.ReplaceAll(p, "/", `\`)), `\`)
	}
	d, prof := norm(dir), norm(profile)
	if d == prof || strings.HasPrefix(d, prof+`\`) {
		return ""
	}
	return "MyMule's folder " + dir + " is outside your user profile (" + profile + "); other users of this " +
		"computer may be able to read mymule.conf (with the api_key) or replace mymule.exe. " +
		"Moving the MyMule folder inside your profile (for example to Documents) avoids that."
}

// TightenDir removes write access for other users from dir when they
// have it: older versions created the temporary folder 0755 or wider,
// and a folder others can write in lets them plant fake downloads there.
// It leaves alone what is not only MyMule's: a folder it does not own,
// one with the sticky bit (like /tmp), and one that is or contains
// another folder in keep (the downloads folder, which other programs
// such as a media server may need). Reports whether it changed anything.
// Does nothing on Windows.
func TightenDir(dir string, keep ...string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o022 == 0 ||
		st.Mode()&os.ModeSticky != 0 || !ownedByMe(st) {
		return false
	}
	abs, _ := filepath.Abs(dir)
	for _, k := range keep {
		if k == "" {
			continue
		}
		ka, _ := filepath.Abs(k)
		if rel, err := filepath.Rel(abs, ka); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return false
		}
	}
	return os.Chmod(dir, st.Mode().Perm()&^0o022) == nil
}
