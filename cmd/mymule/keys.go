package main

import "unicode/utf8"

// keyKind identifies a key press decoded from terminal input.
type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyBackspace
	keyEsc
	keyUp
	keyDown
	keyLeft
	keyRight
	keyPgUp
	keyPgDn
	keyHome
	keyEnd
	keyTab
	keyCtrlC
	keyCtrlG
	keyCtrlU
)

type key struct {
	kind keyKind
	r    rune // for keyRune
}

// parseKeys decodes a chunk of raw terminal input (as read in one go)
// into key presses: printable runes (UTF-8), control keys and the usual
// VT escape sequences for arrows and paging. A lone ESC is the Esc key.
func parseKeys(b []byte) []key {
	var keys []key
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(b) {
				keys = append(keys, key{kind: keyEsc})
				i++
				continue
			}
			if b[i+1] != '[' && b[i+1] != 'O' {
				keys = append(keys, key{kind: keyEsc})
				i++
				continue
			}
			// CSI / SS3: parameters then a final byte in 0x40..0x7e.
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j >= len(b) {
				i = len(b) // truncated sequence: drop it
				continue
			}
			params, final := string(b[i+2:j]), b[j]
			i = j + 1
			switch {
			case final == 'A':
				keys = append(keys, key{kind: keyUp})
			case final == 'B':
				keys = append(keys, key{kind: keyDown})
			case final == 'C':
				keys = append(keys, key{kind: keyRight})
			case final == 'D':
				keys = append(keys, key{kind: keyLeft})
			case final == 'H':
				keys = append(keys, key{kind: keyHome})
			case final == 'F':
				keys = append(keys, key{kind: keyEnd})
			case final == '~' && (params == "5"):
				keys = append(keys, key{kind: keyPgUp})
			case final == '~' && (params == "6"):
				keys = append(keys, key{kind: keyPgDn})
			case final == '~' && (params == "1" || params == "7"):
				keys = append(keys, key{kind: keyHome})
			case final == '~' && (params == "4" || params == "8"):
				keys = append(keys, key{kind: keyEnd})
			}
		case c == '\r' || c == '\n':
			keys = append(keys, key{kind: keyEnter})
			i++
			if c == '\r' && i < len(b) && b[i] == '\n' {
				i++
			}
		case c == 0x7f || c == 0x08:
			keys = append(keys, key{kind: keyBackspace})
			i++
		case c == '\t':
			keys = append(keys, key{kind: keyTab})
			i++
		case c == 0x03:
			keys = append(keys, key{kind: keyCtrlC})
			i++
		case c == 0x07:
			keys = append(keys, key{kind: keyCtrlG})
			i++
		case c == 0x15:
			keys = append(keys, key{kind: keyCtrlU})
			i++
		case c < 0x20:
			i++ // other control characters are ignored
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			keys = append(keys, key{kind: keyRune, r: r})
			i += size
		}
	}
	return keys
}
