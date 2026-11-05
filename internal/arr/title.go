package arr

import (
	"path"
	"regexp"
	"strings"
)

// The *arr apps parse release names written for torrents and Usenet.
// eD2K file names are freer, so the title shown to the apps is tidied up;
// the file itself keeps its real name.

// Prefixes some servers add to names, e.g. "[kad example.org] ".
var sourcePrefixRe = regexp.MustCompile(`(?i)^\s*\[(?:kad|torrent|usenet|ed2k|emule)\b[^\]]*\]\s*`)

// Language tags common in eD2K names that the apps don't recognise
// (e.g. "Spa.Eng", "ESP-ING", "[spa, eng]"), and the word they do.
var languageTags = []struct {
	word string
	re   *regexp.Regexp
}{
	{"SPANISH", langRe(`spa|esp|castellano|español|espanol|spanish`)},
	{"ENGLISH", langRe(`eng|ing|english`)},
	{"FRENCH", langRe(`fre|fra|vff|vfq|truefrench|french`)},
	{"GERMAN", langRe(`ger|deu|deutsch|german`)},
	{"ITALIAN", langRe(`ita|italiano|italian`)},
	{"CATALAN", langRe(`cat|català|catala|catalan`)},
	// Not "por": it is also a common Spanish word ("... por Fulano").
	{"PORTUGUESE", langRe(`portugues|português|portuguese`)},
}

// langRe matches one of the words as a whole word (letters only count as
// word characters, so "Spa.Eng" and "[spa,eng]" both match).
func langRe(words string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` + words + `)(?:[^\p{L}]|$)`)
}

var videoExts = map[string]bool{".mkv": true, ".avi": true, ".mp4": true, ".m4v": true,
	".mpg": true, ".mpeg": true, ".ts": true, ".wmv": true, ".rmvb": true, ".divx": true}

// releaseTitle returns the title presented to the apps for a file name:
// without a server prefix, and with the full language names appended
// (before the extension) when the name only has abbreviations.
func releaseTitle(name string) string {
	t := sourcePrefixRe.ReplaceAllString(name, "")
	if t == "" {
		t = name
	}
	base, ext := t, path.Ext(t)
	if videoExts[strings.ToLower(ext)] {
		base = strings.TrimSuffix(t, ext)
	} else {
		ext = ""
	}
	lower := strings.ToLower(base)
	var add []string
	for _, l := range languageTags {
		if l.re.MatchString(base) && !strings.Contains(lower, strings.ToLower(l.word)) {
			add = append(add, l.word)
		}
	}
	if len(add) == 0 {
		return base + ext
	}
	return base + " " + strings.Join(add, " ") + ext
}
