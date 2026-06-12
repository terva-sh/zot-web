package fetch

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
	xhtml "golang.org/x/net/html"
)

func parseHTML(t *testing.T, s string) *xhtml.Node {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func imageURLs(imgs []Image) map[string]bool {
	set := map[string]bool{}
	for _, im := range imgs {
		set[im.URL] = true
	}
	return set
}

// TestCollectImages exercises the whole-page harvester: lazy-load attrs,
// <picture><source>, <a href> straight to an image, and og:image — the cases
// that defeated the old src-only <img> scan.
func TestCollectImages(t *testing.T) {
	html := `<html><head>
<meta property="og:image" content="https://cdn.example.com/social.png">
</head><body>
<img src="data:image/gif;base64,AAAA" data-src="/lazy/pic1.png" alt="lazy">
<picture><source srcset="/srcset/pic2.webp 1x, /srcset/pic2-2x.webp 2x"><img src="/fallback.png"></picture>
<a href="/full/pic3.jpg"><img src="/thumb/pic3.png"></a>
<a href="/not-an-image">just text</a>
</body></html>`
	u, _ := url.Parse("https://example.com/board")
	got := imageURLs(collectImages(parseHTML(t, html), u))

	want := []string{
		"https://cdn.example.com/social.png", // og:image
		"https://example.com/lazy/pic1.png",  // data-src (data: src ignored)
		"https://example.com/srcset/pic2.webp",
		"https://example.com/fallback.png",
		"https://example.com/full/pic3.jpg", // <a> to full image
		"https://example.com/thumb/pic3.png",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing image %q in %v", w, got)
		}
	}
	if got["https://example.com/not-an-image"] {
		t.Errorf("non-image <a> href was collected as an image")
	}
}

// TestImgURLLazyLoad confirms a real src wins over data-src, but a data: src
// defers to it.
func TestImgURLLazyLoad(t *testing.T) {
	u, _ := url.Parse("https://example.com/")
	cases := []struct{ html, want string }{
		{`<img src="https://example.com/real.png" data-src="/lazy.png">`, "https://example.com/real.png"},
		{`<img src="data:image/gif;base64,AA" data-src="/lazy.png">`, "https://example.com/lazy.png"},
		{`<img data-original="/orig.png">`, "https://example.com/orig.png"},
		{`<img data-srcset="/a.png 1x, /b.png 2x">`, "https://example.com/a.png"},
	}
	for _, c := range cases {
		doc := parseHTML(t, c.html)
		img := firstDescendant(doc, "img")
		if img == nil {
			t.Fatalf("no <img> in %q", c.html)
		}
		if got := imgURL(img, u); got != c.want {
			t.Errorf("imgURL(%q) = %q, want %q", c.html, got, c.want)
		}
	}
}

// TestCollectLinks gathers links across the whole document, absolute and
// de-duplicated, skipping fragments and javascript:.
func TestCollectLinks(t *testing.T) {
	html := `<html><body>
<nav><a href="/">Home</a><a href="/login">Login</a></nav>
<article><a href="https://other.com/x">X</a><a href="/">Home again</a></article>
<a href="#frag">frag</a><a href="javascript:void(0)">js</a>
</body></html>`
	u, _ := url.Parse("https://example.com/page")
	links := collectLinks(parseHTML(t, html), u)

	set := map[string]string{}
	for _, l := range links {
		set[l.URL] = l.Text
	}
	for _, w := range []string{"https://example.com/", "https://example.com/login", "https://other.com/x"} {
		if _, ok := set[w]; !ok {
			t.Errorf("missing link %q in %v", w, set)
		}
	}
	if len(links) != 3 {
		t.Errorf("want 3 deduped links, got %d: %v", len(links), set)
	}
	if set["https://example.com/"] != "Home" {
		t.Errorf("dedup should keep first anchor text, got %q", set["https://example.com/"])
	}
}

