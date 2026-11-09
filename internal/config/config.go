// Package config manages MyMule's data directory and persistent identity.
package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"mymule/internal/ed2k"
	"mymule/internal/fsutil"
)

// Dir returns the base directory: the folder containing the mymule
// executable (portable mode), or MYMULE_HOME when set. Config, identity,
// server list, log, and by default temp/ and downloads/ live here.
func Dir() (string, error) {
	dir := os.Getenv("MYMULE_HOME")
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir = filepath.Dir(exe)
	}
	// Only a folder we create gets 0700: an existing one may be shared
	// with other programs (it is wherever the executable is).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// LegacyDir is where versions before portable mode kept their data
// (%AppData%\mymule, ~/.config/mymule...).
func LegacyDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "mymule")
}

// Path returns a file path inside the data directory.
func Path(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// UserHash loads our persistent 16-byte user hash, generating it on first use.
// Bytes 5 and 14 are set to 14 and 111, which is how eMule-compatible
// clients mark their hashes.
func UserHash() (ed2k.Hash, error) {
	var h ed2k.Hash
	p, err := Path("userhash.dat")
	if err != nil {
		return h, err
	}
	if b, err := os.ReadFile(p); err == nil && len(b) == 16 {
		copy(h[:], b)
		return h, nil
	}
	if _, err := rand.Read(h[:]); err != nil {
		return h, err
	}
	h[5], h[14] = 14, 111
	if err := fsutil.WriteFile(p, h[:], 0o600); err != nil {
		return h, fmt.Errorf("saving user hash: %w", err)
	}
	return h, nil
}

// SigningKey derives the key that signs web sessions and Torznab links
// from the api_key and a random secret kept in sign.key (made on first
// use). The api_key alone would make every cookie and signed link an
// offline test for guessing it; with the secret they reveal nothing about
// it. Changing the api_key still changes the key (logging everybody out).
func SigningKey(apiKey string) ([]byte, error) {
	p, err := Path("sign.key")
	if err != nil {
		return nil, err
	}
	secret, err := os.ReadFile(p)
	if err != nil || len(secret) != 32 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		if err := fsutil.WriteFile(p, secret, 0o600); err != nil {
			return nil, err
		}
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("mymule-signing|" + apiKey))
	return m.Sum(nil), nil
}
