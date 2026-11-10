// Package notify sends short messages about what MyMule did (a download
// finished, the wish list found something, something failed) to ntfy,
// Telegram, Discord or any web address (a webhook).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Kinds of events.
const (
	Completed = "completed" // a download finished
	Wanted    = "wanted"    // the wish list found a file and started it
	Error     = "error"     // something failed (a file could not be moved...)
	Test      = "test"      // asked for by the user
)

// AllKinds are the events that can be chosen (notify_on).
var AllKinds = []string{Completed, Wanted, Error}

// Event is one message.
type Event struct {
	Kind    string `json:"event"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// target is where messages go.
type target struct {
	kind string // ntfy, telegram, discord, webhook
	url  string // where to send it
	chat string // telegram: the chat
}

var (
	telegramToken = regexp.MustCompile(`^[0-9]{3,15}:[A-Za-z0-9_-]{20,64}$`)
	telegramChat  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{4,64})$`)
)

// parseTarget reads one notify_urls entry.
func parseTarget(s string) (target, error) {
	// tgram://BOT_TOKEN/CHAT_ID: the token has a ':', which is not a
	// valid address port, so it is read by hand.
	for _, p := range []string{"tgram://", "telegram://"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			token, chat, _ := strings.Cut(strings.TrimSuffix(rest, "/"), "/")
			if !telegramToken.MatchString(token) || !telegramChat.MatchString(chat) {
				return target{}, errors.New("telegram: use tgram://BOT_TOKEN/CHAT_ID")
			}
			return target{kind: "telegram", url: "https://api.telegram.org/bot" + token + "/sendMessage", chat: chat}, nil
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return target{}, fmt.Errorf("%q is not an address", redact(s))
	}
	path := strings.Trim(u.EscapedPath(), "/")
	switch u.Scheme {
	case "ntfy", "ntfy+http":
		// ntfy://topic (ntfy.sh) or ntfy://server/topic.
		host, topic := "ntfy.sh", u.Host
		if path != "" {
			host, topic = u.Host, path
		}
		if topic == "" || strings.Contains(topic, "/") {
			return target{}, errors.New("ntfy: use ntfy://topic or ntfy://server/topic")
		}
		scheme := "https"
		if u.Scheme == "ntfy+http" {
			scheme = "http"
		}
		t := target{kind: "ntfy", url: scheme + "://" + host + "/" + topic}
		if u.User != nil {
			t.url = scheme + "://" + u.User.String() + "@" + host + "/" + topic
		}
		return t, nil
	case "discord":
		// discord://WEBHOOK_ID/WEBHOOK_TOKEN
		if path == "" || strings.Contains(path, "/") {
			return target{}, errors.New("discord: use discord://WEBHOOK_ID/WEBHOOK_TOKEN or the webhook's https address")
		}
		return target{kind: "discord", url: "https://discord.com/api/webhooks/" + u.Host + "/" + path}, nil
	case "http", "https":
		if (u.Host == "discord.com" || u.Host == "discordapp.com") && strings.HasPrefix(path, "api/webhooks/") {
			return target{kind: "discord", url: s}, nil
		}
		return target{kind: "webhook", url: s}, nil
	}
	return target{}, fmt.Errorf("%q: use ntfy://, tgram://, discord://, or an http(s):// address", u.Scheme+"://…")
}

// Parse checks a notify_urls value (addresses separated by spaces or
// commas).
func Parse(urls string) error {
	for _, s := range fields(urls) {
		if _, err := parseTarget(s); err != nil {
			return err
		}
	}
	return nil
}

// ParseKinds checks a notify_on value and returns the kinds in it.
func ParseKinds(s string) ([]string, error) {
	var out []string
	for _, k := range fields(s) {
		k = strings.ToLower(k)
		ok := false
		for _, a := range AllKinds {
			ok = ok || a == k
		}
		if !ok {
			return nil, fmt.Errorf("%q is not an event (%s)", k, strings.Join(AllKinds, ", "))
		}
		out = append(out, k)
	}
	return out, nil
}

func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == ';' })
}

// redact hides what follows the scheme of an address (tokens) in errors.
func redact(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		return s[:i+3] + "…"
	}
	return "…"
}

// Notifier sends events to the configured addresses.
type Notifier struct {
	targets []target
	on      map[string]bool
	client  *http.Client
	logf    func(string, ...any)
	wg      sync.WaitGroup
}

// New makes a Notifier for notify_urls and notify_on (nil kinds: all). It
// does nothing without addresses.
func New(urls string, kinds []string, logf func(string, ...any)) (*Notifier, error) {
	n := &Notifier{on: map[string]bool{Test: true}, client: &http.Client{Timeout: 15 * time.Second}, logf: logf}
	for _, s := range fields(urls) {
		t, err := parseTarget(s)
		if err != nil {
			return nil, err
		}
		n.targets = append(n.targets, t)
	}
	if kinds == nil {
		kinds = AllKinds
	}
	for _, k := range kinds {
		n.on[k] = true
	}
	return n, nil
}

// Enabled reports whether there is somewhere to send events.
func (n *Notifier) Enabled() bool { return n != nil && len(n.targets) > 0 }

// Send sends e in the background, if its kind was chosen.
func (n *Notifier) Send(e Event) {
	if !n.Enabled() || !n.on[e.Kind] {
		return
	}
	for _, t := range n.targets {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := n.send(ctx, t, e); err != nil && n.logf != nil {
				n.logf("notification to %s failed: %v", t.kind, err)
			}
		}()
	}
}

// SendNow sends e to every address and waits, returning the first error
// (for the "test" button).
func (n *Notifier) SendNow(ctx context.Context, e Event) error {
	if !n.Enabled() {
		return errors.New("no notify_urls set")
	}
	var first error
	for _, t := range n.targets {
		if err := n.send(ctx, t, e); err != nil && first == nil {
			first = fmt.Errorf("%s: %w", t.kind, err)
		}
	}
	return first
}

// Wait waits for the notifications being sent (when MyMule stops).
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

func (n *Notifier) send(ctx context.Context, t target, e Event) error {
	var req *http.Request
	var err error
	switch t.kind {
	case "ntfy":
		u, _ := url.Parse(t.url)
		user := u.User
		u.User = nil
		req, err = http.NewRequestWithContext(ctx, "POST", u.String(), strings.NewReader(e.Message))
		if err == nil {
			req.Header.Set("Title", e.Title)
			req.Header.Set("Tags", map[string]string{Completed: "white_check_mark", Wanted: "star", Error: "warning", Test: "bell"}[e.Kind])
			if user != nil {
				p, _ := user.Password()
				req.SetBasicAuth(user.Username(), p)
			}
		}
	case "telegram":
		req, err = jsonRequest(ctx, t.url, map[string]string{"chat_id": t.chat, "text": e.Title + "\n" + e.Message})
	case "discord":
		req, err = jsonRequest(ctx, t.url, map[string]string{"content": "**" + e.Title + "**\n" + e.Message})
	default:
		req, err = jsonRequest(ctx, t.url, e)
	}
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "MyMule")
	resp, err := n.client.Do(req)
	if err != nil {
		// The address may hold a token: only say what went wrong.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}

func jsonRequest(ctx context.Context, u string, body any) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
