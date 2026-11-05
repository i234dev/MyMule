// Package release reads what a file name says about a movie or episode
// (languages, resolution, source, year, season/episode), checks whether
// it belongs to a given work and scores it against the user's
// preferences.
package release

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Language is a language tag found in file names.
type Language struct {
	Code string         // es, en, fr...
	Word string         // the English name the *arr apps recognise
	Re   *regexp.Regexp // abbreviations and names used in file names
}

// wordRe matches one of the words as a whole word (letters only count as
// word characters, so "Spa.Eng" and "[spa,eng]" both match).
func wordRe(words string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` + words + `)(?:[^\p{L}]|$)`)
}

// Languages that can be recognised. Not "por" for Portuguese: it is also
// a common Spanish word ("... por Fulano").
var Languages = []Language{
	{"es", "SPANISH", wordRe(`spa|esp|castellano|español|espanol|spanish`)},
	{"en", "ENGLISH", wordRe(`eng|ing|english|ingles|inglés`)},
	{"fr", "FRENCH", wordRe(`fre|fra|vff|vfq|truefrench|french|français|francais`)},
	{"de", "GERMAN", wordRe(`ger|deu|deutsch|german|alemán|aleman`)},
	{"it", "ITALIAN", wordRe(`ita|italiano|italian`)},
	{"ca", "CATALAN", wordRe(`cat|català|catala|catalan`)},
	{"pt", "PORTUGUESE", wordRe(`portugues|português|portuguese|dublado`)},
}

var (
	latinoRe   = wordRe(`latino|lat|latinoamericano|spanish latin`)
	subsRe     = wordRe(`vose|vos|vosi|subs|sub|subtitulado|subtitulos|subtítulos|subtitles|subbed|vost|vostfr`)
	dualRe     = wordRe(`dual|multi`)
	yearRe     = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)[0-9]{2})(?:[^0-9p]|$)`)
	episodeRe  = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[ ._-]?e(\d{1,3})(?:[^0-9]|$)|(?:^|[^0-9])(\d{1,2})x(\d{2,3})(?:[^0-9]|$)|(?i)cap(?:itulo|ítulo)?[ ._-]*(\d)(\d{2})(?:[^0-9]|$)`)
	seasonRe   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:s|season|temporada|temp)[ ._-]?(\d{1,2})(?:[^0-9]|$)`)
	bridgedRe  = regexp.MustCompile(`(?i)^\s*\[(kad|torrent|usenet)\b[^\]]*\]\s*`)
	prefixRe   = regexp.MustCompile(`(?i)^\s*\[(?:kad|torrent|usenet|ed2k|emule)\b[^\]]*\]\s*`)
	resolution = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"2160p", regexp.MustCompile(`(?i)2160p|\b4k\b|\buhd\b`)},
		{"1080p", regexp.MustCompile(`(?i)1080[pi]|fullhd|full hd|\bfhd\b|microhd`)},
		{"720p", regexp.MustCompile(`(?i)720p|\bhd720\b|\bhdrip\b`)},
		{"480p", regexp.MustCompile(`(?i)480p|576p|\bsd\b|dvdrip|xvid|divx|\brip\b`)},
	}
	sources = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"cam", wordRe(`cam|camrip|hdcam|ts|telesync|hdts|tc|telecine`)},
		{"screener", wordRe(`screener|scr|dvdscr|bdscr|r5`)},
		{"remux", wordRe(`remux|bdremux`)},
		{"bluray", wordRe(`bluray|blu-ray|bdrip|brrip|bd|bdr|m2ts`)},
		{"web", wordRe(`web|web-dl|webdl|webrip|amzn|nf|hmax|dsnp`)},
		{"hdtv", wordRe(`hdtv|tvrip|dvb|satrip|dtt`)},
		{"dvd", wordRe(`dvd|dvdrip|dvd5|dvd9|vhsrip|vhs`)},
	}
	videoExts = map[string]bool{".mkv": true, ".avi": true, ".mp4": true, ".m4v": true, ".mpg": true,
		".mpeg": true, ".ts": true, ".wmv": true, ".rmvb": true, ".divx": true, ".mov": true, ".iso": true}
)

