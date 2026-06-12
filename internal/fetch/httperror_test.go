package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
)

func TestHTTPStatusErrorHints(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{403, `user_agent: "browser"`},
		{401, "access denied"},
		{404, "page not found"},
		{410, "page not found"},
		{429, "rate limited"},
		{500, "transient"},
		{418, "client error"},
	}
	for _, c := range cases {
		msg := (&HTTPStatusError{Status: c.status, URL: "https://example.com/x"}).Error()
		if !strings.Contains(msg, c.want) {
			t.Errorf("status %d: error %q missing hint %q", c.status, msg, c.want)
		}
	}
}

// statusServer fails the first n requests with status, then serves a page.
func statusServer(t *testing.T, status int, failures int32) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= failures {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><p>recovered after a transient failure</p></body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestRetryRecoversFrom503(t *testing.T) {
	srv, calls := statusServer(t, http.StatusServiceUnavailable, 1)
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 10}, ParseAllowList([]string{"127.0.0.1"}))
	out, err := c.Fetch(context.Background(), srv.URL, 200, 0, "")
	if err != nil {
		t.Fatalf("fetch should have recovered via retry, got: %v", err)
	}
	if !strings.Contains(out, "recovered") {
		t.Errorf("unexpected body:\n%s", out)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Errorf("server saw %d requests, want 2 (one retry)", n)
	}
}

func TestNoRetryOnPermanentStatus(t *testing.T) {
	srv, calls := statusServer(t, http.StatusNotFound, 99)
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 10}, ParseAllowList([]string{"127.0.0.1"}))
	_, err := c.Fetch(context.Background(), srv.URL, 200, 0, "")
	if err == nil || !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("want http 404 error, got: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("server saw %d requests, want 1 (404 must not be retried)", n)
	}
}
