// Package search implements the pluggable web_search backend. v1 ships Tavily
// (default) and SearXNG (self-hosted alternate) behind one Provider interface;
// adding Brave/Serper/Exa later is one new file each.
package search

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/terva-sh/zot-web/internal/config"
)

// Result is one search hit.
type Result struct {
	Title     string
	URL       string
	Snippet   string
	Published string // publication date when the backend reports one
}

// Query is one web_search request. Text is required; everything else is an
// optional filter that providers map to their closest native equivalent.
type Query struct {
	Text           string
	Count          int
	Freshness      string   // "", "day", "week", "month", "year"
	IncludeDomains []string // restrict results to these domains
	ExcludeDomains []string // drop results from these domains
	Depth          string   // "", "basic", "advanced" (advanced only affects Tavily)
}

// Normalize trims and canonicalizes q in place, rejecting values no backend
// understands so the model gets a correction instead of silently-ignored
// filters.
func (q *Query) Normalize() error {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" {
		return fmt.Errorf("query is required")
	}
	q.Count = clampCount(q.Count)
	q.Freshness = strings.ToLower(strings.TrimSpace(q.Freshness))
	switch q.Freshness {
	case "", "day", "week", "month", "year":
	default:
		return fmt.Errorf("invalid freshness %q (use \"day\", \"week\", \"month\", or \"year\")", q.Freshness)
	}
	q.Depth = strings.ToLower(strings.TrimSpace(q.Depth))
	switch q.Depth {
	case "", "basic", "advanced":
	default:
		return fmt.Errorf("invalid depth %q (use \"basic\" or \"advanced\")", q.Depth)
	}
	q.IncludeDomains = cleanDomains(q.IncludeDomains)
	q.ExcludeDomains = cleanDomains(q.ExcludeDomains)
	return nil
}

// cleanDomains lowercases entries and drops empties and stray schemes/paths
// (the model sometimes passes full URLs where domains are expected).
func cleanDomains(ds []string) []string {
	var out []string
	for _, d := range ds {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		if i := strings.IndexByte(d, '/'); i >= 0 {
			d = d[:i]
		}
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// Provider is a web-search backend.
type Provider interface {
	Search(ctx context.Context, q Query) ([]Result, error)
}

// New builds the provider selected by cfg.SearchBackend, using the provided
// HTTP client (which should be SSRF-guarded when the SearXNG URL is
// user-configurable).
func New(cfg config.Config, httpClient *http.Client) (Provider, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 25 * time.Second}
	}
	switch cfg.SearchBackend {
	case "tavily":
		if cfg.TavilyAPIKey == "" {
			return nil, fmt.Errorf("tavily backend selected but no API key (set TAVILY_API_KEY or tavily_api_key in config.json)")
		}
		return &tavily{key: cfg.TavilyAPIKey, client: httpClient}, nil
	case "searxng":
		if cfg.SearxngURL == "" {
			return nil, fmt.Errorf("searxng backend selected but no instance URL (set ZOT_WEB_SEARXNG_URL or searxng_url in config.json)")
		}
		u, err := url.Parse(cfg.SearxngURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("searxng: invalid URL %q (must be http(s)://host…)", cfg.SearxngURL)
		}
		return &searxng{base: u.String(), client: httpClient}, nil
	default:
		return nil, fmt.Errorf("unknown search backend %q (use \"tavily\" or \"searxng\")", cfg.SearchBackend)
	}
}

// Format renders results as a compact markdown list the model can read and
// chain into web_fetch. URLs are kept so the model can cite and follow them.
func Format(query string, results []Result) string {
	if len(results) == 0 {
		// The backend answered with an empty set — distinct from a backend
		// error, which surfaces as a tool error instead of this message.
		return fmt.Sprintf("No results for %q. Try broader or different terms, or relax any freshness/domain filters.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s", i+1, strings.TrimSpace(r.Title))
		if d := shortDate(r.Published); d != "" {
			fmt.Fprintf(&b, " (%s)", d)
		}
		fmt.Fprintf(&b, "\n   %s\n", r.URL)
		if s := oneLine(r.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return strings.TrimSpace(b.String())
}

// shortDate normalizes a backend-reported publication date to YYYY-MM-DD when
// it parses as a common layout, and otherwise passes the raw value through
// (capped, single-line).
func shortDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02", time.RFC1123, time.RFC1123Z, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	s = oneLine(s)
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 300
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// clampCount keeps result counts sane.
func clampCount(n int) int {
	if n <= 0 {
		return 5
	}
	if n > 10 {
		return 10
	}
	return n
}
