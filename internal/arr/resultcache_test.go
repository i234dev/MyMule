package arr

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"mymule/internal/api"
	"mymule/internal/server"
)

func TestTorznabPagesShareOneSearch(t *testing.T) {
	fx := newFixture(t)
	base := fx.srv.URL + "/torznab/api?t=movie&q=Example&apikey=" + testKey

	// The first page runs the search; the next ones are cut from the same
	// answer. fakeResults has four results with sources.
	_, p1 := get(t, http.DefaultClient, base+"&limit=1&offset=0")
	_, p2 := get(t, http.DefaultClient, base+"&limit=1&offset=1")
	_, p3 := get(t, http.DefaultClient, base+"&limit=1&offset=3")
	_, p4 := get(t, http.DefaultClient, base+"&limit=1&offset=4")
	if len(fx.searches) != 1 {
		t.Fatalf("eD2K searches for 4 pages: %d, want 1", len(fx.searches))
	}
	n := func(s string) int { return strings.Count(s, "<item>") }
	if n(p1) != 1 || n(p2) != 1 || n(p3) != 1 || n(p4) != 0 {
		t.Fatalf("items per page: %d %d %d %d, want 1 1 1 0", n(p1), n(p2), n(p3), n(p4))
	}
	if p1 == p2 {
		t.Fatal("pages 1 and 2 are the same")
	}

	// Another category selection is another answer.
	get(t, http.DefaultClient, base+"&cat=2000")
	get(t, http.DefaultClient, base+"&cat=2000")
	if len(fx.searches) != 2 {
		t.Fatalf("searches after a different cat: %d, want 2", len(fx.searches))
	}
}

func TestResultCacheExpires(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := &resultCache{now: func() time.Time { return now }}
	runs := 0
	fn := func() ([]tzItem, bool, error) { runs++; return []tzItem{{cat: catMovies}}, true, nil }

	for i := 0; i < 3; i++ {
		if _, err := c.do(context.Background(), "k", fn); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 {
		t.Fatalf("runs within the TTL: %d", runs)
	}
	now = now.Add(resultTTL + time.Second)
	c.do(context.Background(), "k", fn)
	if runs != 2 {
		t.Fatalf("runs after the TTL: %d", runs)
	}
}

func TestResultCacheDoesNotKeepFailuresOrPartialAnswers(t *testing.T) {
	c := &resultCache{}
	runs := 0
	boom := errors.New("boom")
	if _, err := c.do(context.Background(), "e", func() ([]tzItem, bool, error) { runs++; return nil, false, boom }); err != boom {
		t.Fatalf("error not returned: %v", err)
	}
	c.do(context.Background(), "e", func() ([]tzItem, bool, error) { runs++; return nil, true, nil })
	if runs != 2 {
		t.Fatalf("a failed search was remembered (runs=%d)", runs)
	}
	// Partial: returned to the caller, but searched again next time.
	items, _ := c.do(context.Background(), "p", func() ([]tzItem, bool, error) { runs++; return []tzItem{{}}, false, nil })
	c.do(context.Background(), "p", func() ([]tzItem, bool, error) { runs++; return []tzItem{{}, {}}, true, nil })
	if len(items) != 1 || runs != 4 {
		t.Fatalf("partial answer: items=%d runs=%d", len(items), runs)
	}
}

func TestResultCacheJoinsRunningSearch(t *testing.T) {
	c := &resultCache{}
	var mu sync.Mutex
	runs := 0
	release := make(chan struct{})
	started := make(chan struct{})
	fn := func() ([]tzItem, bool, error) {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		if first {
			close(started)
		}
		<-release
		return []tzItem{{cat: catMovies}}, true, nil
	}
	var wg sync.WaitGroup
	got := make([]int, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i > 0 {
				<-started // arrive while the first search is running
			}
			items, err := c.do(context.Background(), "k", fn)
			if err == nil {
				got[i] = len(items)
			}
		}()
	}
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs != 1 {
		t.Fatalf("searches run for 4 simultaneous requests: %d", runs)
	}
	for i, n := range got {
		if n != 1 {
			t.Fatalf("request %d got %d items", i, n)
		}
	}
}

func TestResultCacheWaiterRunsItselfWhenLeaderFails(t *testing.T) {
	c := &resultCache{}
	release := make(chan struct{})
	started := make(chan struct{})
	boom := errors.New("client went away")
	go c.do(context.Background(), "k", func() ([]tzItem, bool, error) {
		close(started)
		<-release
		return nil, false, boom
	})
	<-started
	done := make(chan []tzItem, 1)
	go func() {
		items, _ := c.do(context.Background(), "k", func() ([]tzItem, bool, error) {
			return []tzItem{{cat: catMovies}}, true, nil
		})
		done <- items
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case items := <-done:
		if len(items) != 1 {
			t.Fatalf("waiter got %d items", len(items))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never answered")
	}
}

// A search that panics (net/http recovers it) must not stay in flight:
// the request waiting for it runs the search itself, and later requests
// for the same search are answered instead of waiting forever.
func TestResultCacheSurvivesPanickingSearch(t *testing.T) {
	c := &resultCache{}
	release := make(chan struct{})
	started := make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		c.do(context.Background(), "k", func() ([]tzItem, bool, error) {
			close(started)
			<-release
			panic("boom")
		})
	}()
	<-started
	done := make(chan []tzItem, 1)
	go func() {
		items, _ := c.do(context.Background(), "k", func() ([]tzItem, bool, error) {
			return []tzItem{{cat: catMovies}}, true, nil
		})
		done <- items
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if p := <-panicked; p != "boom" {
		t.Fatalf("the panic was not passed on: %v", p)
	}
	select {
	case items := <-done:
		if len(items) != 1 {
			t.Fatalf("waiter got %d items", len(items))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never answered")
	}

	// A later request for the same search: no flight left behind.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	items, err := c.do(ctx, "k", func() ([]tzItem, bool, error) {
		t.Error("the cached answer was not used")
		return nil, false, nil
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("later request: %d items, %v", len(items), err)
	}
	c.mu.Lock()
	left := len(c.flights)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d flights left behind", left)
	}
}

// A request that comes after a panic, with no other request around, runs
// the search again instead of waiting on the dead flight.
func TestResultCacheRetriesAfterPanic(t *testing.T) {
	c := &resultCache{}
	func() {
		defer func() { recover() }()
		c.do(context.Background(), "k", func() ([]tzItem, bool, error) { panic("boom") })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	items, err := c.do(ctx, "k", func() ([]tzItem, bool, error) {
		return []tzItem{{cat: catTV}}, true, nil
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("after a panic: %d items, %v", len(items), err)
	}
}

func TestTorznabFailedSearchIsNotCached(t *testing.T) {
	fx := newFixture(t)
	fail := true
	fx.a.search = func(ctx context.Context, q server.Query, global bool) ([]api.SearchResult, error) {
		fx.searches = append(fx.searches, q)
		if fail {
			return nil, errors.New("no servers")
		}
		return fakeResults, nil
	}
	base := fx.srv.URL + "/torznab/api?t=movie&q=Example&apikey=" + testKey
	if code, _ := get(t, http.DefaultClient, base); code != http.StatusServiceUnavailable {
		t.Fatalf("failed search status: %d", code)
	}
	fail = false
	if code, body := get(t, http.DefaultClient, base); code != 200 || !strings.Contains(body, "Example Movie 2024") {
		t.Fatalf("after recovery: %d %s", code, body)
	}
}
