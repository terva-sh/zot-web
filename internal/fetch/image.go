package fetch

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the WebP decoder for image.Decode
)

// ImageResult is a fetched (and optionally resized) image ready for multimodal
// injection or writing to disk.
type ImageResult struct {
	Data     []byte // encoded image bytes (PNG/JPEG/GIF; WebP is transcoded to PNG on resize)
	MimeType string
	Width    int
	Height   int
	FinalURL string // after redirects
	Resized  bool
	OrigW    int // dimensions of the source image before any resize
	OrigH    int
}

// ImageTooLargeError reports that an image exceeds the injection byte cap even
// after any requested resize. It carries a suggested max_dimension so the model
// can resubmit web_fetch_image and converge under the cap.
type ImageTooLargeError struct {
	Bytes      int
	MaxBytes   int64
	Width      int
	Height     int
	SuggestDim int
}

func (e *ImageTooLargeError) Error() string {
	return fmt.Sprintf("image is %s (%dx%d), over the %s cap for model injection; "+
		"resubmit web_fetch_image with max_dimension=%d to downsample it",
		humanBytes(int64(e.Bytes)), e.Width, e.Height, humanBytes(e.MaxBytes), e.SuggestDim)
}

// maxImagePixels caps decoded image area before any full image.Decode call.
// It protects the extension from compressed images that are small on the wire
// but expand into very large pixel buffers during resize/validation. At ~4 bytes
// per RGBA pixel a 40M-pixel image is already a ~160 MiB decode buffer, so the
// cap is deliberately well below what a 25 MiB download could otherwise unpack
// to. It still comfortably admits 4K/8K-class photography.
const maxImagePixels int64 = 40_000_000

// resizeSem bounds how many image decode/resize operations run concurrently.
// Each one allocates pixel buffers up to maxImagePixels*4 bytes for the source
// plus the scaled destination, and tool calls each run in their own goroutine
// (see proto.Run) behind a burst-10 rate limiter — so without this gate a burst
// of large images could spike to multiple GiB and OOM the extension.
var resizeSem = make(chan struct{}, 3)

// FetchImage retrieves an image (http/https only, SSRF-guarded) and returns it
// ready for multimodal injection. maxDimension (longest edge, px) downsamples
// the image when set; 0 leaves it at native size. An image still over the
// configured byte cap after resizing yields *ImageTooLargeError. A non-empty
// userAgent overrides the configured UA ("browser" expands to a browser UA).
func (c *Client) FetchImage(ctx context.Context, raw string, maxDimension int, userAgent string) (ImageResult, error) {
	u, err := parseURL(raw)
	if err != nil {
		return ImageResult{}, err
	}

	ceiling := c.imageDownloadCeiling()
	f, err := c.download(ctx, u, ceiling, userAgent)
	if err != nil {
		return ImageResult{}, err
	}
	if f.truncated {
		return ImageResult{}, fmt.Errorf("image exceeds the %s download ceiling and was not fully retrieved", humanBytes(ceiling))
	}

	mime := canonicalImageMIME(f.contentType, f.body)
	if mime == "" {
		return ImageResult{}, fmt.Errorf("unsupported or non-image content (%s); web_fetch_image handles PNG, JPEG, GIF, and WebP", displayType(f.contentType))
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(f.body))
	if err != nil {
		return ImageResult{}, fmt.Errorf("could not decode image: %w", err)
	}
	if err := validateImageDimensions(cfg.Width, cfg.Height); err != nil {
		return ImageResult{}, err
	}

	res := ImageResult{
		Data: f.body, MimeType: mime,
		Width: cfg.Width, Height: cfg.Height,
		FinalURL: f.finalURL,
		OrigW:    cfg.Width, OrigH: cfg.Height,
	}

	if maxDimension > 0 && longestEdge(cfg.Width, cfg.Height) > maxDimension {
		out, ow, oh, omime, rerr := resizeImage(f.body, mime, maxDimension)
		if rerr != nil {
			return ImageResult{}, fmt.Errorf("resize failed: %w", rerr)
		}
		res.Data, res.MimeType, res.Width, res.Height, res.Resized = out, omime, ow, oh, true
	}

	if int64(len(res.Data)) > c.imageMaxBytes {
		return ImageResult{}, &ImageTooLargeError{
			Bytes:      len(res.Data),
			MaxBytes:   c.imageMaxBytes,
			Width:      res.Width,
			Height:     res.Height,
			SuggestDim: suggestDimension(res.Width, res.Height, len(res.Data), c.imageMaxBytes),
		}
	}
	return res, nil
}

