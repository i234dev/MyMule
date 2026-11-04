package md4

import (
	"encoding/hex"
	"testing"
)

// Test vectors from RFC 1320, appendix A.5.
func TestRFC1320(t *testing.T) {
	cases := map[string]string{
		"":                           "31d6cfe0d16ae931b73c59d7e0c089c0",
		"a":                          "bde52cb31de33e46245e05fbdbd6fb24",
		"abc":                        "a448017aaf21d8525fc10ae87aa6729d",
		"message digest":             "d9130a8164549fe818874806e1c7014b",
		"abcdefghijklmnopqrstuvwxyz": "d79e1c308aa5bbcdeea8ed63df412da9",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789":                   "043f8582f241db351ce627e153e7f0e4",
		"12345678901234567890123456789012345678901234567890123456789012345678901234567890": "e33b4ddc9c38f2199c3e7b164fcc0536",
	}
	for in, want := range cases {
		got := Sum([]byte(in))
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("md4(%q) = %x, want %s", in, got, want)
		}
		// Streaming in small pieces must give the same result.
		h := New()
		for i := 0; i < len(in); i++ {
			h.Write([]byte{in[i]})
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			t.Errorf("streaming md4(%q) mismatch", in)
		}
	}
}
