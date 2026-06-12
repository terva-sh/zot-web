package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// searxng queries a self-hosted SearXNG instance's JSON API. The instance must
// have `json` listed under search.formats in settings.yml, else it returns 403.
type searxng struct {
	base   string
	client *http.Client
}

func (s *searxng) Search(ctx context.Context, q Query) ([]Result, error) {
	baseURL, err := url.Parse(s.base)
	if err != nil {
		return nil, fmt.Errorf("searxng: invalid base URL %q: %w", s.base, err)
	}
	baseURL = baseURL.JoinPath("search")
	text := q.Text
	// A single include domain becomes a site: hint most engines honor; the
	// post-filter below enforces the domain filters regardless of engine
	// support, so multiple includes and excludes still work.
	if len(q.IncludeDomains) == 1 {
		text += " site:" + q.IncludeDomains[0]
	}
	params := url.Values{}
	params.Set("q", text)
	params.Set("format", "json")
	if q.Freshness != "" {
		params.Set("time_range", q.Freshness) // SearXNG accepts day/week/month/year
	}
	baseURL.RawQuery = params.Encode()
	endpoint := baseURL.String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Limit response body to 1 MiB to prevent memory exhaustion.
	const maxBody = 1 << 20
	limited := io.LimitReader(resp.Body, maxBody)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng: HTTP %d (is `json` enabled in search.formats?)", resp.StatusCode)
	}

	var out struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.NewDecoder(limited).Decode(&out); err != nil {
		return nil, fmt.Errorf("searxng: decode: %w", err)
	}
	limit := clampCount(q.Count)
	res := make([]Result, 0, limit)
	for _, r := range out.Results {
		if len(res) >= limit {
			break
		}
		if !domainPermitted(r.URL, q.IncludeDomains, q.ExcludeDomains) {
			continue
		}
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Content, Published: r.PublishedDate})
	}
	return res, nil
}

// domainPermitted applies include/exclude domain filters to a result URL.
// Matching is by host suffix (docs.example.com matches example.com).
func domainPermitted(rawURL string, include, exclude []string) bool {
	if len(include) == 0 && len(exclude) == 0 {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	matches := func(domain string) bool {
		return host == domain || strings.HasSuffix(host, "."+domain)
	}
	for _, d := range exclude {
		if matches(d) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, d := range include {
		if matches(d) {
			return true
		}
	}
	return false
}
