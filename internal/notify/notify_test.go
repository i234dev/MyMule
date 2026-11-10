package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParse(t *testing.T) {
	good := map[string]target{
		"ntfy://mymule-test":                 {kind: "ntfy", url: "https://ntfy.sh/mymule-test"},
		"ntfy://ntfy.example.org/downloads":  {kind: "ntfy", url: "https://ntfy.example.org/downloads"},
		"ntfy+http://192.0.2.10:8080/alerts": {kind: "ntfy", url: "http://192.0.2.10:8080/alerts"},
		"tgram://123456789:AAExampleExampleExampleExample12/-100123": {kind: "telegram",
			url: "https://api.telegram.org/bot123456789:AAExampleExampleExampleExample12/sendMessage", chat: "-100123"},
		"discord://111/abcDEF":                              {kind: "discord", url: "https://discord.com/api/webhooks/111/abcDEF"},
		"https://discord.com/api/webhooks/111/abcDEF":       {kind: "discord", url: "https://discord.com/api/webhooks/111/abcDEF"},
		"https://hooks.example.org/mymule?key=example-key1": {kind: "webhook", url: "https://hooks.example.org/mymule?key=example-key1"},
	}
	for s, want := range good {
		got, err := parseTarget(s)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", s, got, err)
		}
	}
	for _, s := range []string{"ftp://example.org/x", "ntfy://", "tgram://nottoken/1", "tgram://123456789:AAExampleExampleExampleExample12/",
		"discord://111", "mailto:someone@example.org", "just words"} {
		if _, err := parseTarget(s); err == nil {
			t.Errorf("%s: accepted", s)
		}
	}
	// Errors do not show tokens.
	if err := Parse("tgram://123456789:AAExampleExampleExampleExample12/x y"); err == nil || strings.Contains(err.Error(), "AAExample") {
		t.Errorf("error: %v", err)
	}
	if k, err := ParseKinds("completed, error"); err != nil || len(k) != 2 {
		t.Fatalf("%v %v", k, err)
	}
	if _, err := ParseKinds("completed, everything"); err == nil {
		t.Fatal("unknown event accepted")
	}
}

// Events go to each address in its format, only the kinds chosen.
func TestSend(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("Title") + "|" + string(b)
		mu.Unlock()
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	var logged []string
	n, err := New("ntfy+http://"+host+"/topic, "+srv.URL+"/hook", []string{Completed}, func(f string, a ...any) {
		mu.Lock()
		logged = append(logged, f)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	n.Send(Event{Kind: Wanted, Title: "not chosen", Message: "x"})
	n.Send(Event{Kind: Completed, Title: "Download finished", Message: "Example.Film.mkv"})
	n.Wait()
	if got["/topic"] != "Download finished|Example.Film.mkv" {
		t.Errorf("ntfy got %q", got["/topic"])
	}
	var e Event
	if json.Unmarshal([]byte(strings.TrimPrefix(got["/hook"], "|")), &e); e.Kind != Completed || e.Message != "Example.Film.mkv" {
		t.Errorf("webhook got %q", got["/hook"])
	}
	if len(got) != 2 {
		t.Errorf("sent %v", got)
	}

	bad, _ := New(srv.URL+"/fail", nil, nil)
	if err := bad.SendNow(context.Background(), Event{Kind: Test, Title: "t", Message: "m"}); err == nil {
		t.Fatal("a failure not reported")
	}
	var none *Notifier
	none.Send(Event{Kind: Completed})
	if none.Enabled() {
		t.Fatal("nil notifier enabled")
	}
}

// A file name cannot ping everyone or hide a link in a Discord message.
func TestDiscordBody(t *testing.T) {
	b := discordBody(Event{Title: "Download finished", Message: "@everyone [click](https://example.org) *x*.mkv"})
	want := `**Download finished**` + "\n" + `\@everyone \[click\]\(https\://example.org\) \*x\*.mkv`
	if b["content"] != want {
		t.Fatalf("%q", b["content"])
	}
	if m, ok := b["allowed_mentions"].(map[string]any); !ok || len(m["parse"].([]string)) != 0 {
		t.Fatalf("%v", b["allowed_mentions"])
	}
}
