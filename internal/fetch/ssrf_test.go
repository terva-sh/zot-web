package fetch

import (
	"context"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
)

func TestIsBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"10.1.2.3":        true,  // private
		"192.168.0.1":     true,  // private
		"172.16.5.5":      true,  // private
		"169.254.169.254": true,  // link-local / cloud metadata
		"100.64.1.1":      true,  // CGNAT
		"192.0.2.10":      true,  // TEST-NET-1 documentation
		"198.18.0.1":      true,  // benchmarking
		"198.51.100.10":   true,  // TEST-NET-2 documentation
		"203.0.113.10":    true,  // TEST-NET-3 documentation
		"240.0.0.1":       true,  // reserved
		"::1":             true,  // loopback v6
		"fc00::1":         true,  // ULA
		"0.0.0.0":         true,  // unspecified
		"2001:db8::1":     true,  // documentation v6
		"2002::1":         true,  // 6to4 special-use v6
		"8.8.8.8":         false, // public
		"1.1.1.1":         false, // public
		"93.184.216.34":   false, // public
		"2001:4860::8888": false, // public v6
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test ip %q", s)
		}
		if got := isBlockedIP(ip); got != want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestAllowListPermitted(t *testing.T) {
	a := ParseAllowList([]string{"localhost", "10.0.0.0/24", "192.168.1.50", "grafana.internal"})
	check := func(host, ip string, want bool) {
		t.Helper()
		if got := a.permitted(host, net.ParseIP(ip)); got != want {
			t.Errorf("permitted(%q, %s) = %v, want %v", host, ip, got, want)
		}
	}
	check("example.com", "8.8.8.8", true)       // public always ok
	check("evil.internal", "172.16.0.1", false) // unlisted private blocked
	check("box", "10.0.1.5", false)             // outside allowlisted /24
	check("box", "192.168.1.51", false)         // not the allowlisted exact IP
	check("localhost", "127.0.0.1", true)       // hostname allowlist
	check("grafana.internal", "10.5.5.5", true) // hostname allowlist
	check("GRAFANA.INTERNAL", "10.5.5.5", true) // case-insensitive
	check("box", "10.0.0.5", true)              // inside the /24
	check("box", "192.168.1.50", true)          // exact IP
}

// testClient builds a Client suitable for render/extract unit tests (no network
// is exercised by render itself).
func testClient() *Client {
	return New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5}, ParseAllowList(nil))
}

// TestRenderReadability feeds a realistic article wrapped in nav/script/footer
// chrome and asserts the readability+markdown pipeline keeps the body and drops
// the noise. Assertions hold whether the readability path or the heuristic
// fallback runs (both strip <script> and keep visible text).
func TestRenderReadability(t *testing.T) {
	page := `<html><head><title>Widget Guide</title><style>.a{color:red}</style></head>
<body>
<nav><a href="/">Home</a> <a href="/login">Log in</a></nav>
<script>tracker();</script>
<article>
<h1>All About Widgets</h1>
<p>Widgets are small components that do useful things. This paragraph explains the basics in enough detail to read like real article content.</p>
<p>The second paragraph continues the discussion, giving the readability algorithm enough text to recognize this as the page's main content.</p>
<p>A third paragraph ensures there is sufficient length for extraction to succeed reliably.</p>
</article>
<footer>Copyright 2026 Widget Co.</footer>
</body></html>`
	u, _ := url.Parse("https://example.com/widgets")
	p := testClient().render(u, "text/html", []byte(page))
	out := p.Title + "\n" + p.Markdown
	if !strings.Contains(out, "Widgets are small components") {
		t.Errorf("missing article body in %q", out)
	}
	if !strings.Contains(out, "second paragraph") {
		t.Errorf("missing later paragraph in %q", out)
	}
	if !strings.Contains(out, "Widget Guide") {
		t.Errorf("missing title in %q", out)
	}
	if strings.Contains(out, "tracker()") || strings.Contains(out, "color:red") {
		t.Errorf("script/style not stripped: %q", out)
	}
}