// Info is what a file name says.
type Info struct {
	Languages  []string `json:"languages,omitempty"` // es, en... (es-419 for "latino")
	Subs       bool     `json:"subs,omitempty"`      // subtitles mentioned (VOSE...)
	Dual       bool     `json:"dual,omitempty"`      // "dual"/"multi" audio
	Resolution string   `json:"resolution,omitempty"`
	Source     string   `json:"source,omitempty"`
	Year       int      `json:"year,omitempty"`
	Season     int      `json:"season,omitempty"`
	Episode    int      `json:"episode,omitempty"`
	Video      bool     `json:"video"`
	Bridged    bool     `json:"bridged,omitempty"` // found by a server on Kad/torrent/usenet: often unreachable
}

// Parse reads a file name.
func Parse(name string) Info {
	var in Info
	in.Bridged = bridgedRe.MatchString(name)
	name = prefixRe.ReplaceAllString(name, "")
	ext := strings.ToLower(path.Ext(name))
	in.Video = videoExts[ext]
	base := name
	if in.Video {
		base = strings.TrimSuffix(name, path.Ext(name))
	}
	for _, l := range Languages {
		if l.Re.MatchString(base) {
			in.Languages = append(in.Languages, l.Code)
		}
	}
	if latinoRe.MatchString(base) {
		// "Latino" is Latin American Spanish, not Spain's.
		in.Languages = append(removeLang(in.Languages, "es"), "es-419")
	}
	in.Subs = subsRe.MatchString(base)
	in.Dual = dualRe.MatchString(base)
	for _, r := range resolution {
		if r.re.MatchString(base) {
			in.Resolution = r.name
			break
		}
	}
	for _, s := range sources {
		if s.re.MatchString(base) {
			in.Source = s.name
			break
		}
	}
	for _, m := range yearRe.FindAllStringSubmatch(base, -1) {
		if y, _ := strconv.Atoi(m[1]); y >= 1900 && y <= 2100 {
			in.Year = y // the last year in the name is usually the release year
		}
	}
	if m := episodeRe.FindStringSubmatch(base); m != nil {
		s, e := m[1]+m[3]+m[5], m[2]+m[4]+m[6]
		in.Season, _ = strconv.Atoi(s)
		in.Episode, _ = strconv.Atoi(e)
	} else if m := seasonRe.FindStringSubmatch(base); m != nil {
		in.Season, _ = strconv.Atoi(m[1])
	}
	return in
}

func removeLang(list []string, code string) []string {
	var out []string
	for _, l := range list {
		if l != code {
			out = append(out, l)
		}
	}
	return out
}

// ---- matching ----

// accents maps accented Latin letters to plain ones.
var accents = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u",
	"ñ", "n", "ç", "c", "ý", "y", "ÿ", "y", "œ", "oe", "æ", "ae", "ß", "ss",
)

