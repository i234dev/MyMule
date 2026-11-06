package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mymule/internal/download"
)

const testLink = "ed2k://|file|test.iso|20000000|0123456789ABCDEF0123456789ABCDEF|/"

func TestAPI(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp})
	stopped := make(chan struct{})
	srv, err := Listen(m, dir, func() { close(stopped) })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong token is rejected.
	bad := *c
	bad.info.Token = "nope"
	if _, err := bad.Status(); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("bad token: %v", err)
	}
	// No auth header at all.
	resp, err := http.Get("http://" + c.info.Addr + "/v1/status")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %v", resp.StatusCode, err)
	}

	r, err := c.Add(AddRequest{Links: []string{testLink, "garbage"}, Sources: []string{"192.168.1.20:4662"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Added) != 1 || len(r.Errors) != 1 {
		t.Fatalf("add: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(tmp, "0123456789ABCDEF0123456789ABCDEF.part.json")); err != nil {
		t.Fatalf("part file not created: %v", err)
	}

	st, err := c.Status()
	if err != nil || len(st.Files) != 1 || st.Files[0].Name != "test.iso" || st.Files[0].Sources != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}

	if name, err := c.Pause("test"); err != nil || name != "test.iso" {
		t.Fatalf("pause: %q %v", name, err)
	}
	if st, _ := c.Status(); !st.Files[0].Paused {
		t.Fatal("not paused")
	}
	if _, err := c.Resume("0123"); err != nil {
		t.Fatalf("resume by hash prefix: %v", err)
	}
	if _, err := c.Pause("nothing-matches"); err == nil {
		t.Fatal("pause of unknown download succeeded")
	}
	if _, err := c.Search(SearchRequest{Query: "x"}); err == nil || !strings.Contains(err.Error(), "server") {
		t.Fatalf("search without server: %v", err)
	}

	if _, err := c.Cancel("test.iso"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "0123456789ABCDEF0123456789ABCDEF.part")); !os.IsNotExist(err) {
		t.Fatal("cancel did not delete data")
	}

	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	<-stopped
	srv.Close()
	if _, err := Connect(dir); err != ErrNotRunning {
		t.Fatalf("after close: %v", err)
	}
}

// A stale api.json may point at a port someone else is using now. Connect
// must find out without ever sending the token there.
func TestConnectRefusesImpostor(t *testing.T) {
	dir := t.TempDir()
	got := make(chan string, 10)
	imp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		w.Write([]byte(`{"proof":"0000"}`))
	}))
	defer imp.Close()
	info, _ := json.Marshal(Info{Addr: strings.TrimPrefix(imp.URL, "http://"), Token: "secret-token-1234567890", PID: 1})
	os.WriteFile(filepath.Join(dir, InfoFile), info, 0o600)
	if _, err := Connect(dir); err != ErrNotRunning {
		t.Fatalf("Connect to an impostor: %v", err)
	}
	close(got)
	for a := range got {
		if a != "" {
			t.Fatalf("the token was sent to the impostor: %q", a)
		}
	}
}
