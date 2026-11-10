package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mymule/internal/fsutil"
	"mymule/internal/mediaserver"
	"mymule/internal/notify"
)

// SettingsFile is the name of the configuration file in the base directory.
const SettingsFile = "mymule.conf"

// Settings is the user configuration. Command-line flags override it.
type Settings struct {
	Port uint16 // TCP listen port for peers; 0 disables incoming connections

	// Kad: eMule's serverless network (on by default).
	Kad     bool
	KadPort uint16 // UDP port
	// KadNodesURL is a nodes.dat to start from when no Kad contact is
	// known; empty = only from other clients.
	KadNodesURL string
	// UPnP opens port and kad_port on the router (UPnP or NAT-PMP).
	UPnP bool
	// IPFilterURL is an IP filter list downloaded weekly into
	// ipfilter.dat; empty = only a list put there by hand.
	IPFilterURL string
	Server      string // ip:port to connect to; empty = pick from the server list
	Nick        string
	TempDir     string // absolute after Load
	DownloadDir string // absolute after Load
	MaxConns    int
	// Obfuscation of peer connections: "true" (when the peer supports
	// it), "required" (only obfuscated connections) or "false".
	Obfuscation string

	Sharing        bool     // master switch: share and upload at all
	SharedDirs     []string // extra shared folders, absolute after Load
	ShareDownloads bool     // share DownloadDir too
	UploadSlots    int
	UploadLimit    int // KB/s, 0 = unlimited
	DownloadLimit  int // KB/s, 0 = unlimited
	// From ScheduleFrom to ScheduleTo ("HH:MM", every day; empty = off),
	// these limits are used instead (see LimitsAt).
	ScheduleFrom, ScheduleTo                   string
	ScheduleUploadLimit, ScheduleDownloadLimit int

	APIListen string // host:port of the HTTP API; empty disables it
	APIKey    string // required by every API request
	// APIHosts are extra host names the API answers to (besides IP
	// addresses, localhost and this computer's name).
	APIHosts []string

	// NetworkHosts are the computers whose Windows network folders
	// (\\host\share) may be library folders. Others are refused: just
	// opening such a folder sends Windows' login hash to that computer.
	NetworkHosts []string

	// WebUI serves the web interface on api_listen. Off by default.
	WebUI bool

	// ArrSupport turns on the Torznab indexer and the qBittorrent-
	// compatible API for Radarr, Sonarr and the like. Off by default.
	ArrSupport bool

	// TMDBKey is the TMDB API key for movie/series data and posters in the
	// web interface: "" = MyMule's own key, "off" = don't use TMDB.
	TMDBKey string

	// SearchLanguages are language codes (e.g. "es") whose titles the
	// *arr indexer also searches for, looked up on Wikidata. Empty = off.
	SearchLanguages []string

	// NotifyURLs are where notifications go (ntfy://, tgram://,
	// discord://, http(s):// webhooks), separated by spaces or commas;
	// NotifyOn the events to send (notify.AllKinds).
	NotifyURLs string
	NotifyOn   []string

	// MediaServer (jellyfin, emby, plex or empty) is asked to scan its
	// libraries when MyMule puts new files in them.
	MediaServer      string
	MediaServerURL   string
	MediaServerToken string
}

// Defaults returns the built-in settings (directories relative).
func Defaults() Settings {
	return Settings{
		Port:           4662,
		Kad:            true,
		KadPort:        4672,
		KadNodesURL:    DefaultKadNodesURL,
		UPnP:           true,
		Nick:           "MyMule",
		TempDir:        "temp",
		DownloadDir:    "downloads",
		MaxConns:       40,
		Obfuscation:    "true",
		Sharing:        true,
		ShareDownloads: true,
		UploadSlots:    20,
		UploadLimit:    1000,
		APIListen:      "127.0.0.1:4711",
		NotifyOn:       notify.AllKinds,
	}
}

// MinDownloadLimit is the lowest download limit (KB/s): about 1 KB/s for
// each of the connections a download may have.
const MinDownloadLimit = 32

// DefaultKadNodesURL is where a first nodes.dat comes from (the one eMule
// offers), over HTTPS: the file decides which nodes we first talk to.
const DefaultKadNodesURL = "https://upd.emule-security.org/nodes.dat"

