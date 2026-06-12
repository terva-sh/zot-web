package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryNormalize(t *testing.T) {
	q := Query{
		Text:           "  golang  ",
		Freshness:      "Week",
		Depth:          "ADVANCED",
		IncludeDomains: []string{" https://Docs.Example.com/path ", ""},
		ExcludeDomains: []string{"Spam.example"},
	}
	if err := q.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if q.Text != "golang" || q.Freshness != "week" || q.Depth != "advanced" || q.Count != 5 {
		t.Errorf("normalized = %+v", q)
	}
	if len(q.IncludeDomains) != 1 || q.IncludeDomains[0] != "docs.example.com" {
		t.Errorf("include domains = %v, want [docs.example.com]", q.IncludeDomains)
	}
	if len(q.ExcludeDomains) != 1 || q.ExcludeDomains[0] != "spam.example" {
		t.Errorf("exclude domains = %v, want [spam.example]", q.ExcludeDomains)
	}

	bad := Query{Text: "x", Freshness: "fortnight"}
	if err := bad.Normalize(); err == nil || !strings.Contains(err.Error(), "freshness") {
		t.Errorf("want freshness error, got %v", err)
	}
	bad = Query{Text: "x", Depth: "deep"}
	if err := bad.Normalize(); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Errorf("want depth error, got %v", err)
	}
	bad = Query{Text: "   "}
	if err := bad.Normalize(); err == nil {
		t.Error("want error for empty query")
	}
}

func TestTavilySendsFilters(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"T","url":"https://a.example/x","content":"c","published_date":"2026-05-01T00:00:00Z"}]}`))
	}))
	t.Cleanup(srv.Close)
	tv := &tavily{key: "k", client: srv.Client()}
	// Point the request at the test server by rewriting via the client transport.
	tv.client = &http.Client{Transport: rewriteHost(srv)}

	q := Query{Text: "golang", Count: 3, Freshness: "week", Depth: "advanced",
		IncludeDomains: []string{"a.example"}, ExcludeDomains: []string{"b.example"}}
	res, err := tv.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got["time_range"] != "week" || got["search_depth"] != "advanced" {
		t.Errorf("payload = %v", got)
	}
	if inc, ok := got["include_domains"].([]any); !ok || len(inc) != 1 || inc[0] != "a.example" {
		t.Errorf("include_domains = %v", got["include_domains"])
	}
	if exc, ok := got["exclude_domains"].([]any); !ok || len(exc) != 1 || exc[0] != "b.example" {
		t.Errorf("exclude_domains = %v", got["exclude_domains"])
	}
	if len(res) != 1 || res[0].Published == "" {
		t.Errorf("results = %+v, want published date carried through", res)
	}
}

// rewriteHost redirects any request to the given test server.
func rewriteHost(srv *httptest.Server) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		u := *r.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(srv.URL, "http://")
		r2 := r.Clone(r.Context())
		r2.URL = &u
		return http.DefaultTransport.RoundTrip(r2)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSearxNGFiltersAndFreshness(t *testing.T) {
	var gotQuery, gotTimeRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotTimeRange = r.URL.Query().Get("time_range")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[
			{"title":"keep","url":"https://docs.example.com/a","content":"x","publishedDate":"2026-04-02"},
			{"title":"drop-excluded","url":"https://spam.example/b","content":"x"},
			{"title":"drop-not-included","url":"https://other.example/c","content":"x"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	se := &searxng{base: srv.URL, client: srv.Client()}

	q := Query{Text: "golang", Count: 10, Freshness: "month",
		IncludeDomains: []string{"example.com"}, ExcludeDomains: []string{"spam.example"}}
	res, err := se.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotTimeRange != "month" {
		t.Errorf("time_range = %q, want month", gotTimeRange)
	}
	if !strings.Contains(gotQuery, "site:example.com") {
		t.Errorf("query %q missing site: hint", gotQuery)
	}
	if len(res) != 1 || res[0].Title != "keep" {
		t.Errorf("post-filter results = %+v, want only the docs.example.com hit", res)
	}
	if res[0].Published != "2026-04-02" {
		t.Errorf("published = %q", res[0].Published)
	}
}

func TestFormatShowsPublishedDate(t *testing.T) {
	out := Format("q", []Result{
		{Title: "A", URL: "https://a.example", Snippet: "s", Published: "2026-05-01T12:00:00Z"},
		{Title: "B", URL: "https://b.example", Snippet: "s"},
	})
	if !strings.Contains(out, "A (2026-05-01)") {
		t.Errorf("missing normalized date:\n%s", out)
	}
	if strings.Contains(out, "B (") {
		t.Errorf("undated result should have no date suffix:\n%s", out)
	}
}
