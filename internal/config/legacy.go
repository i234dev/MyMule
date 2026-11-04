package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ImportLegacy brings data from the pre-portable location (LegacyDir)
// into the current base directory, once: identity and server list are
// copied, and partial downloads are moved into tempDir. It returns
// human-readable notes about what was done. It does nothing when the
// base directory already has an identity.
func ImportLegacy(tempDir string) []string {
	old := LegacyDir()
	base, err := Dir()
	if old == "" || err != nil || sameDir(old, base) {
		return nil
	}
	oldHash := filepath.Join(old, "userhash.dat")
	if mine, err := os.ReadFile(filepath.Join(base, "userhash.dat")); err == nil {
		// Already set up here. If the old identity is the one we imported
		// earlier, retire it so other MyMule folders don't inherit it.
		if theirs, err := os.ReadFile(oldHash); err == nil && string(theirs) == string(mine) {
			os.Rename(oldHash, oldHash+".imported")
		}
		return nil
	}
	if _, err := os.Stat(oldHash); err != nil {
		return nil // nothing to import
	}
	// Never import while an old-style daemon may still be using it.
	if _, err := os.Stat(filepath.Join(old, "api.json")); err == nil {
		return []string{fmt.Sprintf("not importing from %s: a daemon seems to be running there", old)}
	}

	var notes []string
	for _, name := range []string{"userhash.dat", "server.met"} {
		if err := copyIfMissing(filepath.Join(old, name), filepath.Join(base, name)); err == nil {
			notes = append(notes, "imported "+name)
		}
	}
	// The identity now lives here; retire the old copy so it is imported
	// only once (two clients with one identity kick each other off servers).
	os.Rename(oldHash, oldHash+".imported")
	parts, _ := filepath.Glob(filepath.Join(old, "temp", "*.part*"))
	if len(parts) > 0 {
		os.MkdirAll(tempDir, 0o755)
		moved := 0
		for _, p := range parts {
			dst := filepath.Join(tempDir, filepath.Base(p))
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.Rename(p, dst); err != nil {
				notes = append(notes, fmt.Sprintf("could not move %s: %v (move it by hand)", p, err))
				continue
			}
			moved++
		}
		if moved > 0 {
			notes = append(notes, fmt.Sprintf("moved %d partial download files into %s", moved, tempDir))
		}
	}
	if len(notes) > 0 {
		notes = append([]string{"importing data from the old location " + old}, notes...)
	}
	return notes
}

func sameDir(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && filepath.Clean(aa) == filepath.Clean(bb)
}

func copyIfMissing(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return os.ErrExist
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