// imageDownloadCeiling is the hard cap on bytes pulled off the network for an
// image. It is deliberately larger than the injection cap so an oversized
// original can be decoded and resized down to fit.
func (c *Client) imageDownloadCeiling() int64 {
	const floor = 25 << 20 // 25 MiB
	if ceil := c.imageMaxBytes * 4; ceil > floor {
		return ceil
	}
	return floor
}

// canonicalImageMIME returns the normalized media type for a supported image,
// trusting a declared image content-type and otherwise sniffing the bytes.
// Returns "" for anything web_fetch_image does not handle.
func canonicalImageMIME(contentType string, body []byte) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if isSupportedImageMIME(ct) {
		return ct
	}
	// Header missing or wrong (e.g. octet-stream): fall back to content sniffing.
	if sniff := http.DetectContentType(body); isSupportedImageMIME(sniff) {
		return sniff
	}
	return ""
}

func isSupportedImageMIME(ct string) bool {
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func validateImageDimensions(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid image dimensions %dx%d", w, h)
	}
	pixels := int64(w) * int64(h)
	if pixels > maxImagePixels {
		return fmt.Errorf("image dimensions %dx%d exceed the safe decode limit of %d pixels", w, h, maxImagePixels)
	}
	return nil
}

// resizeImage decodes data, downscales it so its longest edge is maxDimension
// (preserving aspect, never enlarging), and re-encodes. PNG/JPEG/GIF keep their
// format; WebP transcodes to PNG (Go has no WebP encoder).
func resizeImage(data []byte, mime string, maxDimension int) (out []byte, w, h int, outMime string, err error) {
	// Bound concurrent decode+scale memory across simultaneous tool calls.
	resizeSem <- struct{}{}
	defer func() { <-resizeSem }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, "", err
	}
	b := src.Bounds()
	w, h = scaledDims(b.Dx(), b.Dy(), maxDimension)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	outMime = mime
	var buf bytes.Buffer
	switch mime {
	case "image/jpeg":
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	case "image/gif":
		err = gif.Encode(&buf, dst, nil)
	default: // image/png and image/webp → PNG
		outMime = "image/png"
		err = png.Encode(&buf, dst)
	}
	if err != nil {
		return nil, 0, 0, "", err
	}
	return buf.Bytes(), w, h, outMime, nil
}

func longestEdge(w, h int) int {
	if w > h {
		return w
	}
	return h
}

// scaledDims returns w,h scaled so the longest edge is maxDim, preserving the
// aspect ratio. It never enlarges and never returns a zero dimension.
func scaledDims(w, h, maxDim int) (int, int) {
	le := longestEdge(w, h)
	if le <= maxDim || le == 0 {
		return w, h
	}
	scale := float64(maxDim) / float64(le)
	nw := int(math.Round(float64(w) * scale))
	nh := int(math.Round(float64(h) * scale))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

// suggestDimension estimates a max_dimension likely to bring the encoded image
// under cap. Encoded size grows roughly with pixel area, so scale the longest
// edge by sqrt(cap/bytes) with a safety margin, and guarantee an actual shrink.
func suggestDimension(w, h, encodedBytes int, cap int64) int {
	le := longestEdge(w, h)
	if le == 0 || encodedBytes == 0 {
		return le
	}
	ratio := math.Sqrt(float64(cap)/float64(encodedBytes)) * 0.9
	d := int(float64(le) * ratio)
	if d >= le {
		d = le * 3 / 4 // ensure the suggestion is smaller than the current size
	}
	if d < 64 {
		d = 64
	}
	return d
}

// humanBytes renders a byte count as a compact human-readable size.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