// TestRenderTable confirms the GFM table plugin is active: a real <table>
// becomes a pipe table rather than linearized text.
func TestRenderTable(t *testing.T) {
	page := `<html><body><article>
<h1>Specs</h1>
<p>The following table lists the specifications in a structured form for the reader.</p>
<table>
<tr><th>Name</th><th>Value</th></tr>
<tr><td>Width</td><td>10cm</td></tr>
<tr><td>Height</td><td>20cm</td></tr>
</table>
<p>That concludes the specifications section of this document about the product.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/specs")
	p := testClient().render(u, "text/html", []byte(page))
	if !strings.Contains(p.Markdown, "|") || !strings.Contains(p.Markdown, "Width") {
		t.Errorf("expected a pipe table with cell content, got:\n%s", p.Markdown)
	}
}

// TestRenderImages checks that <img> URLs are indexed out to [image:N]
// placeholders (with alt), resolved to absolute URLs, and that data: URIs are
// skipped.
func TestRenderImages(t *testing.T) {
	page := `<html><body><article>
<h1>Gallery</h1>
<p>This article includes a picture to demonstrate the image extraction behavior end to end.</p>
<p><img src="/pics/cat.png" alt="A cat"> some text after the image to keep the paragraph long enough.</p>
<p>Here is an inline data image that must be ignored: <img src="data:image/gif;base64,R0lGOD999"> and more trailing text.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/gallery")
	p := testClient().render(u, "text/html", []byte(page))
	if len(p.Images) != 1 {
		t.Fatalf("expected exactly 1 indexed image (data: skipped), got %d: %+v", len(p.Images), p.Images)
	}
	if p.Images[0].URL != "https://example.com/pics/cat.png" {
		t.Errorf("relative src not resolved to absolute: %q", p.Images[0].URL)
	}
	if !strings.Contains(p.Markdown, "[image:1: A cat]") {
		t.Errorf("placeholder with alt missing from markdown:\n%s", p.Markdown)
	}
	if strings.Contains(p.Markdown, "cat.png") || strings.Contains(p.Markdown, "{{IMG") {
		t.Errorf("raw url or sentinel leaked into markdown:\n%s", p.Markdown)
	}
}

// TestImageMetadata checks the enriched fields: dimensions, figcaption, and the
// enclosing source-page link.
func TestImageMetadata(t *testing.T) {
	page := `<html><body><article>
<h1>Gallery</h1>
<p>An introductory paragraph long enough for readability to treat this as the page's main content here.</p>
<figure><a href="/wiki/File:Cat.png"><img src="/thumb/cat.png" alt="A cat" width="800" height="600"></a><figcaption>A fluffy cat sitting</figcaption></figure>
<p>A closing paragraph adding more body text so the article is clearly long enough for reliable extraction.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/gallery")
	p := testClient().render(u, "text/html", []byte(page))
	if len(p.Images) != 1 {
		t.Fatalf("want 1 image, got %d: %+v", len(p.Images), p.Images)
	}
	im := p.Images[0]
	if im.URL != "https://example.com/thumb/cat.png" {
		t.Errorf("url: %q", im.URL)
	}
	if im.Width != "800" || im.Height != "600" {
		t.Errorf("dimensions: %q×%q", im.Width, im.Height)
	}
	if im.Caption != "A fluffy cat sitting" {
		t.Errorf("caption: %q", im.Caption)
	}
	if im.SourcePage != "https://example.com/wiki/File:Cat.png" {
		t.Errorf("source page: %q", im.SourcePage)
	}
}

// TestRenderInlineImages: with inline images configured, URLs stay in the
// markdown and nothing is indexed.
func TestRenderInlineImages(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5, FetchInlineImages: true}, ParseAllowList(nil))
	page := `<html><body><article><h1>G</h1>
<p>A paragraph long enough for readability to treat this as the article content here.</p>
<p><img src="https://cdn.example.com/x.png" alt="x"> trailing text to lengthen the paragraph body.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/g")
	p := c.render(u, "text/html", []byte(page))
	if len(p.Images) != 0 {
		t.Errorf("inline mode should not index images, got %+v", p.Images)
	}
	if !strings.Contains(p.Markdown, "x.png") {
		t.Errorf("inline mode should keep the image URL in markdown:\n%s", p.Markdown)
	}
}

