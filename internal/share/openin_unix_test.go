//go:build unix

package share

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a video was listed does not block the scan.
func TestOpenInFIFO(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "Made-up Film.mkv")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := openIn(root, p, 0, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO opened as a file")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}
