package fetch

import (
	"net/url"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
)

func TestDecodeToUTF8Windows1252Header(t *testing.T) {
	body, err := charmap.Windows1252.NewEncoder().Bytes([]byte("café déjà-vu"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(decodeToUTF8(body, "text/plain; charset=windows-1252"))
	if got != "café déjà-vu" {
		t.Errorf("decoded = %q, want %q", got, "café déjà-vu")
	}
}

func TestDecodeToUTF8PassthroughUTF8(t *testing.T) {
	in := "plain utf-8 — no transcoding needed"
	if got := string(decodeToUTF8([]byte(in), "text/plain; charset=utf-8")); got != in {
		t.Errorf("utf-8 passthrough altered the body: %q", got)
	}
}

// TestRenderShiftJISMetaCharset renders an HTML page whose encoding is declared
// only via <meta charset>, the case where the Content-Type header is no help.
func TestRenderShiftJISMetaCharset(t *testing.T) {
	const text = "こんにちは、世界。日本語のページです。"
	htmlSrc := `<html><head><meta charset="shift_jis"><title>テスト</title></head><body><article>` +
		`<p>` + text + `</p><p>` + text + `</p><p>` + text + `</p></article></body></html>`
	body, err := japanese.ShiftJIS.NewEncoder().Bytes([]byte(htmlSrc))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://example.jp/page")
	p := testClient().render(u, "text/html", body)
	if !strings.Contains(p.Markdown, "こんにちは、世界") {
		t.Errorf("rendered Markdown lost the Shift_JIS text:\n%s", p.Markdown)
	}
}

// TestRenderWindows1252Header covers the header-declared charset path through
// the full HTML render pipeline.
func TestRenderWindows1252Header(t *testing.T) {
	htmlSrc := `<html><body><article><p>Smörgåsbord — naïve façade, ` +
		`with enough filler text for readability to keep the paragraph intact.</p></article></body></html>`
	body, err := charmap.Windows1252.NewEncoder().Bytes([]byte(htmlSrc))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://example.com/1252")
	p := testClient().render(u, "text/html; charset=windows-1252", body)
	if !strings.Contains(p.Markdown, "Smörgåsbord") || !strings.Contains(p.Markdown, "naïve façade") {
		t.Errorf("rendered Markdown is mojibake:\n%s", p.Markdown)
	}
}
