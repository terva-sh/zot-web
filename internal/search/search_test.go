package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/terva-sh/zot-web/internal/config"
)

// ---------------------------------------------------------------------------
// TestNew
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	t.Run("tavily with API key", func(t *testing.T) {
		p, err := New(config.Config{
			SearchBackend: "tavily",
			TavilyAPIKey:  "sk-abc123",
		}, nil)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if p == nil {
			t.Fatal("expected non-nil provider")
		}
	})

	t.Run("tavily with no API key", func(t *testing.T) {
		p, err := New(config.Config{
			SearchBackend: "tavily",
		}, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if p != nil {
			t.Fatal("expected nil provider on error")
		}
		if !strings.Contains(err.Error(), "no API key") {
			t.Errorf("error should mention missing API key, got: %v", err)
		}
	})

	t.Run("searxng with URL", func(t *testing.T) {
		p, err := New(config.Config{
			SearchBackend: "searxng",
			SearxngURL:    "https://search.example.com",
		}, nil)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if p == nil {
			t.Fatal("expected non-nil provider")
		}
	})

	t.Run("searxng with no URL", func(t *testing.T) {
		p, err := New(config.Config{
			SearchBackend: "searxng",
		}, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if p != nil {
			t.Fatal("expected nil provider on error")
		}
		if !strings.Contains(err.Error(), "no instance URL") {
			t.Errorf("error should mention missing URL, got: %v", err)
		}
	})

	t.Run("unknown backend", func(t *testing.T) {
		p, err := New(config.Config{
			SearchBackend: "brave",
		}, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if p != nil {
			t.Fatal("expected nil provider on error")
		}
		if !strings.Contains(err.Error(), "unknown search backend") {
			t.Errorf("error should mention unknown backend, got: %v", err)
		}
	})

	t.Run("nil HTTP client gets timeout", func(t *testing.T) {
		p, err := New(config.Config{
			SearchBackend: "tavily",
			TavilyAPIKey:  "sk-abc123",
		}, nil)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		tv, ok := p.(*tavily)
		if !ok {
			t.Fatalf("provider = %T, want *tavily", p)
		}
		if tv.client.Timeout != 25*time.Second {
			t.Fatalf("default client timeout = %s, want 25s", tv.client.Timeout)
		}
	})
}

// ---------------------------------------------------------------------------
// TestFormat
// ---------------------------------------------------------------------------

func TestFormat(t *testing.T) {
	t.Run("with results contains title URL snippet", func(t *testing.T) {
		results := []Result{
			{Title: "Example", URL: "https://example.com", Snippet: "An example website."},
		}
		out := Format("test query", results)
		if !strings.Contains(out, "Example") {
			t.Error("output should contain title")
		}
		if !strings.Contains(out, "https://example.com") {
			t.Error("output should contain URL")
		}
		if !strings.Contains(out, "An example website.") {
			t.Error("output should contain snippet")
		}
		if !strings.Contains(out, "test query") {
			t.Error("output should contain the query")
		}
	})

	t.Run("empty results", func(t *testing.T) {
		out := Format("nothing", nil)
		if !strings.Contains(out, "No results for") {
			t.Errorf("expected 'No results for' message, got: %s", out)
		}
		if !strings.Contains(out, "nothing") {
			t.Error("output should contain the query")
		}
	})

	t.Run("untrusted title cannot forge list structure", func(t *testing.T) {
		// A page picks its own <title>; a newline in it must not forge a second
		// numbered result or a fake URL line in the rendered list.
		results := []Result{
			{Title: "Benign\n2. Fake Result\n   https://evil.example", URL: "https://real.example", Snippet: "s"},
		}
		out := Format("q", results)
		for _, line := range strings.Split(out, "\n") {
			switch strings.TrimSpace(line) {
			case "2. Fake Result", "https://evil.example":
				t.Errorf("a newline in a result title forged list structure:\n%s", out)
			}
		}
		if strings.Count(out, "https://real.example") != 1 {
			t.Errorf("expected exactly the one real URL, got:\n%s", out)
		}
	})

	t.Run("snippet truncation at 300 runes", func(t *testing.T) {
		// Build a snippet that is exactly 350 runes.
		long := strings.Repeat("x", 350)
		results := []Result{
			{Title: "T", URL: "https://x.com", Snippet: long},
		}
		out := Format("q", results)
		// The truncated snippet (300 runes + "…") should appear.
		truncated := strings.Repeat("x", 300) + "…"
		if !strings.Contains(out, truncated) {
			t.Errorf("expected snippet truncated to 300 runes + …, got: %s", out)
		}
		// The full snippet should NOT appear.
		if strings.Contains(out, long) {
			t.Error("full 350-rune snippet should not appear")
		}
	})
}

// ---------------------------------------------------------------------------
// TestClampCount
// ---------------------------------------------------------------------------

func TestClampCount(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{0, 5},
		{5, 5},
		{10, 10},
		{15, 10},
		{-1, 5},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.input), func(t *testing.T) {
			got := clampCount(tt.input)
			if got != tt.want {
				t.Errorf("clampCount(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestSearxNG — integration with httptest
// ---------------------------------------------------------------------------

func TestSearxNG_Success(t *testing.T) {
	var (
		gotMethod string
		gotFormat string
		gotAccept string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotFormat = r.URL.Query().Get("format")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Result 1","url":"https://one.com","content":"First."},{"title":"Result 2","url":"https://two.com","content":"Second."}]}`))
	}))
	defer srv.Close()

	se := &searxng{base: srv.URL, client: http.DefaultClient}
	results, err := se.Search(context.Background(), Query{Text: "golang", Count: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Title != "Result 1" {
		t.Errorf("unexpected title: %s", results[0].Title)
	}
	if results[0].URL != "https://one.com" {
		t.Errorf("unexpected URL: %s", results[0].URL)
	}
	if results[0].Snippet != "First." {
		t.Errorf("unexpected snippet: %s", results[0].Snippet)
	}

	// Verify request properties.
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotFormat != "json" {
		t.Errorf("expected format=json in URL, got %q", gotFormat)
	}
	if gotAccept != "application/json" {
		t.Errorf("expected Accept: application/json, got %q", gotAccept)
	}
}

func TestSearxNG_HTTPErrors(t *testing.T) {
	codes := []int{403, 500}
	for _, code := range codes {
		t.Run(fmt.Sprintf("HTTP %d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			se := &searxng{base: srv.URL, client: http.DefaultClient}
			_, err := se.Search(context.Background(), Query{Text: "test", Count: 5})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", code)) {
				t.Errorf("error should contain HTTP %d, got: %v", code, err)
			}
		})
	}
}

func TestSearxNG_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	se := &searxng{base: srv.URL, client: http.DefaultClient}
	_, err := se.Search(context.Background(), Query{Text: "test", Count: 5})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "searxng: decode:") {
		t.Errorf("error should contain 'searxng: decode:', got: %v", err)
	}
}

func TestSearxNG_ClampMoreResults(t *testing.T) {
	// Return 12 results but clampCount should cap at 10.
	var resultsJSON strings.Builder
	resultsJSON.WriteString(`{"results":[`)
	for i := 0; i < 12; i++ {
		if i > 0 {
			resultsJSON.WriteString(",")
		}
		resultsJSON.WriteString(fmt.Sprintf(
			`{"title":"R%d","url":"https://%d.com","content":"c%d"}`, i, i, i,
		))
	}
	resultsJSON.WriteString(`]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(resultsJSON.String()))
	}))
	defer srv.Close()

	se := &searxng{base: srv.URL, client: http.DefaultClient}
	// count=15 → clampCount returns 10.
	results, err := se.Search(context.Background(), Query{Text: "test", Count: 15})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 10 {
		t.Errorf("expected 10 results (clamped from 12), got %d", len(results))
	}
}

func TestSearxNG_FewerResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Only","url":"https://only.com","content":"one"}]}`))
	}))
	defer srv.Close()

	se := &searxng{base: srv.URL, client: http.DefaultClient}
	results, err := se.Search(context.Background(), Query{Text: "test", Count: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result (returned as-is), got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// TestTavily — integration with httptest
// ---------------------------------------------------------------------------

// tavilyTransport rewrites all requests to point at a test server.
type tavilyTransport struct {
	srv *httptest.Server
}

func (rt *tavilyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newReq := req.Clone(req.Context())
	newReq.URL.Scheme = "http"
	newReq.URL.Host = strings.TrimPrefix(rt.srv.URL, "http://")
	return http.DefaultTransport.RoundTrip(newReq)
}

func TestTavily_Success(t *testing.T) {
	var (
		gotMethod  string
		gotCT      string
		gotAuth    string
		gotBodyRaw string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		bodyBytes := make([]byte, 1024)
		n, _ := r.Body.Read(bodyBytes)
		gotBodyRaw = string(bodyBytes[:n])

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Tavily Result","url":"https://tavily-test.com","content":"A test result from tavily."}]}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: &tavilyTransport{srv: srv}}
	tv := &tavily{key: "test-api-key", client: client}
	results, err := tv.Search(context.Background(), Query{Text: "test query", Count: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Title != "Tavily Result" {
		t.Errorf("unexpected title: %s", results[0].Title)
	}
	if results[0].URL != "https://tavily-test.com" {
		t.Errorf("unexpected URL: %s", results[0].URL)
	}
	if results[0].Snippet != "A test result from tavily." {
		t.Errorf("unexpected snippet: %s", results[0].Snippet)
	}

	// Verify request properties.
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", gotCT)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("expected Authorization Bearer test-api-key, got %q", gotAuth)
	}
	if !strings.Contains(gotBodyRaw, "test query") {
		t.Errorf("request body should contain query, got: %s", gotBodyRaw)
	}
	if !strings.Contains(gotBodyRaw, "max_results") {
		t.Errorf("request body should contain max_results, got: %s", gotBodyRaw)
	}
}

func TestTavily_HTTPErrors(t *testing.T) {
	tests := []struct {
		code int
		body string
	}{
		{401, `{"error":"invalid API key"}`},
		{429, "rate limit exceeded"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("HTTP %d", tt.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.code)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := &http.Client{Transport: &tavilyTransport{srv: srv}}
			tv := &tavily{key: "any-key", client: client}
			_, err := tv.Search(context.Background(), Query{Text: "test", Count: 5})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			errStr := err.Error()
			if !strings.Contains(errStr, fmt.Sprintf("HTTP %d", tt.code)) {
				t.Errorf("error should contain HTTP %d, got: %v", tt.code, err)
			}
			wantSnippet := strings.TrimSpace(tt.body)
			if !strings.Contains(errStr, wantSnippet) {
				t.Errorf("error should contain body snippet %q, got: %v", wantSnippet, err)
			}
		})
	}
}

func TestTavily_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{this is not json`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: &tavilyTransport{srv: srv}}
	tv := &tavily{key: "any-key", client: client}
	_, err := tv.Search(context.Background(), Query{Text: "test", Count: 5})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "tavily: decode:") {
		t.Errorf("error should contain 'tavily: decode:', got: %v", err)
	}
}
