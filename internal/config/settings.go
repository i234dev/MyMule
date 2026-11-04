package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SettingsFile is the name of the configuration file in the base directory.
const SettingsFile = "mymule.conf"

// Settings is the user configuration. Command-line flags override it.
type Settings struct {
	Port        uint16 // TCP listen port for peers; 0 disables incoming connections
	Server      string // ip:port to connect to; empty = pick from the server list
	Nick        string
	TempDir     string // absolute after Load
	DownloadDir string // absolute after Load
	MaxConns    int

	Sharing        bool     // master switch: share and upload at all
	SharedDirs     []string // extra shared folders, absolute after Load
	ShareDownloads bool     // share DownloadDir too
	UploadSlots    int
	UploadLimit    int // KB/s, 0 = unlimited
}

// Defaults returns the built-in settings (directories relative).
func Defaults() Settings {
	return Settings{
		Port:           4662,
		Nick:           "MyMule",
		TempDir:        "temp",
		DownloadDir:    "downloads",
		MaxConns:       40,
		Sharing:        true,
		ShareDownloads: true,
		UploadSlots:    3,
		UploadLimit:    100,
	}
}

// setting describes one key of mymule.conf: its comment, how to render
// its default and whether it exists in a given file.
type setting struct {
	key     string
	comment string
	value   func(Settings) string
}

var settingsDoc = []setting{
	{"port", "TCP port for incoming peer connections. Forward it in your router\n(TCP) to get HighID. 0 disables incoming connections.",
		func(s Settings) string { return strconv.Itoa(int(s.Port)) }},
	{"server", "Server to connect to, as ip:port. Leave empty to pick automatically\nfrom the server list ('mymule servers list'), preferring HighID.",
		func(s Settings) string { return s.Server }},
	{"nick", "Nickname shown to other users.", func(s Settings) string { return s.Nick }},
	{"temp_dir", "Folder for partial downloads (.part files).", func(s Settings) string { return s.TempDir }},
	{"download_dir", "Folder for completed downloads.", func(s Settings) string { return s.DownloadDir }},
	{"max_conns", "Maximum simultaneous peer connections.", func(s Settings) string { return strconv.Itoa(s.MaxConns) }},
	{"sharing", "Share files with other users at all: true or false. With false,\nnothing is shared or uploaded (other users give you lower priority).",
		func(s Settings) string { return strconv.FormatBool(s.Sharing) }},
	{"share_downloads", "Share the files in download_dir with other users (like eMule's\nIncoming folder): true or false. Partial downloads are always shared,\nbut only the parts already verified.",
		func(s Settings) string { return strconv.FormatBool(s.ShareDownloads) }},
	{"shared_dirs", "More folders to share, separated by ';'. Subfolders are not included.\nExample: shared_dirs = D:\\Linux ISOs; E:\\Free books",
		func(s Settings) string { return strings.Join(s.SharedDirs, "; ") }},
	{"upload_slots", "How many peers can download from you at the same time; the rest\nwait in a queue and take turns.",
		func(s Settings) string { return strconv.Itoa(s.UploadSlots) }},
	{"upload_limit", "Upload speed limit in KB/s (0 = no limit). Using all your upload\nbandwidth also slows down your downloads.",
		func(s Settings) string { return strconv.Itoa(s.UploadLimit) }},
}

func renderSetting(st setting, s Settings) string {
	var b strings.Builder
	for _, l := range strings.Split(st.comment, "\n") {
		b.WriteString("# " + l + "\n")
	}
	b.WriteString(st.key + " = " + st.value(s) + "\n")
	return b.String()
}

const settingsHeader = `# MyMule configuration.
# One "key = value" per line; lines starting with # are comments.
# Relative paths are relative to this file's folder.
# Command-line flags (e.g. -port, -server) override these values.
`

// SettingsPath returns the path of mymule.conf.
func SettingsPath() (string, error) { return Path(SettingsFile) }

// LoadSettings reads mymule.conf, creating it with the defaults if it
// does not exist. Settings added in newer versions are appended (with
// their comments) to an existing file so the user can see them.
// Directory settings are returned as absolute paths.
func LoadSettings() (Settings, error) {
	s := Defaults()
	base, err := Dir()
	if err != nil {
		return s, err
	}
	p := filepath.Join(base, SettingsFile)
	data, err := os.ReadFile(p)
	switch {
	case os.IsNotExist(err):
		var b strings.Builder
		b.WriteString(settingsHeader)
		for _, st := range settingsDoc {
			b.WriteString("\n" + renderSetting(st, s))
		}
		if werr := os.WriteFile(p, []byte(b.String()), 0o644); werr != nil {
			return s, fmt.Errorf("cannot create %s (is the folder writable? set MYMULE_HOME to use another one): %w", p, werr)
		}
	case err != nil:
		return s, err
	default:
		present, err := parseSettings(string(data), &s)
		if err != nil {
			return s, fmt.Errorf("%s: %w", p, err)
		}
		var add strings.Builder
		for _, st := range settingsDoc {
			if !present[st.key] {
				add.WriteString("\n" + renderSetting(st, Defaults()))
			}
		}
		if add.Len() > 0 {
			text := string(data)
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += "\n# --- settings added by a newer MyMule version ---\n" + add.String()
			os.WriteFile(p, []byte(text), 0o644) // best effort
		}
	}
	s.TempDir = absUnder(base, s.TempDir)
	s.DownloadDir = absUnder(base, s.DownloadDir)
	for i, d := range s.SharedDirs {
		s.SharedDirs[i] = absUnder(base, d)
	}
	return s, nil
}

func absUnder(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// parseSettings applies text to s and reports which keys it contained.
func parseSettings(text string, s *Settings) (map[string]bool, error) {
	present := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	line := 0
	for sc.Scan() {
		line++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		key, val, ok := strings.Cut(l, "=")
		if !ok {
			return present, fmt.Errorf("line %d: expected key = value", line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.Trim(strings.TrimSpace(val), `"`)
		if key == "incoming_dir" {
			key = "download_dir"
		}
		present[key] = true
		switch key {
		case "port":
			n, err := strconv.ParseUint(val, 10, 16)
			if err != nil {
				return present, fmt.Errorf("line %d: port must be 0-65535", line)
			}
			s.Port = uint16(n)
		case "server":
			s.Server = val
		case "nick":
			if val != "" {
				s.Nick = val
			}
		case "temp_dir":
			if val != "" {
				s.TempDir = val
			}
		case "download_dir":
			if val != "" {
				s.DownloadDir = val
			}
		case "max_conns":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return present, fmt.Errorf("line %d: max_conns must be a positive number", line)
			}
			s.MaxConns = n
		case "sharing":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return present, fmt.Errorf("line %d: sharing must be true or false", line)
			}
			s.Sharing = b
		case "share_downloads":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return present, fmt.Errorf("line %d: share_downloads must be true or false", line)
			}
			s.ShareDownloads = b
		case "shared_dirs":
			s.SharedDirs = nil
			for _, d := range strings.Split(val, ";") {
				if d = strings.Trim(strings.TrimSpace(d), `"`); d != "" {
					s.SharedDirs = append(s.SharedDirs, d)
				}
			}
		case "upload_slots":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > 50 {
				return present, fmt.Errorf("line %d: upload_slots must be between 1 and 50", line)
			}
			s.UploadSlots = n
		case "upload_limit":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return present, fmt.Errorf("line %d: upload_limit must be 0 (no limit) or a speed in KB/s", line)
			}
			s.UploadLimit = n
		default:
			return present, fmt.Errorf("line %d: unknown setting %q", line, key)
		}
	}
	return present, sc.Err()
}
