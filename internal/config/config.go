// Package config manages MyMule's data directory and persistent identity.
package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"mymule/internal/ed2k"
)

// Dir returns the data directory, creating it if needed.
// Override with the MYMULE_HOME environment variable.
func Dir() (string, error) {
	dir := os.Getenv("MYMULE_HOME")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "mymule")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
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
	if err := os.WriteFile(p, h[:], 0o600); err != nil {
		return h, fmt.Errorf("saving user hash: %w", err)
	}
	return h, nil
}
