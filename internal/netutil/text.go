package netutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// CleanText makes text received from the network (file names, nicks,
// server names and messages) safe to print in a terminal or a log line,
// and cuts it to at most max bytes (0 = no limit).
//
// Control characters would let a remote party rewrite the screen, change
// the window title, plant links or clipboard contents (OSC 8/52) in some
// terminals, or forge log lines with a newline; invisible format
// characters such as U+202E (right-to-left override) make "evil<U+202E>vkm.exe"
// look like "evilexe.mkv". Line breaks and tabs become spaces; other
// controls and format characters are dropped; invalid UTF-8 is replaced.
func CleanText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r == utf8.RuneError:
			b.WriteRune(utf8.RuneError)
		case unsafeRune(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return Truncate(b.String(), max)
}

// unsafeRune reports C0 and C1 controls, DEL and Unicode format
// characters (bidi overrides and isolates, zero-width characters, BOM...).
func unsafeRune(r rune) bool {
	return r < 0x20 || (r >= 0x7F && r <= 0x9F) || unicode.Is(unicode.Cf, r)
}

// Truncate cuts s to at most max bytes without splitting a character
// (max <= 0 means no limit).
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
