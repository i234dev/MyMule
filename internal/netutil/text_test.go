package netutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanText(t *testing.T) {
	cases := map[string]string{
		"Película (2001) ñ 日本 🎬":             "Película (2001) ñ 日本 🎬",
		"a\x1b[2J\x1b[1;31mred":              "a[2J[1;31mred",
		"title\x1b]0;owned\x07":              "title]0;owned",
		"clip\x1b]52;c;ZWNobyBoaQ==\x07":     "clip]52;c;ZWNobyBoaQ==",
		"line1\nFAKE 12:00 COMPLETED x":      "line1 FAKE 12:00 COMPLETED x",
		"tab\there\r":                        "tab here ",
		"evil\u202evkm.exe":                  "evilvkm.exe",
		"zero\u200bwidth\ufeff\u2066x\u2069": "zerowidthx",
		"c1\u009bcontrol\u0085":              "c1control",
		"del\x7f":                            "del",
	}
	for in, want := range cases {
		if got := CleanText(in, 0); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanText("bad\xffutf8", 0); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 kept: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	s := strings.Repeat("ñ", 100) // 2 bytes each
	if got := Truncate(s, 51); len(got) != 50 || !utf8.ValidString(got) {
		t.Fatalf("Truncate split a character: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	if Truncate("short", 100) != "short" || Truncate("abc", 0) != "abc" {
		t.Fatal("Truncate changed a short string")
	}
	if got := CleanText(strings.Repeat("x", 500), 64); len(got) != 64 {
		t.Fatalf("CleanText limit: %d bytes", len(got))
	}
}
