package ratings

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
)

func TestStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ratings.json")
	s := Open(p)
	h := ed2k.Hash{1}
	if err := s.Set(h, Rating{Name: "Example.Film.mkv", Size: 1000, Rating: 6}); err != ErrBad {
		t.Fatal("rating 6 accepted")
	}
	long := strings.Repeat("á", 200)
	if err := s.Set(h, Rating{Name: "Example.Film.mkv", Size: 1000, Rating: 1, Comment: "fake\nline two " + long}); err != nil {
		t.Fatal(err)
	}
	r, ok := Open(p).Get(h)
	if !ok || r.Rating != 1 || strings.Contains(r.Comment, "\n") || len([]rune(r.Comment)) != MaxComment {
		t.Fatalf("%+v", r)
	}

	var mu sync.Mutex
	var sent []int
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx, func(_ ed2k.Hash, size uint64, name string, rating int, comment string) error {
		mu.Lock()
		sent = append(sent, rating)
		mu.Unlock()
		return nil
	}, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != 1 {
		t.Fatalf("published %v, want once", sent)
	}
	s.Set(h, Rating{})
	if _, ok := s.Get(h); ok {
		t.Fatal("not removed")
	}
}
