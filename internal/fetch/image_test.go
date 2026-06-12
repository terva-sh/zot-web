package fetch

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/terva-sh/zot-web/internal/config"
)

// patternImage builds a w×h image whose pixels vary per coordinate, so it does
// not compress down to a handful of bytes (useful for size-cap tests).
func patternImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 13), B: uint8((x + y) * 5), A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, patternImage(w, h)); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func pngConfigOnly(w, h uint32) []byte {
	var out bytes.Buffer
	out.Write([]byte("\x89PNG\r\n\x1a\n"))
	writePNGChunk := func(kind string, data []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(kind)
		out.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(kind))
		crc.Write(data)
		binary.Write(&out, binary.BigEndian, crc.Sum32())
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // truecolor
	writePNGChunk("IHDR", ihdr)
	writePNGChunk("IEND", nil)
	return out.Bytes()
}

// imageServer serves body with the given content-type over a loopback test
// server, returning a Client that allowlists loopback so the SSRF gate permits
// it, plus the URL.
func imageServer(t *testing.T, contentType string, body []byte, maxBytes int64) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{FetchMaxBytes: 1 << 20, FetchImageMaxBytes: maxBytes, FetchTimeoutSec: 5}
	return New(cfg, ParseAllowList([]string{"127.0.0.1"})), srv.URL
}

func TestFetchImagePassthrough(t *testing.T) {
	png := encodePNG(t, 120, 60)
	c, url := imageServer(t, "image/png", png, 5<<20)

	got, err := c.FetchImage(context.Background(), url, 0, "")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if got.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", got.MimeType)
	}
	if got.Width != 120 || got.Height != 60 {
		t.Errorf("dims = %dx%d, want 120x60", got.Width, got.Height)
	}
	if got.Resized {
		t.Error("Resized = true, want false (no max_dimension given)")
	}
	if !bytes.Equal(got.Data, png) {
		t.Error("passthrough data should equal the served bytes")
	}
}

func TestFetchImageResizes(t *testing.T) {
	c, url := imageServer(t, "image/png", encodePNG(t, 300, 100), 5<<20)

	got, err := c.FetchImage(context.Background(), url, 150, "")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if !got.Resized {
		t.Fatal("Resized = false, want true")
	}
	if got.Width != 150 || got.Height != 50 {
		t.Errorf("resized dims = %dx%d, want 150x50", got.Width, got.Height)
	}
	if got.OrigW != 300 || got.OrigH != 100 {
		t.Errorf("orig dims = %dx%d, want 300x100", got.OrigW, got.OrigH)
	}
	// Output must be a decodable image of the reported size.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("resized output not decodable: %v", err)
	}
	if cfg.Width != 150 || cfg.Height != 50 {
		t.Errorf("decoded resized dims = %dx%d, want 150x50", cfg.Width, cfg.Height)
	}
}

func TestFetchImageDoesNotUpscale(t *testing.T) {
	c, url := imageServer(t, "image/png", encodePNG(t, 40, 20), 5<<20)

	got, err := c.FetchImage(context.Background(), url, 1000, "")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if got.Resized || got.Width != 40 || got.Height != 20 {
		t.Errorf("max_dimension above native size should not resize; got %dx%d resized=%v", got.Width, got.Height, got.Resized)
	}
}

func TestFetchImageTooLarge(t *testing.T) {
	c, url := imageServer(t, "image/png", encodePNG(t, 400, 400), 1000) // 1000-byte cap

	_, err := c.FetchImage(context.Background(), url, 0, "")
	var tooBig *ImageTooLargeError
	if !errors.As(err, &tooBig) {
		t.Fatalf("expected *ImageTooLargeError, got %v", err)
	}
	if tooBig.SuggestDim <= 0 || tooBig.SuggestDim >= 400 {
		t.Errorf("SuggestDim = %d, want a shrink in (0,400)", tooBig.SuggestDim)
	}
}

func TestFetchImageRejectsNonImage(t *testing.T) {
	c, url := imageServer(t, "text/plain", []byte("just some text, not an image at all"), 5<<20)

	_, err := c.FetchImage(context.Background(), url, 0, "")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("non-image")) {
		t.Fatalf("expected non-image rejection, got %v", err)
	}
}

func TestFetchImageSniffsWhenContentTypeMissing(t *testing.T) {
	// No Content-Type header: canonicalImageMIME must sniff the PNG magic.
	c, url := imageServer(t, "", encodePNG(t, 32, 32), 5<<20)

	got, err := c.FetchImage(context.Background(), url, 0, "")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if got.MimeType != "image/png" {
		t.Errorf("sniffed mime = %q, want image/png", got.MimeType)
	}
}

func TestFetchImageRejectsHugePixelDimensions(t *testing.T) {
	// A tiny PNG header can claim dimensions that would allocate enormous pixel
	// buffers if fully decoded. FetchImage must reject it after DecodeConfig.
	c, url := imageServer(t, "image/png", pngConfigOnly(100_000, 100_000), 5<<20)

	_, err := c.FetchImage(context.Background(), url, 1024, "")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("safe decode limit")) {
		t.Fatalf("expected safe decode limit rejection, got %v", err)
	}
}

func TestValidateImageDimensions(t *testing.T) {
	if err := validateImageDimensions(1, 1); err != nil {
		t.Fatalf("1x1 should be valid: %v", err)
	}
	if err := validateImageDimensions(0, 10); err == nil {
		t.Fatal("zero width should be rejected")
	}
	if err := validateImageDimensions(100_000, 100_000); err == nil {
		t.Fatal("huge dimensions should be rejected")
	}
}

func TestResizeImageJPEGStaysJPEG(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, patternImage(200, 200), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	out, w, h, mime, err := resizeImage(buf.Bytes(), "image/jpeg", 100)
	if err != nil {
		t.Fatalf("resizeImage: %v", err)
	}
	if mime != "image/jpeg" || w != 100 || h != 100 {
		t.Errorf("got %s %dx%d, want image/jpeg 100x100", mime, w, h)
	}
	if _, _, derr := image.DecodeConfig(bytes.NewReader(out)); derr != nil {
		t.Errorf("resized jpeg not decodable: %v", derr)
	}
}

func TestScaledDims(t *testing.T) {
	cases := []struct{ w, h, max, wantW, wantH int }{
		{300, 100, 150, 150, 50},
		{100, 300, 150, 50, 150},
		{200, 200, 100, 100, 100},
		{80, 40, 1000, 80, 40}, // never upscales
		{0, 0, 100, 0, 0},      // degenerate
	}
	for _, c := range cases {
		gw, gh := scaledDims(c.w, c.h, c.max)
		if gw != c.wantW || gh != c.wantH {
			t.Errorf("scaledDims(%d,%d,%d) = %d,%d, want %d,%d", c.w, c.h, c.max, gw, gh, c.wantW, c.wantH)
		}
	}
}

func TestCanonicalImageMIME(t *testing.T) {
	png := encodePNG(t, 8, 8)
	if got := canonicalImageMIME("image/png; charset=binary", png); got != "image/png" {
		t.Errorf("declared png = %q, want image/png", got)
	}
	if got := canonicalImageMIME("application/octet-stream", png); got != "image/png" {
		t.Errorf("sniffed png = %q, want image/png", got)
	}
	if got := canonicalImageMIME("text/html", []byte("<html></html>")); got != "" {
		t.Errorf("non-image = %q, want empty", got)
	}
}