// oldKadNodesURL is the default of earlier versions, written into their
// mymule.conf: read as the HTTPS one.
const oldKadNodesURL = "http://upd.emule-security.org/nodes.dat"

// NewAPIKey returns a random key for the HTTP API.
func NewAPIKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
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
	{"kad", "Kad, eMule's network without servers: true or false. It finds files\nand sources that servers do not know.",
		func(s Settings) string { return strconv.FormatBool(s.Kad) }},
	{"kad_port", "UDP port for Kad. Forward it in your router (UDP) so that other Kad\nnodes can reach you; without it Kad still works, with less.",
		func(s Settings) string { return strconv.Itoa(int(s.KadPort)) }},
	{"kad_nodes_url", "A nodes.dat (list of Kad nodes) to start from when MyMule knows none,\nas http:// or https:// URL. Empty = start only from other clients.",
		func(s Settings) string { return s.KadNodesURL }},
	{"upnp", "Open port (TCP) and kad_port (UDP) on the router automatically, with\nUPnP or NAT-PMP, when the router allows it; they are closed again when\nMyMule stops. false = forward them by hand.",
		func(s Settings) string { return strconv.FormatBool(s.UPnP) }},
	{"ip_filter_url", "An IP filter list to download every week into ipfilter.dat in the data\nfolder, as http:// or https:// URL: eMule's ipfilter.dat or the P2P\nformat, plain, gzip or zip. MyMule neither connects to nor takes\nconnections from the addresses in it. Empty = only use an ipfilter.dat\nyou put in the data folder yourself (none by default).",
		func(s Settings) string { return s.IPFilterURL }},
	{"server", "Server to connect to, as ip:port. Leave empty to pick automatically\nfrom the server list ('mymule servers list'), preferring HighID.",
		func(s Settings) string { return s.Server }},
	{"nick", "Nickname shown to other users.", func(s Settings) string { return s.Nick }},
	{"temp_dir", "Folder for partial downloads (.part files).", func(s Settings) string { return s.TempDir }},
	{"download_dir", "Folder for completed downloads.", func(s Settings) string { return s.DownloadDir }},
	{"max_conns", "Maximum simultaneous peer connections.", func(s Settings) string { return strconv.Itoa(s.MaxConns) }},
	{"obfuscation", "Protocol obfuscation: connections to other users look like random\ndata, which hides eMule traffic from internet providers that slow it\ndown. true = obfuscate with every user that supports it; required =\nonly obfuscated connections (fewer sources: older clients cannot connect);\nfalse = never. It does not make you anonymous.",
		func(s Settings) string { return s.Obfuscation }},
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
	{"download_limit", "Download speed limit in KB/s (0 = no limit, the default). At least\n32 KB/s: shared by every peer you download from, a lower limit would\nleave each of them too little to keep its place.",
		func(s Settings) string { return strconv.Itoa(s.DownloadLimit) }},
	{"schedule_from", "Speed schedule: from this time (HH:MM, e.g. 01:00) to schedule_to, every\nday, the schedule_ limits below are used instead of the ones above (for\nexample, no limit at night). Empty = off.",
		func(s Settings) string { return s.ScheduleFrom }},
	{"schedule_to", "End of the speed schedule (HH:MM, e.g. 08:00); it may be the next day.",
		func(s Settings) string { return s.ScheduleTo }},
	{"schedule_upload_limit", "Upload speed limit in KB/s during the schedule (0 = no limit).",
		func(s Settings) string { return strconv.Itoa(s.ScheduleUploadLimit) }},
	{"schedule_download_limit", "Download speed limit in KB/s during the schedule (0 = no limit, else at\nleast 32).",
		func(s Settings) string { return strconv.Itoa(s.ScheduleDownloadLimit) }},
	{"api_listen", "HTTP API for scripts and for apps like Radarr, Sonarr and Prowlarr\n(see docs/API.md). 127.0.0.1:4711 = only this computer.\n0.0.0.0:4711 = every device on your network: use it only on a trusted\nnetwork and never forward this port in your router. Empty = disabled.",
		func(s Settings) string { return s.APIListen }},
	{"api_key", "Key that every API request must send (X-Api-Key header or ?apikey=).\nGenerated randomly for you; you can replace it with any long random text.",
		func(s Settings) string { return s.APIKey }},
	{"api_hosts", "The API and web interface answer to IP addresses, localhost and this\ncomputer's name. If you reach them by another name (a NAS name, or a domain\nbehind a reverse proxy), list it here, separated by commas. Other names\nare refused, which stops web pages from reaching the API through DNS tricks.",
		func(s Settings) string { return strings.Join(s.APIHosts, ", ") }},
	{"network_hosts", "Windows only: computers whose network folders (\\\\nas\\Movies) may be\nlibrary folders in the web interface, separated by commas (e.g. nas,\n192.168.0.10). Others are refused, because just opening a network folder\nsends your Windows login hash to that computer. Mapped drives (Z:) are\nnot affected.",
		func(s Settings) string { return strings.Join(s.NetworkHosts, ", ") }},
	{"web_ui", "Web interface in your browser, on the api_listen address\n(e.g. http://127.0.0.1:4711/). Log in with api_key. true or false.",
		func(s Settings) string { return strconv.FormatBool(s.WebUI) }},
	{"tmdb_api_key", "Movie and series data and posters for the web interface come from TMDB\n(themoviedb.org). Empty = use MyMule's own key. Put your own key here,\nor off to use only Wikidata (no posters).",
		func(s Settings) string { return s.TMDBKey }},
	{"arr_support", "Radarr, Sonarr, Lidarr, Readarr and Prowlarr support (see docs/ARR.md):\ntrue serves a Torznab indexer and a qBittorrent-compatible API on\napi_listen. false = off.",
		func(s Settings) string { return strconv.FormatBool(s.ArrSupport) }},
	{"notify_urls", "Notifications when a download finishes, the wish list finds something,\nor something fails. Where to send them, separated by spaces:\nntfy://topic (ntfy.sh) or ntfy://your-server/topic, tgram://BOT_TOKEN/CHAT_ID\n(Telegram), discord://WEBHOOK_ID/WEBHOOK_TOKEN, or any https:// address\n(gets a JSON POST). Empty = no notifications.",
		func(s Settings) string { return s.NotifyURLs }},
	{"notify_on", "Which notifications to send, separated by commas: completed, wanted\n(the wish list found a file), error.",
		func(s Settings) string { return strings.Join(s.NotifyOn, ", ") }},
	{"media_server", "Your media server, asked to scan its libraries when MyMule adds files to\nthem: jellyfin, emby, plex, or empty (none).",
		func(s Settings) string { return s.MediaServer }},
	{"media_server_url", "Its address, e.g. http://192.168.0.10:8096 (Jellyfin/Emby) or\nhttp://192.168.0.10:32400 (Plex).",
		func(s Settings) string { return s.MediaServerURL }},
	{"media_server_token", "An API key made in Jellyfin/Emby (Dashboard > API Keys), or your Plex\ntoken.",
		func(s Settings) string { return s.MediaServerToken }},
	{"search_languages", "With arr_support: Radarr/Sonarr only search by the original or English\ntitle. List language codes here (e.g. es, or es, fr) to also search by the title in\nthose languages, looked up on Wikidata (only the IMDb/TMDB/TVDB id of the\nmovie or show is sent). Empty = off, no lookups.",
		func(s Settings) string { return strings.Join(s.SearchLanguages, ", ") }},
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
	TightenPermissions(base)
	p := filepath.Join(base, SettingsFile)
	data, err := os.ReadFile(p)
	switch {
	case os.IsNotExist(err):
		s.APIKey = NewAPIKey()
		var b strings.Builder
		b.WriteString(settingsHeader)
		for _, st := range settingsDoc {
			b.WriteString("\n" + renderSetting(st, s))
		}
		if werr := fsutil.WriteFile(p, []byte(b.String()), 0o600); werr != nil {
			return s, fmt.Errorf("cannot create %s (is the folder writable? set MYMULE_HOME to use another one): %w", p, werr)
		}
	case err != nil:
		return s, err
	default:
		present, err := parseSettings(string(data), &s)
		if err != nil {
			return s, fmt.Errorf("%s: %w", p, err)
		}
		text := string(data)
		changed := false
		if s.APIKey == "" {
			s.APIKey = NewAPIKey()
			if present["api_key"] {
				// Present but empty: fill it in place.
				text = emptyKeyLine.ReplaceAllString(text, "${1}api_key = "+s.APIKey)
				changed = true
			}
		}
		var add strings.Builder
		for _, st := range settingsDoc {
			if !present[st.key] {
				add.WriteString("\n" + renderSetting(st, s))
			}
		}
		if add.Len() > 0 {
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += "\n# --- settings added by a newer MyMule version ---\n" + add.String()
			changed = true
		}
		if changed {
			fsutil.WriteFile(p, []byte(text), 0o600) // best effort
		}
	}
	if err := applyEnv(&s, os.LookupEnv); err != nil {
		return s, err
	}
	if err := checkSettings(s); err != nil {
		return s, fmt.Errorf("%s: %w", p, err)
	}
	s.TempDir = absUnder(base, s.TempDir)
	s.DownloadDir = absUnder(base, s.DownloadDir)
	for i, d := range s.SharedDirs {
		s.SharedDirs[i] = absUnder(base, d)
	}
	return s, nil
}