// normalize lowercases, removes accents and turns everything that isn't a
// letter or digit into single spaces: "The Movie (1982).mkv" -> "the movie 1982 mkv".
func normalize(s string) string {
	var b strings.Builder
	space := true
	for _, r := range accents.Replace(strings.ToLower(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// Characters that separate a title from other text in a name, as in
// "Other Title - The Movie (1982)" or "[DVD] The Movie".
const separators = "()[]{}-|_/;:,+"

// Words that may follow a title directly: release details, not more
// words of a longer title ("The Movie 1982 1080p" but not "The Movie
// Returns").
var tagWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`dual multi multisub bdrip brrip bluray bdremux remux dvdrip dvd hdrip webrip web webdl dl
		hdtv tvrip x264 x265 h264 h265 hevc avc xvid divx ac3 eac3 dts aac mp3 spanish castellano english ingles
		latino vose vos vosi subs sub subtitulado esp spa eng ing ita fre fra ger cat french german italian
		extended remastered remasterizado uncut directors cut version edicion hd microhd fullhd 4k uhd hdr sdr
		proper repack limited internal complete completa mkv avi mp4 cd1 cd2 by hq ts cam screener dvdscr`) {
		tagWords[w] = true
	}
}

// fold lowercases and removes accents, keeping punctuation.
func fold(s string) string { return accents.Replace(strings.ToLower(s)) }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// MatchTitle returns the title that appears in the file name as the name
// of the work, not as part of a longer text ("" if none). Longer titles
// are tried first, so "The Movie Returns" wins over "The Movie".
func MatchTitle(name string, titles []string) string {
	n := fold(prefixRe.ReplaceAllString(name, ""))
	sorted := append([]string{}, titles...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(normalize(sorted[i])) > len(normalize(sorted[j])) })
	for _, t := range sorted {
		words := strings.Fields(normalize(t))
		if len(words) == 0 {
			continue
		}
		var parts []string
		for _, w := range words {
			parts = append(parts, regexp.QuoteMeta(w))
		}
		re := regexp.MustCompile(`(?:^|[^\p{L}\p{N}])(` + strings.Join(parts, `[^\p{L}\p{N}]+`) + `)(?:[^\p{L}\p{N}]|$)`)
		for _, m := range re.FindAllStringSubmatchIndex(n, -1) {
			if titleStandsAlone(n, m[2], m[3]) {
				return t
			}
		}
	}
	return ""
}

// titleStandsAlone checks the text around a title found at n[start:end].
func titleStandsAlone(n string, start, end int) bool {
	// Before: nothing, a separator, or a year ("(1982) The Movie").
	before := strings.TrimRightFunc(n[:start], func(r rune) bool { return !isWordRune(r) })
	gapBefore := n[len(before):start]
	if before != "" && !strings.ContainsAny(gapBefore, separators) {
		fields := strings.FieldsFunc(before, func(r rune) bool { return !isWordRune(r) })
		last := fields[len(fields)-1]
		if y, err := strconv.Atoi(last); err != nil || y < 1900 || y > 2100 {
			return false
		}
	}
	// After: nothing, a separator, a year or a release detail.
	rest := n[end:]
	after := strings.TrimLeftFunc(rest, func(r rune) bool { return !isWordRune(r) })
	gapAfter := rest[:len(rest)-len(after)]
	if after == "" || strings.ContainsAny(gapAfter, separators) {
		return true
	}
	next := strings.FieldsFunc(after, func(r rune) bool { return !isWordRune(r) })[0]
	if y, err := strconv.Atoi(next); err == nil && y >= 1900 && y <= 2100 {
		return true
	}
	return tagWords[next] || resolutionWord(next) || episodeWordRe.MatchString(next)
}

// episodeWordRe matches what follows a series title: S02E05, 2x05, S02,
// "temporada", "season", "capitulo"...
var episodeWordRe = regexp.MustCompile(`^(?:s\d{1,2}(?:e\d{1,3})?|\d{1,2}x\d{2,3}|season|temporada|temp|capitulo|cap|episode|episodio|ep|e\d{1,3}|t\d{1,2})$`)

func resolutionWord(w string) bool {
	switch w {
	case "2160p", "1080p", "1080i", "720p", "576p", "480p":
		return true
	}
	return false
}

// ---- scoring ----

// Prefs are the user's preferences for choosing a release.
type Prefs struct {
	Languages  []string // wanted audio languages, best first (e.g. es, en)
	Subs       bool     // accept original version with subtitles
	Resolution string   // preferred: 2160p, 1080p, 720p or 480p
	MaxSize    int64    // bytes, 0 = no limit
	NoCam      bool     // drop cam/telesync/screener copies
}

// Candidate is a search result with what we know about it.
type Candidate struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Sources  int    `json:"sources"`
	Complete int    `json:"complete"`
	Info     Info   `json:"info"`
	Matched  string `json:"matched_title"`
	Score    int    `json:"score"`
	// Language guessed from the matched title when the name has no tag
	// (a file named with the Spanish title is usually in Spanish).
	GuessedLanguage string `json:"guessed_language,omitempty"`
	Hidden          bool   `json:"hidden"` // fails the filters; shown on request
	HiddenWhy       string `json:"hidden_why,omitempty"`
}