// TestExtractHeuristicFallback covers the tag-stripper directly: it must drop
// script/style and unescape entities for bodies readability can't handle.
func TestExtractHeuristicFallback(t *testing.T) {
	page := `<html><head><style>.x{color:red}</style></head>` +
		`<body><script>evil()</script><h1>Hello</h1><p>World &amp; more</p></body></html>`
	got := heuristicExtract([]byte(page))
	if !strings.Contains(got, "Hello") {
		t.Errorf("missing visible text in %q", got)
	}
	if !strings.Contains(got, "World & more") {
		t.Errorf("entities not unescaped: %q", got)
	}
	if strings.Contains(got, "evil()") || strings.Contains(got, "color:red") {
		t.Errorf("script/style not stripped: %q", got)
	}
}

// TestRenderNonHTML returns non-HTML bodies verbatim (no title, no images).
func TestRenderNonHTML(t *testing.T) {
	u, _ := url.Parse("https://example.com/data.txt")
	p := testClient().render(u, "text/plain", []byte("  plain body  "))
	if p.Title != "" {
		t.Errorf("non-HTML should have no title, got %q", p.Title)
	}
	if p.Markdown != "plain body" {
		t.Errorf("non-HTML body should pass through trimmed, got %q", p.Markdown)
	}
}

// TestRenderBinarySuppressed: binary content is summarized, not dumped.
func TestRenderBinarySuppressed(t *testing.T) {
	u, _ := url.Parse("https://example.com/cat.png")
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 200)...) // NUL-laden binary
	p := testClient().render(u, "image/png", body)
	if !strings.Contains(p.Markdown, "image/png") || !strings.Contains(p.Markdown, "not rendered as text") {
		t.Errorf("expected a binary summary, got: %q", p.Markdown)
	}
	if strings.Contains(p.Markdown, "PNG") {
		t.Errorf("raw bytes leaked into output: %q", p.Markdown)
	}
}

// TestRenderSVGIsText: SVG is XML text and should pass through, not be summarized.
func TestRenderSVGIsText(t *testing.T) {
	u, _ := url.Parse("https://example.com/logo.svg")
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><title>Logo</title></svg>`
	p := testClient().render(u, "image/svg+xml", []byte(svg))
	if !strings.Contains(p.Markdown, "svg") {
		t.Errorf("svg should pass through as text, got: %q", p.Markdown)
	}
}

// TestRenderResolvesLinksAgainstBase: relative links resolve against the base
// URL render is given (load passes the post-redirect URL).
func TestRenderResolvesLinksAgainstBase(t *testing.T) {
	page := `<html><body><article>
<h1>Title</h1>
<p>See <a href="/wiki/Foo">Foo</a> in this paragraph that is long enough to read as the page's main article content here.</p>
<p>A second paragraph giving readability enough text to lock onto this region as the body.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/page")
	p := testClient().render(u, "text/html", []byte(page))
	if !strings.Contains(p.Markdown, "https://example.com/wiki/Foo") {
		t.Errorf("relative link not resolved against base:\n%s", p.Markdown)
	}
}

// TestFetchOffsetWindowing drives the metadata block + offset paging using a
// pre-seeded cache entry (so no network is touched).
func TestFetchOffsetWindowing(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5, FetchCacheMaxEntries: 4, FetchCacheTTLSec: 60}, ParseAllowList(nil))
	u, _ := url.Parse("https://example.com/big")
	body := strings.Repeat("0123456789", 50) // 500 runes
	c.cache.put(page{URL: u.String(), Title: "Big", Markdown: body, ContentType: "text/html; charset=utf-8"})

	out, err := c.Fetch(context.Background(), u.String(), 100, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Chars: 0-100 of 500") {
		t.Errorf("missing/incorrect char metadata:\n%s", out)
	}
	if !strings.Contains(out, "Content-Type: text/html; charset=utf-8") {
		t.Errorf("missing content-type metadata:\n%s", out)
	}
	if !strings.Contains(out, "continue with offset=100") {
		t.Errorf("missing continuation hint:\n%s", out)
	}

	// Final window: no continuation hint when we reach the end.
	out2, err := c.Fetch(context.Background(), u.String(), 100, 450, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "Chars: 450-500 of 500") {
		t.Errorf("incorrect tail window metadata:\n%s", out2)
	}
	if strings.Contains(out2, "continue with offset") {
		t.Errorf("should not advertise more chars at the end:\n%s", out2)
	}
}

