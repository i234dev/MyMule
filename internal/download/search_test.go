package download

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/server"
)

// A server that repeats a result cannot add its sources again, and none
// can claim an absurd number; different servers still add up.
func TestResultMergerCannotBeInflated(t *testing.T) {
	var mg resultMerger
	fake := server.Result{Hash: ed2k.Hash{1}, Name: "fake.mkv", Size: 1000, Sources: 900, CompleteSources: 900}
	for i := 0; i < 1000; i++ {
		mg.add("203.0.113.1", fake)
	}
	mg.add("203.0.113.1", server.Result{Hash: ed2k.Hash{2}, Name: "huge.mkv", Size: 1000, Sources: 4_000_000_000, CompleteSources: 4_000_000_000})
	real := server.Result{Hash: ed2k.Hash{3}, Name: "real.mkv", Size: 1000, Sources: 40, CompleteSources: 30}
	mg.add("198.51.100.1", real)
	mg.add("198.51.100.2", real)

	res, servers := mg.results()
	if servers != 3 {
		t.Fatalf("%d servers, want 3", servers)
	}
	got := map[string]server.Result{}
	for _, r := range res {
		got[r.Name] = r
	}
	if got["fake.mkv"].Sources != 900 {
		t.Errorf("repeated result: %d sources, want 900", got["fake.mkv"].Sources)
	}
	if got["huge.mkv"].Sources != maxReportedSources || got["huge.mkv"].CompleteSources != maxReportedSources {
		t.Errorf("absurd claim: %d/%d sources, want the cap %d", got["huge.mkv"].Sources, got["huge.mkv"].CompleteSources, maxReportedSources)
	}
	if got["real.mkv"].Sources != 80 || got["real.mkv"].CompleteSources != 60 {
		t.Errorf("two servers: %d/%d sources, want 80/60", got["real.mkv"].Sources, got["real.mkv"].CompleteSources)
	}
}

func TestResultMergerLimits(t *testing.T) {
	var mg resultMerger
	for i := 0; i < maxGlobalResults+500; i++ {
		mg.add("203.0.113.1", server.Result{Hash: ed2k.Hash{byte(i), byte(i >> 8), 1}, Size: 1, Sources: 1})
	}
	if res, _ := mg.results(); len(res) != maxGlobalResults {
		t.Fatalf("%d results kept, want %d", len(res), maxGlobalResults)
	}
	if satAdd(^uint32(0)-1, 5) != ^uint32(0) || satAdd(2, 3) != 5 {
		t.Fatal("satAdd wraps or miscounts")
	}
}

// A download that completed but was never moved (the final step failed,
// or MyMule stopped first) is finished by the manager on its own.
func TestManagerFinishesCompleteDownloads(t *testing.T) {
	dir := t.TempDir()
	data := testData(4000)
	link, _ := linkFor(t, "left.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data) // complete, but nobody called Finish
	done := make(chan string, 1)
	m := NewManager(Config{IncomingDir: filepath.Join(dir, "incoming"),
		OnComplete: func(_ ed2k.Hash, _, path string) { done <- path }})
	m.Add(pf)
	m.retryFinish()
	select {
	case p := <-done:
		if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
			t.Fatal("finished file differs")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("complete download was not finished")
	}
}
