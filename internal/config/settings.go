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
}

// Defaults returns the built-in settings (directories relative).
func Defaults() Settings {
	return Settings{
		Port:        4662,
		Nick:        "MyMule",
		TempDir:     "temp",
		DownloadDir: "downloads",
		MaxConns:    40,
	}
}

const settingsTemplate = `# MyMule configuration.
# One "key = value" per line; lines starting with # are comments.
# Relative paths are relative to this file's folder.
# Command-line flags (e.g. -port, -server) override these values.

# TCP port for incoming peer connections. Forward it in your router
# (TCP) to get HighID. 0 disables incoming connections.
port = %d

# Server to connect to, as ip:port. Leave empty to pick automatically
# from the server list ('mymule servers list'), preferring HighID.
server = %s

# Nickname shown to other users.
nick = %s

# Folder for partial downloads (.part files).
temp_dir = %s

# Folder for completed downloads.
download_dir = %s

# Maximum simultaneous peer connections.
max_conns = %d
`

// SettingsPath returns the path of mymule.conf.
func SettingsPath() (string, error) { return Path(SettingsFile) }

// LoadSettings reads mymule.conf, creating it with the defaults if it
// does not exist. Directory settings are returned as absolute paths.
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
		content := fmt.Sprintf(settingsTemplate, s.Port, s.Server, s.Nick, s.TempDir, s.DownloadDir, s.MaxConns)
		if werr := os.WriteFile(p, []byte(content), 0o644); werr != nil {
			return s, fmt.Errorf("cannot create %s (is the folder writable? set MYMULE_HOME to use another one): %w", p, werr)
		}
	case err != nil:
		return s, err
	default:
		if err := parseSettings(string(data), &s); err != nil {
			return s, fmt.Errorf("%s: %w", p, err)
		}
	}
	s.TempDir = absUnder(base, s.TempDir)
	s.DownloadDir = absUnder(base, s.DownloadDir)
	return s, nil
}

func absUnder(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func parseSettings(text string, s *Settings) error {
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
			return fmt.Errorf("line %d: expected key = value", line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "port":
			n, err := strconv.ParseUint(val, 10, 16)
			if err != nil {
				return fmt.Errorf("line %d: port must be 0-65535", line)
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
		case "download_dir", "incoming_dir":
			if val != "" {
				s.DownloadDir = val
			}
		case "max_conns":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return fmt.Errorf("line %d: max_conns must be a positive number", line)
			}
			s.MaxConns = n
		default:
			return fmt.Errorf("line %d: unknown setting %q", line, key)
		}
	}
	return sc.Err()
}
