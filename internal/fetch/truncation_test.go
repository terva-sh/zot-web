package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
)

func TestFormatLinksCapNote(t *testing.T) {
	links := make([]Link, maxLinks)
	for i := range links {
		links[i] = Link{URL: fmt.Sprintf("https://example.com/%d", i)}
	}
	out := FormatLinks("https://example.com", links)
	if !strings.Contains(out, fmt.Sprintf("capped at %d", maxLinks)) {
		t.Error("FormatLinks at the harvest cap should warn that the list may be incomplete")
	}
	if out2 := FormatLinks("https://example.com", links[:3]); strings.Contains(out2, "capped") {
		t.Error("FormatLinks below the cap should not warn")
	}
}

func TestFormatImagesCapNote(t *testing.T) {
	imgs := make([]Image, maxImages)
	for i := range imgs {
		imgs[i] = Image{ID: i + 1, URL: fmt.Sprintf("https://example.com/%d.png", i)}
	}
	out := FormatImages("https://example.com", imgs)
	if !strings.Contains(out, fmt.Sprintf("capped at %d", maxImages)) {
		t.Error("FormatImages at the harvest cap should warn that the list may be incomplete")
	}
	if out2 := FormatImages("https://example.com", imgs[:3]); strings.Contains(out2, "capped") {
		t.Error("FormatImages below the cap should not warn")
	}
}

func TestFormatImagesAnnotatesSVG(t *testing.T) {
	imgs := []Image{
		{ID: 1, URL: "https://example.com/logo.SVG?v=2"},
		{ID: 2, URL: "https://example.com/photo.png"},
	}
	out := FormatImages("https://example.com", imgs)
	lines := strings.Split(out, "\n")
	var svgNotes int
	for _, l := range lines {
		if strings.Contains(l, "web_fetch_image cannot decode") {
			svgNotes++
		}
	}
	if svgNotes != 1 {
		t.Errorf("want exactly 1 svg note (case-insensitive ext, query ignored; none for png), got %d in:\n%s", svgNotes, out)
	}
}

// TestFetchHeaderNotesMarkdownCap serves a plain-text body longer than the
// render cap and asserts the fetch metadata block calls out the dropped tail.
func TestFetchHeaderNotesMarkdownCap(t *testing.T) {
	big := strings.Repeat("a", maxRenderedRunes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(big))
	}))
	t.Cleanup(srv.Close)
	c := New(config.Config{FetchMaxBytes: int64(len(big) + 1), FetchTimeoutSec: 10}, ParseAllowList([]string{"127.0.0.1"}))
	out, err := c.Fetch(context.Background(), srv.URL, 200, 0, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(out, "rune output cap") {
		t.Errorf("fetch header missing the output-cap note:\n%.600s", out)
	}
}
