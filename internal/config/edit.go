package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"mymule/internal/fsutil"
)

// Editing mymule.conf from the web interface.

// SettingInfo is one setting as the web interface shows it.
type SettingInfo struct {
	Key     string `json:"key"`
	Value   string `json:"value"`   // as in mymule.conf (paths as written)
	Default string `json:"default"` // the built-in value
	Comment string `json:"comment"` // its explanation in mymule.conf
	Kind    string `json:"kind"`    // "bool", "int" or "text"
	// Locked settings are only changed in mymule.conf itself: who can
	// reach the API and with what key, and which network folders may be
	// opened. A web session must not be able to widen its own access.
	Locked bool `json:"locked,omitempty"`
	// Env: a MYMULE_ environment variable overrides the file (containers);
	// a change in the file does not take effect while it is set.
	Env bool `json:"env,omitempty"`
	// Secret settings hold tokens: their value is never sent out; Set
	// says whether one is there. A new value replaces it.
	Secret bool `json:"secret,omitempty"`
	Set    bool `json:"set,omitempty"`
}

// secretSettings hold tokens (notification addresses carry them).
var secretSettings = map[string]bool{"notify_urls": true, "media_server_token": true, "tmdb_api_key": true}

// lockedSettings cannot be changed through the API.
var lockedSettings = map[string]bool{
	"api_listen": true, "api_key": true, "api_hosts": true, "network_hosts": true, "web_ui": true,
	"folder_roots": true,
}

var boolSettings = map[string]bool{"kad": true, "upnp": true, "sharing": true, "share_downloads": true, "web_ui": true, "arr_support": true, "update_check": true}
var intSettings = map[string]bool{"port": true, "kad_port": true, "max_conns": true, "upload_slots": true, "upload_limit": true, "download_limit": true}

// maxSettingValue bounds a value written from the API.
const maxSettingValue = 4096

// ReadSettingsForEdit returns every setting as mymule.conf has it now.
func ReadSettingsForEdit() ([]SettingInfo, error) {
	p, err := SettingsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	s := Defaults()
	if _, err := parseSettings(string(data), &s); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	d := Defaults()
	var out []SettingInfo
	for _, st := range settingsDoc {
		kind := "text"
		switch {
		case boolSettings[st.key]:
			kind = "bool"
		case intSettings[st.key]:
			kind = "int"
		}
		si := SettingInfo{Key: st.key, Value: st.value(s), Default: st.value(d), Comment: st.comment, Kind: kind, Locked: lockedSettings[st.key]}
		if st.key == "api_key" {
			si.Value, si.Default = "", "" // never sent out
		}
		// "off" (tmdb_api_key) is no secret.
		if secretSettings[st.key] && !strings.EqualFold(si.Value, "off") {
			si.Secret, si.Set = true, si.Value != ""
			si.Value = ""
		}
		_, si.Env = os.LookupEnv(EnvPrefix + strings.ToUpper(st.key))
		out = append(out, si)
	}
	return out, nil
}

// ErrLockedSetting is returned for a change to a locked setting.
var ErrLockedSetting = errors.New("this setting can only be changed in mymule.conf")

// editMu makes changes to mymule.conf one at a time: two at once would
// each read the file, and the second write would undo the first.
var editMu sync.Mutex

// FolderCheck reports why a folder cannot be used, or nil.
type FolderCheck func(abs string) error

