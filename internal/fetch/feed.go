package fetch

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// Feeds (RSS 0.9x/1.0/2.0 and Atom) used to fall through to the heuristic
// tag-stripper as linearized noise. renderFeed detects them by content type or
// XML root element and renders one Markdown block per entry (title, date,
// link, summary), which is what a model wants from a feed: a scannable list
// it can chain into web_fetch.

// maxFeedItems caps how many entries a rendered feed keeps.
const maxFeedItems = 100

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	DCDate      string `xml:"date"` // dc:date (RSS 1.0)
}

// rssFeed covers RSS 2.0/0.9x (<rss><channel><item>…) and RSS 1.0
// (<rdf:RDF> with a <channel> plus top-level <item> elements).
type rssFeed struct {
	Channel struct {
		Title       string    `xml:"title"`
		Description string    `xml:"description"`
		Items       []rssItem `xml:"item"`
	} `xml:"channel"`
	Items []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Links     []atomLink `xml:"link"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
}

type atomFeed struct {
	Title    string      `xml:"title"`
	Subtitle string      `xml:"subtitle"`
	Entries  []atomEntry `xml:"entry"`
}

// renderFeed renders body as a feed when it is one, reporting ok=false for
// everything else (the caller then continues with the HTML/text pipeline).
func renderFeed(contentType string, body []byte) (page, bool) {
	ct := strings.ToLower(contentType)
	xmlish := strings.Contains(ct, "rss") || strings.Contains(ct, "atom") ||
		strings.Contains(ct, "xml") || ct == "" || strings.HasPrefix(ct, "text/")
	if !xmlish {
		return page{}, false
	}
	switch xmlRootName(body) {
	case "rss", "RDF":
		var f rssFeed
		if decodeXML(body, &f) != nil {
			return page{}, false
		}
		items := f.Channel.Items
		if len(items) == 0 {
			items = f.Items // RSS 1.0 keeps items outside <channel>
		}
		return feedPage(f.Channel.Title, f.Channel.Description, rssEntries(items)), true
	case "feed":
		var f atomFeed
		if decodeXML(body, &f) != nil {
			return page{}, false
		}
		return feedPage(f.Title, f.Subtitle, atomEntries(f.Entries)), true
	}
	return page{}, false
}

// feedEntry is one normalized feed item, whatever the source format.
type feedEntry struct {
	title, link, date, summary string
}

func rssEntries(items []rssItem) []feedEntry {
	out := make([]feedEntry, 0, len(items))
	for _, it := range items {
		date := it.PubDate
		if date == "" {
			date = it.DCDate
		}
		out = append(out, feedEntry{
			title:   strings.TrimSpace(it.Title),
			link:    strings.TrimSpace(it.Link),
			date:    feedDate(date),
			summary: feedSummary(it.Description),
		})
	}
	return out
}

func atomEntries(entries []atomEntry) []feedEntry {
	out := make([]feedEntry, 0, len(entries))
	for _, en := range entries {
		date := en.Published
		if date == "" {
			date = en.Updated
		}
		summary := en.Summary
		if summary == "" {
			summary = en.Content
		}
		out = append(out, feedEntry{
			title:   strings.TrimSpace(en.Title),
			link:    atomAltLink(en.Links),
			date:    feedDate(date),
			summary: feedSummary(summary),
		})
	}
	return out
}

// atomAltLink picks the entry's main link: rel="alternate" (or no rel) wins,
// any link is the fallback.
func atomAltLink(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

// feedPage renders the normalized entries as Markdown.
func feedPage(title, subtitle string, entries []feedEntry) page {
	var b strings.Builder
	if s := oneLine(subtitle); s != "" {
		fmt.Fprintf(&b, "%s\n", s)
	}
	total := len(entries)
	if total > maxFeedItems {
		entries = entries[:maxFeedItems]
	}
	fmt.Fprintf(&b, "\nFeed with %d entries", total)
	if total > len(entries) {
		fmt.Fprintf(&b, " (showing the first %d)", len(entries))
	}
	b.WriteString(":\n")
	for _, en := range entries {
		t := en.title
		if t == "" {
			t = "(untitled)"
		}
		fmt.Fprintf(&b, "\n- **%s**", t)
		if en.date != "" {
			fmt.Fprintf(&b, " — %s", en.date)
		}
		if en.link != "" {
			fmt.Fprintf(&b, "\n  %s", en.link)
		}
		if en.summary != "" {
			fmt.Fprintf(&b, "\n  %s", en.summary)
		}
		b.WriteString("\n")
	}
	return cappedPage(page{Title: strings.TrimSpace(title)}, strings.TrimSpace(b.String()))
}

// feedSummary strips the HTML feeds commonly embed in descriptions and caps
// the result to keep entry lists compact.
func feedSummary(s string) string {
	s = oneLine(heuristicExtract([]byte(s)))
	const max = 400
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// feedDate normalizes the date formats feeds use to YYYY-MM-DD, passing
// unparseable values through as-is.
func feedDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "2006-01-02", "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return oneLine(s)
}

// xmlRootName returns the local name of the document's root element ("" when
// body isn't well-formed XML up to that point).
func xmlRootName(body []byte) string {
	d := newXMLDecoder(body)
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

func decodeXML(body []byte, v any) error {
	return newXMLDecoder(body).Decode(v)
}

// newXMLDecoder builds a lenient decoder: legacy charsets are converted via
// the same machinery the HTML path uses, and Strict is off because real-world
// feeds are full of small wellformedness sins.
func newXMLDecoder(body []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = charset.NewReaderLabel
	d.Strict = false
	return d
}
