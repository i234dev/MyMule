// Package library puts finished movie and series downloads in their
// folders, named with a template ("Title (Year)/Title (Year).mkv").
package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"mymule/internal/fsutil"
)

// Item is what we know about a download started from a movie or series.
type Item struct {
	Kind          string `json:"kind"` // movie or series
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title,omitempty"`
	Year          int    `json:"year,omitempty"`
	Season        int    `json:"season,omitempty"`
	Episode       int    `json:"episode,omitempty"`
	EpisodeTitle  string `json:"episode_title,omitempty"`
	Quality       string `json:"quality,omitempty"`  // 1080p...
	Language      string `json:"language,omitempty"` // ES, EN+ES...
	WorkID        string `json:"work_id,omitempty"`
}

// Settings say where and how finished files are organised.
type Settings struct {
	// Organize turns renaming on. Off, finished files keep their names
	// and stay in the download folder.
	Organize       bool   `json:"organize"`
	MoviesDir      string `json:"movies_dir"`      // empty = rename movies inside the download folder
	SeriesDir      string `json:"series_dir"`      // empty = rename episodes inside the download folder
	MovieTemplate  string `json:"movie_template"`  // see Presets
	SeriesTemplate string `json:"series_template"` // see Presets
	OriginalTitles bool   `json:"original_titles"` // name files with the original title
	Share          bool   `json:"share"`           // share the movie and series folders
}

// Presets offered in the web interface; the first one is the default.
var (
	MoviePresets = []string{
		"{title} ({year})/{title} ({year})",
		"{title} ({year})",
		"{year} - {title}",
		"{title} ({year}) [{quality}]",
	}
	SeriesPresets = []string{
		"{title}/Season {season:2}/{title} - S{season:2}E{episode:2}",
		"{title}/Temporada {season:2}/{title} - {season}x{episode:2}",
		"{title}/Season {season:2}/{title} - S{season:2}E{episode:2} - {episode_title}",
		"{title}/{title} - S{season:2}E{episode:2}",
	}
)

// DefaultSettings: organising is off until the user turns it on.
func DefaultSettings() Settings {
	return Settings{MovieTemplate: MoviePresets[0], SeriesTemplate: SeriesPresets[0], Share: true}
}

var tokenRe = regexp.MustCompile(`\{([a-z_]+)(?::([1-4]))?\}`)

var knownTokens = map[string]bool{"title": true, "original_title": true, "year": true, "quality": true,
	"language": true, "season": true, "episode": true, "episode_title": true}

// CheckTemplate reports problems with a template.
func CheckTemplate(t string, series bool) error {
	t = strings.TrimSpace(t)
	if t == "" {
		return errors.New("the template is empty")
	}
	if len(t) > 300 {
		return errors.New("the template is too long")
	}
	for _, m := range tokenRe.FindAllStringSubmatch(t, -1) {
		if !knownTokens[m[1]] {
			return fmt.Errorf("unknown tag {%s}", m[1])
		}
	}
	if !strings.Contains(t, "{title}") && !strings.Contains(t, "{original_title}") {
		return errors.New("the template needs {title} or {original_title}")
	}
	if series && !strings.Contains(t, "{episode") {
		return errors.New("a series template needs {episode}")
	}
	for _, seg := range strings.FieldsFunc(t, isSlash) {
		if s := strings.TrimSpace(seg); s == "." || s == ".." {
			return errors.New("the template can't contain . or .. folders")
		}
	}
	return nil
}

func isSlash(r rune) bool { return r == '/' || r == '\\' }