// TestFetchBlocksPrivate exercises the real DialContext gate: a loopback target
// with no allowlist must be refused before any connection is made.
func TestFetchBlocksPrivate(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5}, ParseAllowList(nil))
	_, err := c.Fetch(context.Background(), "http://127.0.0.1:1/", 0, 0, "")
	if err == nil {
		t.Fatal("expected loopback fetch to be blocked")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected an SSRF block error, got: %v", err)
	}
}

// TestFetchAllowlistAllowsPrivate confirms an allowlisted loopback gets PAST the
// SSRF gate (it then fails to connect to the closed port, which is fine — the
// point is the error is a connection error, not a block).
func TestFetchAllowlistAllowsPrivate(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5}, ParseAllowList([]string{"127.0.0.1"}))
	_, err := c.Fetch(context.Background(), "http://127.0.0.1:1/", 0, 0, "")
	if err != nil && strings.Contains(err.Error(), "blocked") {
		t.Fatalf("allowlisted loopback should not be SSRF-blocked: %v", err)
	}
}

func TestFetchRejectsNonHTTPScheme(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5}, ParseAllowList(nil))
	_, err := c.Fetch(context.Background(), "ftp://example.com/x", 0, 0, "")
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("expected scheme rejection, got: %v", err)
	}
}

func TestParseURLBlocksServicePorts(t *testing.T) {
	for _, raw := range []string{
		"http://example.com:22/",
		"https://example.com:3306/db",
		"http://example.com:6379/",
		"https://example.com:25/",
	} {
		if _, err := parseURL(raw); err == nil || !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("parseURL(%q) = %v, want a port-not-permitted rejection", raw, err)
		}
	}
	// Web ports and the default (no port) are fine.
	for _, raw := range []string{
		"http://example.com/",
		"https://example.com:443/",
		"http://example.com:8080/",
		"https://example.com:8443/",
		"http://example.com:3000/",
	} {
		if _, err := parseURL(raw); err != nil {
			t.Errorf("parseURL(%q) = %v, want allowed", raw, err)
		}
	}
}

func TestFetchBlocksServicePort(t *testing.T) {
	// Even an allowlisted host can't be used to reach a blocked service port:
	// the rejection happens at URL parse, before any dial.
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5}, ParseAllowList([]string{"127.0.0.1"}))
	if _, err := c.Fetch(context.Background(), "http://127.0.0.1:22/", 0, 0, ""); err == nil ||
		!strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected port-22 rejection, got: %v", err)
	}
}

func TestCacheKeyStripsOnlyKnownTrackingParams(t *testing.T) {
	got := cacheKey("https://example.com/page?b=2&utm_source=x&a=1&fbclid=y")
	want := "https://example.com/page?a=1&b=2"
	if got != want {
		t.Fatalf("cacheKey stripped known trackers = %q, want %q", got, want)
	}

	semantic := "https://example.com/page?ref=docs&r=2&t=chapter&cache=no&cb=variant&rand=seed&_=underscore"
	if got := cacheKey(semantic); got != semantic {
		t.Fatalf("cacheKey should preserve ambiguous params: got %q, want %q", got, semantic)
	}
}

func TestCapMarkdownDoesNotSplitRunes(t *testing.T) {
	prefix := strings.Repeat("a", maxRenderedRunes-1) + "☃"
	input := prefix + "tail"
	got, capped := capMarkdown(input)
	if !capped {
		t.Fatal("capMarkdown should report that the cap fired")
	}
	if !strings.HasPrefix(got, prefix) {
		t.Fatal("capMarkdown did not preserve the complete rune at the boundary")
	}
	if strings.Contains(got, "tail") {
		t.Fatal("capMarkdown did not truncate content beyond the rune cap")
	}
	if !strings.Contains(got, "Markdown output capped") {
		t.Fatal("capMarkdown should append a truncation note")
	}
}
