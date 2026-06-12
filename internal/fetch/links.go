package fetch

import (
	"fmt"
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"
)

// Link is one hyperlink found on a fetched page. URL is resolved to an absolute
// http(s) (or mailto/tel) target; Text is the anchor's visible text.
type Link struct {
	URL  string `json:"url"`
	Text string `json:"text,omitempty"`
}

// maxLinks bounds how many links collectLinks retains, so a link-farm page
// can't blow up the cache entry (or the model's context). Reaching it is rare;
// when it happens the excess is silently dropped after document order.
const maxLinks = 5000

// collectLinks gathers every <a href> across the whole document (not just the
// readability article), resolved to absolute and de-duplicated by URL keeping
// the first anchor text seen. This is what backs web_links: it lets the model
// enumerate a page's outbound links without scraping the rendered Markdown.
func collectLinks(root *xhtml.Node, base *url.URL) []Link {
	var links []Link
	seen := map[string]bool{}
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if len(links) >= maxLinks {
			return
		}
		if n.Type == xhtml.ElementNode && n.Data == "a" {
			if abs := resolveHref(attrVal(n, "href"), base); abs != "" && !seen[abs] {
				seen[abs] = true
				links = append(links, Link{URL: abs, Text: oneLine(nodeText(n))})
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(root)
	return links
}

// resolveHref resolves a raw href against base, returning "" for empty,
// fragment-only, javascript:, and data: links. Relative URLs become absolute.
func resolveHref(raw string, base *url.URL) string {
	h := strings.TrimSpace(raw)
	if h == "" || strings.HasPrefix(h, "#") {
		return ""
	}
	switch low := strings.ToLower(h); {
	case strings.HasPrefix(low, "javascript:"), strings.HasPrefix(low, "data:"):
		return ""
	}
	ref, err := url.Parse(h)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// FormatLinks renders the link list for web_links as a compact, model-readable
// list. URLs are absolute; anchor text follows on an indented line when present.
func FormatLinks(pageURL string, links []Link) string {
	if len(links) == 0 {
		return fmt.Sprintf("No links found on %s.", pageURL)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d link(s) on %s:\n", len(links), pageURL)
	if len(links) >= maxLinks {
		fmt.Fprintf(&b, "(list capped at %d — the page may contain more; use web_fetch_raw to inspect the full source)\n", maxLinks)
	}
	for _, l := range links {
		fmt.Fprintf(&b, "\n%s", l.URL)
		if l.Text != "" {
			fmt.Fprintf(&b, "\n   text: %s", l.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