// EnvPrefix starts the environment variables that override mymule.conf:
// MYMULE_ plus the setting's name in capitals (MYMULE_WEB_UI=true).
const EnvPrefix = "MYMULE_"

// applyEnv applies MYMULE_<SETTING> environment variables on top of
// mymule.conf, the way command-line flags do: they win over the file and
// are not written into it. This is how containers configure MyMule. An
// empty MYMULE_API_KEY is ignored, so it never leaves the API without a
// key.
func applyEnv(s *Settings, lookup func(string) (string, bool)) error {
	for _, st := range settingsDoc {
		name := EnvPrefix + strings.ToUpper(st.key)
		v, ok := lookup(name)
		// Empty as the file reader sees it: quotes and spaces trimmed
		// ('"', '""', '" "' leave no key at all).
		if !ok || (st.key == "api_key" && strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"`)) == "") {
			continue
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("environment variable %s: must be a single line", name)
		}
		if _, err := parseSettings(st.key+" = "+v, s); err != nil {
			return fmt.Errorf("environment variable %s: %s", name, strings.TrimPrefix(err.Error(), "line 1: "))
		}
	}
	return nil
}

var emptyKeyLine = regexp.MustCompile(`(?m)^(\s*)api_key\s*=\s*"?"?\s*$`)

