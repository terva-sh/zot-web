package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
	"github.com/terva-sh/zot-web/internal/version"
)

func TestResolveUserAgent(t *testing.T) {
	cases := []struct{ in, fallback, want string }{
		{"", "zot-web/x", "zot-web/x"},
		{"  ", "zot-web/x", "zot-web/x"},
		{"browser", "zot-web/x", browserUserAgent},
		{"Browser", "zot-web/x", browserUserAgent},
		{"my-bot/1.0", "zot-web/x", "my-bot/1.0"},
	}
	for _, c := range cases {
		if got := resolveUserAgent(c.in, c.fallback); got != c.want {
			t.Errorf("resolveUserAgent(%q, %q) = %q, want %q", c.in, c.fallback, got, c.want)
		}
	}
}

// uaServer records the User-Agent of every request it serves.
func uaServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><p>hello from the ua test page</p></body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), agents...)
	}
}

func TestUserAgentDefaultConfigAndPerCall(t *testing.T) {
	srv, agents := uaServer(t)
	cfg := config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5}

	// Default: zot-web/<version>.
	c := New(cfg, ParseAllowList([]string{"127.0.0.1"}))
	if _, err := c.Fetch(context.Background(), srv.URL, 100, 0, ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// Configured override with the browser alias.
	cfg.UserAgent = "browser"
	cb := New(cfg, ParseAllowList([]string{"127.0.0.1"}))
	if _, err := cb.Fetch(context.Background(), srv.URL, 100, 0, ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// Per-call literal override beats the configured value.
	if _, err := cb.Fetch(context.Background(), srv.URL, 100, 0, "probe/2.0"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	got := agents()
	if len(got) != 3 {
		t.Fatalf("server saw %d requests, want 3 (%v)", len(got), got)
	}
	if want := "zot-web/" + version.Version; got[0] != want {
		t.Errorf("default UA = %q, want %q", got[0], want)
	}
	if got[1] != browserUserAgent || !strings.Contains(got[1], "Mozilla/5.0") {
		t.Errorf("config browser UA = %q, want %q", got[1], browserUserAgent)
	}
	if got[2] != "probe/2.0" {
		t.Errorf("per-call UA = %q, want %q", got[2], "probe/2.0")
	}
}

func TestPerCallUserAgentBypassesCache(t *testing.T) {
	srv, agents := uaServer(t)
	cfg := config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5, FetchCacheMaxEntries: 8}
	c := New(cfg, ParseAllowList([]string{"127.0.0.1"}))

	for range 2 { // second call is a cache hit
		if _, err := c.Fetch(context.Background(), srv.URL, 100, 0, ""); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
	if n := len(agents()); n != 1 {
		t.Fatalf("server saw %d requests before override, want 1 (cache hit expected)", n)
	}
	// An explicit user_agent forces a re-fetch even though the page is cached.
	if _, err := c.Fetch(context.Background(), srv.URL, 100, 0, "browser"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := agents()
	if len(got) != 2 || got[1] != browserUserAgent {
		t.Fatalf("override fetch: server saw %v, want second request with browser UA", got)
	}
}