// UpdateSettings writes values (setting name -> value) into mymule.conf,
// keeping everything else in it as it is. The whole file must still read
// without error, or nothing is written. Changes take effect when MyMule
// restarts.
//
// A changed folder (temp_dir, download_dir, shared_dirs) outside the data
// folder must pass check, the rules library folders follow: whoever holds
// the API key must not be able to share the users' folders or have
// downloads land in a system folder. Without check, folders cannot be
// changed.
func UpdateSettings(values map[string]string, check FolderCheck) error {
	editMu.Lock()
	defer editMu.Unlock()
	known := map[string]bool{}
	for _, st := range settingsDoc {
		known[st.key] = true
	}
	for k, v := range values {
		switch {
		case !known[k]:
			return fmt.Errorf("unknown setting %q", k)
		case lockedSettings[k]:
			return fmt.Errorf("%s: %w", k, ErrLockedSetting)
		case len(v) > maxSettingValue || strings.ContainsFunc(v, unicode.IsControl):
			return fmt.Errorf("%s: the value must be one line of text", k)
		}
	}
	p, err := SettingsPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	text := string(data)
	cur := Defaults()
	if _, err := parseSettings(text, &cur); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	for _, st := range settingsDoc { // in a fixed order
		v, ok := values[st.key]
		if !ok {
			continue
		}
		text = setLine(text, st.key, strings.TrimSpace(v))
	}
	s := Defaults()
	if _, err := parseSettings(text, &s); err != nil {
		return err
	}
	// The folders as MyMule will read them (quotes and spaces taken off),
	// not as they were sent.
	if err := checkFolders(s, cur, filepath.Dir(p), check); err != nil {
		return err
	}
	if err := checkMediaServer(values, cur, s, os.LookupEnv); err != nil {
		return err
	}
	if err := checkSettings(s); err != nil {
		return err
	}
	return fsutil.WriteFile(p, []byte(text), 0o600)
}

// checkMediaServer: the token is sent to the media server's address, so
// whoever changes that must know the token, or could have it sent to
// them. cur and next are the settings before and after the change, read
// from mymule.conf; the environment (MYMULE_...) is applied to both, as
// MyMule will. A token set in the environment cannot be replaced from
// here, so then the server and its address cannot change either.
func checkMediaServer(values map[string]string, cur, next Settings, lookup func(string) (string, bool)) error {
	if err := applyEnv(&cur, lookup); err != nil {
		return err
	}
	if err := applyEnv(&next, lookup); err != nil {
		return err
	}
	if cur.MediaServerToken == "" || (next.MediaServer == cur.MediaServer && next.MediaServerURL == cur.MediaServerURL) {
		return nil
	}
	if v, ok := lookup(EnvPrefix + "MEDIA_SERVER_TOKEN"); ok && strings.TrimSpace(v) != "" {
		return errors.New("media_server: the token is set in the environment (MYMULE_MEDIA_SERVER_TOKEN), so change the media server there too")
	}
	if _, sent := values["media_server_token"]; !sent {
		return errors.New("media_server_token: type the token again when you change the media server or its address")
	}
	return nil
}

// checkFolders applies check to the folders of next (the settings after
// the change) that cur (before it) did not have (see UpdateSettings).
// Folders inside the data folder are MyMule's own, as the default temp
// and downloads are; folders already in mymule.conf were put there by
// whoever edits it, and are not checked again.
func checkFolders(next, cur Settings, base string, check FolderCheck) error {
	had := map[string]bool{}
	for _, d := range append([]string{cur.TempDir, cur.DownloadDir}, cur.SharedDirs...) {
		had[absUnder(base, d)] = true
	}
	for _, f := range []struct {
		key  string
		dirs []string
	}{{"temp_dir", []string{next.TempDir}}, {"download_dir", []string{next.DownloadDir}}, {"shared_dirs", next.SharedDirs}} {
		for _, d := range f.dirs {
			abs := absUnder(base, d)
			if d == "" || had[abs] || insideDir(abs, base) {
				continue
			}
			if check == nil {
				return fmt.Errorf("%s: folders can only be changed in mymule.conf", f.key)
			}
			if err := check(abs); err != nil {
				return fmt.Errorf("%s: %s: %w", f.key, d, err)
			}
		}
	}
	return nil
}

// insideDir reports whether p is inside dir (not dir itself).
func insideDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// setLine sets key to value in a mymule.conf text: every line of key is
// replaced (the file reader takes the last one), or the line is added.
func setLine(text, key, value string) string {
	names := regexp.QuoteMeta(key)
	if key == "download_dir" {
		names = "(?:download_dir|incoming_dir)" // its old name
	}
	re := regexp.MustCompile(`(?im)^[ \t]*` + names + `[ \t]*=[^\r\n]*`)
	line := key + " = " + value
	if re.MatchString(text) {
		return re.ReplaceAllLiteralString(text, line)
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + line + "\n"
}