// tmdbKeyRe matches a TMDB v3 API key.
var tmdbKeyRe = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// languageCodeRe accepts codes like "es", "pt-br" or "zh-hans" (Wikidata's).
var languageCodeRe = regexp.MustCompile(`^[a-z]{2,3}(-[a-z]{2,4})?$`)

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
		case "kad":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return present, fmt.Errorf("line %d: kad must be true or false", line)
			}
			s.Kad = b
		case "upnp":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return present, fmt.Errorf("line %d: upnp must be true or false", line)
			}
			s.UPnP = b
		case "kad_port":
			n, err := strconv.ParseUint(val, 10, 16)
			if err != nil || n == 0 {
				return present, fmt.Errorf("line %d: kad_port must be 1-65535", line)
			}
			s.KadPort = uint16(n)
		case "kad_nodes_url":
			if val != "" {
				u, err := url.Parse(val)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return present, fmt.Errorf("line %d: kad_nodes_url must be an http:// or https:// URL, or empty", line)
				}
			}
			if val == oldKadNodesURL {
				val = DefaultKadNodesURL
			}
			s.KadNodesURL = val
		case "ip_filter_url":
			if val != "" {
				u, err := url.Parse(val)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return present, fmt.Errorf("line %d: ip_filter_url must be an http:// or https:// URL, or empty", line)
				}
			}
			s.IPFilterURL = val
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
		case "obfuscation":
			switch v := strings.ToLower(val); v {
			case "true", "false", "required":
				s.Obfuscation = v
			default:
				return present, fmt.Errorf("line %d: obfuscation must be true, required or false", line)
			}
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
		case "api_hosts":
			s.APIHosts = nil
			for _, h := range strings.Split(val, ",") {
				if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
					s.APIHosts = append(s.APIHosts, h)
				}
			}
		case "network_hosts":
			s.NetworkHosts = nil
			for _, h := range strings.Split(val, ",") {
				h = strings.ToLower(strings.Trim(strings.TrimSpace(h), `\/[]`))
				if i := strings.IndexByte(h, '@'); i >= 0 {
					h = h[:i] // a WebDAV address (nas@SSL@443): its computer
				}
				if h != "" {
					s.NetworkHosts = append(s.NetworkHosts, h)
				}
			}
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
		case "api_listen":
			if val != "" {
				if _, port, err := net.SplitHostPort(val); err != nil || port == "" {
					return present, fmt.Errorf("line %d: api_listen must be host:port (e.g. 127.0.0.1:4711) or empty", line)
				}
			}
			s.APIListen = val
		case "api_key":
			if val != "" && len(val) < 12 {
				return present, fmt.Errorf("line %d: api_key is too short (use at least 12 characters)", line)
			}
			s.APIKey = val
		case "upload_limit":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return present, fmt.Errorf("line %d: upload_limit must be 0 (no limit) or a speed in KB/s", line)
			}
			s.UploadLimit = n
		case "download_limit":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || (n > 0 && n < MinDownloadLimit) {
				return present, fmt.Errorf("line %d: download_limit must be 0 (no limit) or at least %d (KB/s)", line, MinDownloadLimit)
			}
			s.DownloadLimit = n
		case "web_ui":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return present, fmt.Errorf("line %d: web_ui must be true or false", line)
			}
			s.WebUI = b
		case "tmdb_api_key":
			if val != "" && val != "off" && !tmdbKeyRe.MatchString(val) {
				return present, fmt.Errorf("line %d: tmdb_api_key must be a TMDB API key (32 letters and digits), empty or off", line)
			}
			s.TMDBKey = val
		case "arr_support":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return present, fmt.Errorf("line %d: arr_support must be true or false", line)
			}
			s.ArrSupport = b
		case "search_languages":
			s.SearchLanguages = nil
			for _, l := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
				l = strings.ToLower(l)
				if !languageCodeRe.MatchString(l) {
					return present, fmt.Errorf("line %d: search_languages: %q is not a language code (e.g. es, fr, pt-br)", line, l)
				}
				s.SearchLanguages = append(s.SearchLanguages, l)
			}
		case "schedule_from", "schedule_to":
			if val != "" {
				if _, err := parseClock(val); err != nil {
					return present, fmt.Errorf("line %d: %s must be a time like 01:00, or empty", line, key)
				}
			}
			if key == "schedule_from" {
				s.ScheduleFrom = val
			} else {
				s.ScheduleTo = val
			}
		case "schedule_upload_limit", "schedule_download_limit":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || (key == "schedule_download_limit" && n > 0 && n < MinDownloadLimit) {
				return present, fmt.Errorf("line %d: %s must be 0 (no limit) or a speed in KB/s (downloads: at least %d)", line, key, MinDownloadLimit)
			}
			if key == "schedule_upload_limit" {
				s.ScheduleUploadLimit = n
			} else {
				s.ScheduleDownloadLimit = n
			}
		case "notify_urls":
			if err := notify.Parse(val); err != nil {
				return present, fmt.Errorf("line %d: notify_urls: %v", line, err)
			}
			s.NotifyURLs = val
		case "notify_on":
			kinds, err := notify.ParseKinds(val)
			if err != nil {
				return present, fmt.Errorf("line %d: notify_on: %v", line, err)
			}
			s.NotifyOn = kinds
		case "media_server":
			s.MediaServer = strings.ToLower(val)
		case "media_server_url":
			s.MediaServerURL = val
		case "media_server_token":
			s.MediaServerToken = val
		default:
			return present, fmt.Errorf("line %d: unknown setting %q", line, key)
		}
	}
	return present, sc.Err()
}

// checkSettings checks what depends on several settings, once all are read
// (from the file and the environment).
func checkSettings(s Settings) error {
	if (s.ScheduleFrom == "") != (s.ScheduleTo == "") {
		return fmt.Errorf("schedule_from and schedule_to go together: set both, or neither")
	}
	return mediaserver.Check(s.MediaServer, s.MediaServerURL, s.MediaServerToken)
}
