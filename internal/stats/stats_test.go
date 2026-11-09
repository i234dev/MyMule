package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The totals add up across runs: each run starts from what the last one
// saved, and adds its own counts.
func TestTotalsAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	up, down := int64(0), int64(0)
	cur := func() (int64, int64) { return up, down }

	a := Open(path, cur, t.Logf)
	since := a.Totals().Since
	if since.IsZero() || time.Since(since) > time.Minute {
		t.Fatalf("since %v", since)
	}
	up, down = 100, 2000
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	up, down = 150, 2500 // more before the next save
	a.Save()

	// Next run: its own counts start from zero again.
	up, down = 0, 0
	b := Open(path, cur, t.Logf)
	if got := b.Totals(); got.Uploaded != 150 || got.Downloaded != 2500 || !got.Since.Equal(since) {
		t.Fatalf("after restart: %+v", got)
	}
	up, down = 10, 20
	if got := b.Totals(); got.Uploaded != 160 || got.Downloaded != 2520 {
		t.Fatalf("this run added: %+v", got)
	}
	if fi, err := os.Stat(path); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("file: %v", err)
	}
}

// A damaged file, or one with negative counts, starts counting again
// rather than showing nonsense.
func TestDamagedTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	cur := func() (int64, int64) { return 0, 0 }
	for _, bad := range []string{"not json", `{"uploaded":-5,"downloaded":1}`} {
		os.WriteFile(path, []byte(bad), 0o600)
		var logged bool
		s := Open(path, cur, func(string, ...any) { logged = true })
		if got := s.Totals(); got.Uploaded != 0 || got.Downloaded != 0 || got.Since.IsZero() || !logged {
			t.Fatalf("%q: %+v, logged %v", bad, got, logged)
		}
	}
}