// Render fills a template for an item, giving a relative path without
// extension, safe to use as file and folder names on any system.
func Render(t string, it Item, originalTitles bool) string {
	title := it.Title
	if originalTitles && it.OriginalTitle != "" {
		title = it.OriginalTitle
	}
	value := func(name string, pad int) string {
		num := func(n int) string {
			if n <= 0 {
				return ""
			}
			s := strconv.Itoa(n)
			for len(s) < pad {
				s = "0" + s
			}
			return s
		}
		switch name {
		case "title":
			return title
		case "original_title":
			if it.OriginalTitle != "" {
				return it.OriginalTitle
			}
			return it.Title
		case "year":
			return num(it.Year)
		case "quality":
			return it.Quality
		case "language":
			return it.Language
		case "season":
			return num(it.Season)
		case "episode":
			return num(it.Episode)
		case "episode_title":
			return it.EpisodeTitle
		}
		return ""
	}
	var segs []string
	for _, seg := range strings.FieldsFunc(t, isSlash) {
		filled := tokenRe.ReplaceAllStringFunc(seg, func(tok string) string {
			m := tokenRe.FindStringSubmatch(tok)
			pad, _ := strconv.Atoi(m[2])
			// Path separators in values (e.g. "AC/DC") must not create folders.
			return strings.Map(func(r rune) rune {
				if isSlash(r) {
					return '-'
				}
				return r
			}, value(m[1], pad))
		})
		if s := tidy(fitName(tidy(sanitize(filled)))); s != "" {
			segs = append(segs, s)
		}
	}
	return strings.Join(segs, string(filepath.Separator))
}

// sanitize removes characters that aren't allowed in file names on
// Windows, macOS or Linux.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 32 || r == 127:
			return -1
		case strings.ContainsRune(`<>:"|?*`, r):
			return ' '
		}
		return r
	}, s)
	// Trailing dots and spaces are not allowed on Windows.
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	switch strings.ToUpper(strings.SplitN(s, ".", 2)[0]) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "LPT1", "LPT2", "LPT3":
		s = "_" + s
	}
	if s == "." || s == ".." {
		return ""
	}
	return s
}

// maxNameBytes is the longest file or folder name made from a template,
// leaving room under the usual 255-byte limit of file systems for the
// extension and a " (2)" added when the name is taken. Long titles
// (episode titles, some original titles) would otherwise make the move
// fail.
const maxNameBytes = 200

// fitName shortens s to maxNameBytes at most, without cutting a character.
func fitName(s string) string {
	if len(s) <= maxNameBytes {
		return s
	}
	cut := maxNameBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(strings.TrimSpace(s[:cut]), ". ")
}

var (
	emptyRe     = regexp.MustCompile(`\(\s*\)|\[\s*\]|\{\s*\}`)
	spacesRe    = regexp.MustCompile(`\s{2,}`)
	repeatedSep = regexp.MustCompile(`\s+-(\s+-)+\s+`)
	trailingSep = regexp.MustCompile(`\s+-\s*$`)
	leadingSep  = regexp.MustCompile(`^\s*-\s+`)
)

// tidy removes what is left of missing values: "Title ()" -> "Title",
// "Title -  - x" -> "Title - x", "Title - " -> "Title".
func tidy(s string) string {
	s = emptyRe.ReplaceAllString(s, "")
	s = spacesRe.ReplaceAllString(s, " ")
	s = repeatedSep.ReplaceAllString(s, " - ")
	s = trailingSep.ReplaceAllString(s, "")
	s = leadingSep.ReplaceAllString(s, "")
	return strings.TrimRight(strings.TrimSpace(s), ". ")
}

// Plan says where a finished file src would go: the folder (base) and the
// path under it (rel, with the extension). ok is false when the file stays
// as it is: organising is off, or it is not a video, audio or subtitle
// file (see IsMedia).
//
// Without a folder for its kind the file is renamed where it is (base is
// src's folder) and only the template's last part is used, so it does not
// go into subfolders: the download folder is shared without them.
func Plan(st Settings, it Item, src string) (base, rel string, ok bool) {
	if !st.Organize || !IsMedia(src) {
		return "", "", false
	}
	base, tmpl := st.MoviesDir, st.MovieTemplate
	if it.Kind == "series" {
		base, tmpl = st.SeriesDir, st.SeriesTemplate
	}
	inPlace := base == ""
	if inPlace {
		base = filepath.Dir(src)
	}
	ext := strings.ToLower(filepath.Ext(src))
	rel = Render(tmpl, it, st.OriginalTitles)
	if it.Kind == "series" && it.Episode == 0 {
		// A whole season in one file: the season's folder, original name.
		rel = filepath.Join(filepath.Dir(rel), filepath.Base(strings.TrimSuffix(src, filepath.Ext(src))))
	}
	if inPlace {
		rel = filepath.Base(rel)
	}
	if rel == "" || rel == "." || rel == string(filepath.Separator) {
		return "", "", false
	}
	// A season's file keeps its download name, which may be long too.
	if last := filepath.Base(rel); len(last) > maxNameBytes {
		rel = filepath.Join(filepath.Dir(rel), tidy(fitName(last)))
	}
	rel += ext
	// Never outside the chosen folder.
	dst := filepath.Join(base, rel)
	if r, err := filepath.Rel(filepath.Clean(base), dst); err != nil || r == ".." ||
		strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	return base, rel, true
}

