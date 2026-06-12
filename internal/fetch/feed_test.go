package fetch

import (
	"net/url"
	"strings"
	"testing"
)

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
<title>Example Blog</title>
<description>Posts about examples</description>
<item>
  <title>First Post</title>
  <link>https://blog.example/first</link>
  <pubDate>Mon, 01 Jun 2026 10:00:00 +0000</pubDate>
  <description><![CDATA[<p>Some <b>HTML</b> summary text.</p>]]></description>
</item>
<item>
  <title>Second Post</title>
  <link>https://blog.example/second</link>
  <pubDate>Tue, 02 Jun 2026 10:00:00 +0000</pubDate>
  <description>Plain summary</description>
</item>
</channel>
</rss>`

const atomFixture = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom Example</title>
  <entry>
    <title>Entry One</title>
    <link rel="alternate" href="https://atom.example/one"/>
    <published>2026-05-30T08:00:00Z</published>
    <summary>An atom entry summary.</summary>
  </entry>
</feed>`

func TestRenderFeedRSS(t *testing.T) {
	p, ok := renderFeed("application/rss+xml", []byte(rssFixture))
	if !ok {
		t.Fatal("RSS fixture not detected as a feed")
	}
	if p.Title != "Example Blog" {
		t.Errorf("title = %q", p.Title)
	}
	for _, want := range []string{
		"Feed with 2 entries",
		"**First Post** — 2026-06-01",
		"https://blog.example/first",
		"Some HTML summary text.",
		"**Second Post** — 2026-06-02",
	} {
		if !strings.Contains(p.Markdown, want) {
			t.Errorf("feed render missing %q:\n%s", want, p.Markdown)
		}
	}
	if strings.Contains(p.Markdown, "<p>") {
		t.Error("feed summary leaked raw HTML")
	}
}

func TestRenderFeedAtom(t *testing.T) {
	// Served with a generic XML type: detection must fall back to the root element.
	p, ok := renderFeed("application/xml", []byte(atomFixture))
	if !ok {
		t.Fatal("Atom fixture not detected as a feed")
	}
	if p.Title != "Atom Example" {
		t.Errorf("title = %q", p.Title)
	}
	for _, want := range []string{"**Entry One** — 2026-05-30", "https://atom.example/one", "An atom entry summary."} {
		if !strings.Contains(p.Markdown, want) {
			t.Errorf("feed render missing %q:\n%s", want, p.Markdown)
		}
	}
}

func TestRenderFeedRejectsNonFeeds(t *testing.T) {
	if _, ok := renderFeed("application/xml", []byte(`<?xml version="1.0"?><svg></svg>`)); ok {
		t.Error("SVG misdetected as a feed")
	}
	if _, ok := renderFeed("text/plain", []byte("just some text")); ok {
		t.Error("plain text misdetected as a feed")
	}
	if _, ok := renderFeed("application/json", []byte(`{"a":1}`)); ok {
		t.Error("JSON misdetected as a feed")
	}
}

// TestRenderRoutesFeeds exercises the full render path: a feed content type
// must produce the per-entry list, not the raw-XML text passthrough.
func TestRenderRoutesFeeds(t *testing.T) {
	u, _ := url.Parse("https://blog.example/feed.xml")
	p := testClient().render(u, "application/rss+xml; charset=utf-8", []byte(rssFixture))
	if !strings.Contains(p.Markdown, "Feed with 2 entries") {
		t.Errorf("render did not route to the feed renderer:\n%.300s", p.Markdown)
	}
}
