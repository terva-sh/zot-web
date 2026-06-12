package fetch

import (
	"net/url"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

func collectFrom(t *testing.T, htmlSrc string) []Image {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader(htmlSrc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	base, _ := url.Parse("https://example.com/page")
	return collectImages(doc, base)
}

func urls(imgs []Image) []string {
	out := make([]string, len(imgs))
	for i, im := range imgs {
		out[i] = im.URL
	}
	return out
}

func TestCollectImagesNoscriptFallback(t *testing.T) {
	imgs := collectFrom(t, `<html><body>
<img src="data:image/gif;base64,R0lGOD" data-nothing="x">
<noscript><img src="/real/photo.jpg" alt="real"></noscript>
</body></html>`)
	found := false
	for _, u := range urls(imgs) {
		if u == "https://example.com/real/photo.jpg" {
			found = true
		}
	}
	if !found {
		t.Errorf("noscript <img> fallback not harvested: %v", urls(imgs))
	}
}

func TestCollectImagesDataBackground(t *testing.T) {
	imgs := collectFrom(t, `<html><body>
<div data-bg="/hero.webp"></div>
<section data-background-image="url('https://cdn.example.com/banner.png')"></section>
</body></html>`)
	got := strings.Join(urls(imgs), " ")
	if !strings.Contains(got, "https://example.com/hero.webp") {
		t.Errorf("data-bg not harvested: %v", got)
	}
	if !strings.Contains(got, "https://cdn.example.com/banner.png") {
		t.Errorf("data-background-image url(...) not harvested: %v", got)
	}
}

func TestStripCSSURL(t *testing.T) {
	cases := [][2]string{
		{"/a.png", "/a.png"},
		{"url(/a.png)", "/a.png"},
		{`url("/a.png")`, "/a.png"},
		{"URL('/a.png')", "/a.png"},
	}
	for _, c := range cases {
		if got := stripCSSURL(c[0]); got != c[1] {
			t.Errorf("stripCSSURL(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}
