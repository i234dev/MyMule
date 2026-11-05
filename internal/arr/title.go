package arr

import (
	"path"
	"regexp"
	"strings"

	"mymule/internal/release"
)

// The *arr apps parse release names written for torrents and Usenet.
// eD2K file names are freer, so the title shown to the apps is tidied up;
// the file itself keeps its real name.

// Prefixes some servers add to names, e.g. "[kad example.org] ".
var sourcePrefixRe = regexp.MustCompile(`(?i)^\s*\[(?:kad|torrent|usenet|ed2k|emule)\b[^\]]*\]\s*`)

// Language tags (e.g. "Spa.Eng", "ESP-ING", "[spa, eng]") are recognised
// with the table in package release; the apps get the full English word.

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
	for _, l := range release.Languages {
		if l.Re.MatchString(base) && !strings.Contains(lower, strings.ToLower(l.Word)) {
			add = append(add, l.Word)
		}
	}
	if len(add) == 0 {
		return base + ext
	}
	return base + " " + strings.Join(add, " ") + ext
}