// Destination returns where a finished file goes, or "" when it should
// stay as it is (see Plan).
func Destination(st Settings, it Item, src string) string {
	base, rel, ok := Plan(st, it, src)
	if !ok {
		return ""
	}
	return filepath.Join(base, rel)
}

// Move moves src to dst, creating folders. If dst exists, " (2)", " (3)"...
// is added. It works across disks (copy, then delete). Returns the final path.
func Move(src, dst string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	// Claim the name with an exclusive create, so two files moved at the
	// same time to the same name cannot both get it; then rename over it.
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for i := 2; ; i++ {
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if i > 99 {
			return "", fmt.Errorf("too many files named %s", filepath.Base(stem))
		}
		dst = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	}
	// Different disk: copy and remove.
	in, err := os.Open(src)
	if err != nil {
		os.Remove(dst) // the reserved, still empty name
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.mymule-tmp")
	if err != nil {
		os.Remove(dst)
		return "", err
	}
	tmp := out.Name()
	out.Chmod(0o644)
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		os.Remove(dst)
		return "", err
	}
	in.Close()
	os.Remove(src)
	return dst, nil
}

// ---- pending items ----

// Store remembers which downloads are movies or episodes (library.json),
// until they finish.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]Item // by ed2k hash
}

// OpenStore loads the store.
func OpenStore(path string) *Store {
	s := &Store{path: path, data: map[string]Item{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.data)
	}
	return s
}

// Put records an item for a download.
func (s *Store) Put(hash string, it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[strings.ToUpper(hash)] = it
	s.saveLocked()
}

// Take returns and forgets the item of a download.
func (s *Store) Take(hash string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToUpper(hash)
	it, ok := s.data[k]
	if ok {
		delete(s.data, k)
		s.saveLocked()
	}
	return it, ok
}

// Get returns the item of a download.
func (s *Store) Get(hash string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.data[strings.ToUpper(hash)]
	return it, ok
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.data, "", " ")
	if err != nil {
		return
	}
	fsutil.WriteFile(s.path, b, 0o600)
}

// mediaExt lists the file types that belong in a movie or series library:
// video, audio and subtitles. Only these are moved into the library
// folders and shared from them. Library folders can be changed with the
// api_key and receive files named by the network, so anything else (a
// script, a shortcut, a document) must never end up there or be
// published from there.
var mediaExt = map[string]bool{
	// video
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true,
	".mpg": true, ".mpeg": true, ".m2ts": true, ".mts": true, ".ts": true, ".vob": true,
	".webm": true, ".flv": true, ".ogm": true, ".ogv": true, ".divx": true, ".3gp": true,
	// audio
	".mp3": true, ".flac": true, ".aac": true, ".ac3": true, ".dts": true, ".m4a": true,
	".mka": true, ".ogg": true, ".opus": true, ".wav": true, ".wma": true,
	// subtitles
	".srt": true, ".sub": true, ".idx": true, ".ass": true, ".ssa": true, ".vtt": true, ".sup": true,
}

// IsMedia reports whether name is a video, audio or subtitle file.
func IsMedia(name string) bool {
	return mediaExt[strings.ToLower(filepath.Ext(name))]
}
