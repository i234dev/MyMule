package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
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
}

// lockedSettings cannot be changed through the API.
var lockedSettings = map[string]bool{
	"api_listen": true, "api_key": true, "api_hosts": true, "network_hosts": true, "web_ui": true,
}

var boolSettings = map[string]bool{"kad": true, "upnp": true, "sharing": true, "share_downloads": true, "web_ui": true, "arr_support": true}
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
		_, si.Env = os.LookupEnv(EnvPrefix + strings.ToUpper(st.key))
		out = append(out, si)
	}
	return out, nil
}

// ErrLockedSetting is returned for a change to a locked setting.
var ErrLockedSetting = errors.New("this setting can only be changed in mymule.conf")

// UpdateSettings writes values (setting name -> value) into mymule.conf,
// keeping everything else in it as it is. The whole file must still read
// without error, or nothing is written. Changes take effect when MyMule
// restarts.
func UpdateSettings(values map[string]string) error {
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
	return fsutil.WriteFile(p, []byte(text), 0o600)
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