// TestRenderFallbackImagesAndLinks: an article with no <img> of its own still
// gets page-level images (og:image, nav logo) via the full-document fallback,
// flagged as not-inline, plus its links.
func TestRenderFallbackImagesAndLinks(t *testing.T) {
	page := `<html><head><meta property="og:image" content="https://cdn.example.com/og.png"></head>
<body>
<nav><a href="/login">Log in</a><img src="/logo.png"></nav>
<article><h1>Title</h1>
<p>A first paragraph that is clearly long enough for readability to lock onto this region as the page's main content body.</p>
<p>A second paragraph adding more text so extraction reliably succeeds without any image of its own.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/post")
	p := testClient().render(u, "text/html", []byte(page))

	imgs := imageURLs(p.Images)
	if !imgs["https://cdn.example.com/og.png"] {
		t.Errorf("og:image not harvested in fallback: %v", imgs)
	}
	if p.ImagesInline {
		t.Errorf("fallback images must not be flagged inline")
	}
	if strings.Contains(p.Markdown, "[image:") {
		t.Errorf("fallback render should have no inline [image:N] placeholders:\n%s", p.Markdown)
	}
	var loginSeen bool
	for _, l := range p.Links {
		if l.URL == "https://example.com/login" {
			loginSeen = true
		}
	}
	if !loginSeen {
		t.Errorf("expected nav link to be collected, got %+v", p.Links)
	}
}

// TestRenderImagesStillInline keeps the original contract: an article with its
// own <img> indexes it inline as [image:N] and flags ImagesInline.
func TestRenderImagesStillInline(t *testing.T) {
	page := `<html><body><article>
<h1>Gallery</h1>
<p>This article includes a picture to demonstrate the image extraction behavior end to end here.</p>
<p><img src="/pics/cat.png" alt="A cat"> trailing text to keep the paragraph long enough for extraction.</p>
</article></body></html>`
	u, _ := url.Parse("https://example.com/gallery")
	p := testClient().render(u, "text/html", []byte(page))
	if !p.ImagesInline {
		t.Errorf("article image should be flagged inline")
	}
	if !strings.Contains(p.Markdown, "[image:1: A cat]") {
		t.Errorf("inline placeholder missing:\n%s", p.Markdown)
	}
}

// TestGzipRoundTrip covers the raw-body compression used by the page cache.
func TestGzipRoundTrip(t *testing.T) {
	orig := []byte(strings.Repeat("<html><body>hello world</body></html>", 200))
	gz := gzipBytes(orig)
	if len(gz) == 0 || len(gz) >= len(orig) {
		t.Fatalf("expected compression, got %d bytes from %d", len(gz), len(orig))
	}
	out, err := gunzipBytes(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, orig) {
		t.Errorf("round-trip mismatch")
	}
	if gzipBytes(nil) != nil {
		t.Errorf("gzipBytes(nil) should be nil")
	}
	if out, err := gunzipBytes(nil); err != nil || out != nil {
		t.Errorf("gunzipBytes(nil) = (%v, %v)", out, err)
	}
}

// TestRawServedFromCache: Raw decompresses the cached body without a network
// fetch (the web_fetch -> web_fetch_raw warm-cache path).
func TestRawServedFromCache(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5, FetchCacheMaxEntries: 4, FetchCacheTTLSec: 60}, ParseAllowList(nil))
	u, _ := url.Parse("https://example.com/p")
	body := []byte("<html><body>raw &amp; unrendered</body></html>")
	c.cache.put(page{URL: u.String(), RawGzip: gzipBytes(body), ContentType: "text/html; charset=utf-8", FinalURL: u.String()})

	raw, err := c.Raw(context.Background(), u.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw.Body, body) {
		t.Errorf("raw body mismatch: %q", raw.Body)
	}
	if raw.ContentType != "text/html; charset=utf-8" {
		t.Errorf("content-type: %q", raw.ContentType)
	}
}

// TestFetchHintNotInlined: when images were harvested without inline
// placeholders, the web_fetch header says so rather than pointing at [image:N].
func TestFetchHintNotInlined(t *testing.T) {
	c := New(config.Config{FetchMaxBytes: 1 << 20, FetchTimeoutSec: 5, FetchCacheMaxEntries: 4, FetchCacheTTLSec: 60}, ParseAllowList(nil))
	u, _ := url.Parse("https://example.com/board")
	c.cache.put(page{
		URL:          u.String(),
		Markdown:     "thread text with no placeholders",
		ContentType:  "text/html",
		Images:       []Image{{ID: 1, URL: "https://example.com/a.png"}},
		ImagesInline: false,
	})
	out, err := c.Fetch(context.Background(), u.String(), 1000, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not inlined; list URLs with web_images") {
		t.Errorf("expected not-inlined hint, got:\n%s", out)
	}
}