// Target describes what we are looking for.
type Target struct {
	Titles        []string          // all titles of the work
	TitleLanguage map[string]string // title -> language it belongs to ("" if unknown)
	Year          int
	Series        bool
	Season        int // 0 = any
	Episode       int // 0 = any (or the whole season)
}

var resolutionRank = map[string]int{"2160p": 4, "1080p": 3, "720p": 2, "480p": 1}

// Evaluate checks a result against the target and scores it with prefs.
// It returns false when the result is not this work at all.
func Evaluate(c *Candidate, t Target, p Prefs) bool {
	c.Info = Parse(c.Name)
	c.Matched = MatchTitle(c.Name, t.Titles)
	if c.Matched == "" || !c.Info.Video {
		return false
	}
	// A year in the name that is not the work's: another movie with the
	// same title (remakes). Series names often carry other years; allow it.
	if !t.Series && t.Year > 0 && c.Info.Year > 0 && abs(c.Info.Year-t.Year) > 1 {
		return false
	}
	// An episode is not a movie ("Some Show 3x23 The Movie...").
	if !t.Series && c.Info.Episode > 0 {
		return false
	}
	if t.Series {
		if t.Season > 0 && c.Info.Season > 0 && c.Info.Season != t.Season {
			return false
		}
		if t.Episode > 0 && c.Info.Episode > 0 && c.Info.Episode != t.Episode {
			return false
		}
		if t.Episode > 0 && c.Info.Episode == 0 && c.Info.Season == 0 {
			return false // can't tell which episode it is
		}
	}

	langs := c.Info.Languages
	if len(langs) == 0 {
		if l := t.TitleLanguage[c.Matched]; l != "" {
			c.GuessedLanguage = l
			langs = []string{l}
		}
	}

	score := 0
	// Language: the most important thing for most people.
	if len(p.Languages) > 0 {
		best := -1
		for i, want := range p.Languages {
			for _, have := range langs {
				if have == want || strings.HasPrefix(have, want+"-") {
					if best < 0 || i < best {
						best = i
					}
				}
			}
		}
		switch {
		case best >= 0:
			score += 100 - best*30
		case c.Info.Subs && p.Subs:
			score += 40
		case len(langs) == 0:
			score += 10 // unknown
		default:
			score -= 50
		}
	}
	// Resolution: the preferred one scores most, then nearby ones.
	if r := resolutionRank[c.Info.Resolution]; r > 0 {
		want := resolutionRank[p.Resolution]
		if want == 0 {
			want = 3
		}
		score += 30 - abs(r-want)*12
	}
	switch c.Info.Source {
	case "remux", "bluray":
		score += 12
	case "web":
		score += 10
	case "hdtv":
		score += 6
	case "dvd":
		score += 4
	case "screener":
		score -= 40
	case "cam":
		score -= 80
	}
	// Availability: complete sources matter a lot on eD2K.
	score += min(c.Complete, 20)*3 + min(c.Sources, 30)
	if t.Year > 0 && c.Info.Year == t.Year {
		score += 5
	}
	if t.Series && t.Episode > 0 && c.Info.Episode == t.Episode {
		score += 10
	}
	if c.Info.Bridged {
		score -= 60
	}
	c.Score = score

	switch {
	case c.Sources == 0:
		c.Hidden, c.HiddenWhy = true, "no sources"
	case c.Info.Bridged:
		c.Hidden, c.HiddenWhy = true, "sources outside eD2K (Kad/torrent/usenet)"
	case p.NoCam && (c.Info.Source == "cam" || c.Info.Source == "screener"):
		c.Hidden, c.HiddenWhy = true, "cam or screener copy"
	case p.MaxSize > 0 && c.Size > p.MaxSize:
		c.Hidden, c.HiddenWhy = true, "bigger than the maximum size"
	}
	return true
}

// Rank sorts candidates: visible first, then by score.
func Rank(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Hidden != cs[j].Hidden {
			return !cs[i].Hidden
		}
		return cs[i].Score > cs[j].Score
	})
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
