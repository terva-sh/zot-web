// Package fetch implements web_fetch: an SSRF-guarded HTTP client plus
// main-content extraction. Because the model chooses the URL, fetching is the
// extension's main attack surface — every connection is validated against the
// private-range block (with the configurable local allowlist) in a custom
// DialContext that dials the validated IP and re-runs on each redirect hop.
package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/terva-sh/zot-web/internal/config"
	"github.com/terva-sh/zot-web/internal/version"
)

// defaultUserAgent identifies the extension honestly (the robots/etiquette
// default). The user_agent config setting or a per-call user_agent parameter
// overrides it; "browser" expands to browserUserAgent.
var defaultUserAgent = "zot-web/" + version.Version

// browserUserAgent is what the "browser" alias expands to: a common desktop
// Chrome UA, for sites that refuse or degrade content for non-browser clients.
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

// resolveUserAgent expands the "browser" alias and maps empty to fallback.
func resolveUserAgent(ua, fallback string) string {
	ua = strings.TrimSpace(ua)
	switch {
	case ua == "":
		return fallback
	case strings.EqualFold(ua, "browser"):
		return browserUserAgent
	}
	return ua
}

// Client is a reusable SSRF-guarded fetcher. Rendered pages are cached so a
// web_images call following a web_fetch needs no network.
type Client struct {
	http          *http.Client
	maxBytes      int64
	imageMaxBytes int64
	allow         AllowList
	inlineImages  bool
	cache         *cache
	userAgent     string // configured default UA (already alias-resolved)
}

// New builds a Client whose dialer refuses private/reserved destinations unless
// the allowlist permits them.
func New(cfg config.Config, allow AllowList) *Client {
	c := &Client{
		maxBytes:      cfg.FetchMaxBytes,
		imageMaxBytes: cfg.FetchImageMaxBytes,
		allow:         allow,
		inlineImages:  cfg.FetchInlineImages,
		cache:         newCache(time.Duration(cfg.FetchCacheTTLSec)*time.Second, cfg.FetchCacheMaxEntries, cfg.FetchCacheMaxBytes),
		userAgent:     resolveUserAgent(cfg.UserAgent, defaultUserAgent),
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	tr := &http.Transport{
		// Validate at dial time: resolve the host ourselves, pick the first
		// permitted IP, and connect to THAT ip — closing the DNS-rebinding /
		// TOCTOU gap. The transport re-invokes this for every redirect host.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ipa := range ips {
				if c.allow.permitted(host, ipa.IP) {
					return dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
				}
			}
			return nil, &SSRFBlockedError{Host: host}
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          10,
	}
	c.http = &http.Client{
		Transport: tr,
		Timeout:   time.Duration(cfg.FetchTimeoutSec) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}
	return c
}

// Fetch retrieves raw (http/https only) and returns its main content as
// Markdown, prefixed with a metadata block. maxChars caps the returned window
// (default 20000); offset skips that many characters into the rendered page so
// callers can page through dense documents. The page is rendered once and
// cached; image URLs are replaced with `[image:N]` placeholders (unless inline
// images are configured) and retrievable via Images. A non-empty userAgent
// overrides the configured UA for this request ("browser" expands to a common
// browser UA) and bypasses the cache read so the page is actually re-fetched.
func (c *Client) Fetch(ctx context.Context, raw string, maxChars, offset int, userAgent string) (string, error) {
	u, err := parseURL(raw)
	if err != nil {
		return "", err
	}
	p, err := c.load(ctx, u, userAgent)
	if err != nil {
		return "", err
	}

	if maxChars <= 0 {
		maxChars = 20000
	}
	full := []rune(p.Markdown)
	total := len(full)
	start := min(max(offset, 0), total)
	end := min(start+maxChars, total)
	window := string(full[start:end])

	var b strings.Builder
	// Header: article title (when readability found one) above the source URL.
	if p.Title != "" {
		fmt.Fprintf(&b, "# %s\n%s\n", p.Title, u.String())
	} else {
		fmt.Fprintf(&b, "# %s\n", u.String())
	}
	if p.FinalURL != "" && p.FinalURL != u.String() {
		fmt.Fprintf(&b, "Final-URL: %s\n", p.FinalURL)
	}
	if p.ContentType != "" {
		fmt.Fprintf(&b, "Content-Type: %s\n", p.ContentType)
	}
	fmt.Fprintf(&b, "Chars: %d-%d of %d\n", start, end, total)
	if p.MarkdownCapped {
		fmt.Fprintf(&b, "Note: the render hit the %d-rune output cap, so the page's tail is missing and not reachable via offset; use web_fetch_raw for the complete source\n", maxRenderedRunes)
	}
	if !c.inlineImages && len(p.Images) > 0 {
		if p.ImagesInline {
			fmt.Fprintf(&b, "Images: %d (shown as [image:N]; resolve with web_images)\n", len(p.Images))
		} else {
			// Heuristic/fallback render: no inline placeholders, but the URLs
			// were still harvested from the page.
			fmt.Fprintf(&b, "Images: %d (not inlined; list URLs with web_images)\n", len(p.Images))
		}
	}
	b.WriteString("\n")
	b.WriteString(window)

	if end < total {
		fmt.Fprintf(&b, "\n\n…[%d more chars; continue with offset=%d]", total-end, end)
	} else if p.BodyTruncated {
		b.WriteString("\n\n…[the source response was capped at the byte limit before rendering]")
	}
	return b.String(), nil
}

// Images returns the images found on raw (resolved to absolute URLs). It serves
// a cached render when available, fetching only on a cold cache.
func (c *Client) Images(ctx context.Context, raw string) ([]Image, error) {
	u, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	p, err := c.load(ctx, u, "")
	if err != nil {
		return nil, err
	}
	return p.Images, nil
}

// Links returns every link found on raw (resolved to absolute URLs), served
// from cache when the page was recently fetched and otherwise by fetching it.
func (c *Client) Links(ctx context.Context, raw string) ([]Link, error) {
	u, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	p, err := c.load(ctx, u, "")
	if err != nil {
		return nil, err
	}
	return p.Links, nil
}

// CacheList snapshots the page cache for inspection (newest first).
func (c *Client) CacheList() []CacheEntryInfo { return c.cache.list() }

// CacheClear empties the page cache, returning the number of entries dropped.
func (c *Client) CacheClear() int { return c.cache.clear() }

// RawDoc is the unrendered response body for a fetched page.
type RawDoc struct {
	Body        []byte
	ContentType string
	FinalURL    string
	Truncated   bool // body hit the byte cap before it was saved
}

// Raw returns the unrendered response body for raw, decompressed from the same
// cache that backs web_fetch (fetching only on a cold cache). It's the basis
// for web_fetch_raw: the exact bytes the server sent, for the model to grep.
// A non-empty userAgent overrides the configured UA and bypasses the cache read.
func (c *Client) Raw(ctx context.Context, raw, userAgent string) (RawDoc, error) {
	u, err := parseURL(raw)
	if err != nil {
		return RawDoc{}, err
	}
	p, err := c.load(ctx, u, userAgent)
	if err != nil {
		return RawDoc{}, err
	}
	body, err := gunzipBytes(p.RawGzip)
	if err != nil {
		return RawDoc{}, fmt.Errorf("decompressing cached page: %w", err)
	}
	return RawDoc{Body: body, ContentType: p.ContentType, FinalURL: p.FinalURL, Truncated: p.BodyTruncated}, nil
}

// parseURL validates and normalizes a model-supplied URL.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (only http/https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	if p := u.Port(); p != "" && blockedPorts[p] {
		return nil, fmt.Errorf("port %s is not permitted (it is a well-known non-web service port)", p)
	}
	return u, nil
}

// blockedPorts are well-known non-HTTP service ports. Internal addresses are
// already refused by the SSRF guard, so this only adds defense-in-depth against
// using the fetcher to poke these services on *public* hosts; web content never
// lives here, so blocking them costs no legitimate fetch.
var blockedPorts = map[string]bool{
	"22":    true, // SSH
	"23":    true, // Telnet
	"25":    true, // SMTP
	"110":   true, // POP3
	"143":   true, // IMAP
	"445":   true, // SMB
	"465":   true, // SMTPS
	"587":   true, // SMTP submission
	"993":   true, // IMAPS
	"995":   true, // POP3S
	"1433":  true, // MSSQL
	"3306":  true, // MySQL
	"3389":  true, // RDP
	"5432":  true, // PostgreSQL
	"5900":  true, // VNC
	"6379":  true, // Redis
	"11211": true, // memcached
	"27017": true, // MongoDB
}

// load returns the rendered page for u, from cache when fresh, otherwise by
// fetching and rendering (and caching the result). A non-empty userAgent skips
// the cache read (the caller asked for a fresh fetch as that UA); the result
// still replaces the cached entry so follow-up web_images/web_links calls see
// the same snapshot.
func (c *Client) load(ctx context.Context, u *url.URL, userAgent string) (page, error) {
	// Normalize the cache key: strip common tracking/utm params so cache-buster
	// variants of the same page don't evict each other.
	key := cacheKey(u.String())
	if userAgent == "" {
		if p, ok := c.cache.get(key); ok {
			return p, nil
		}
	}
	f, err := c.download(ctx, u, c.maxBytes, userAgent)
	if err != nil {
		return page{}, err
	}
	// Resolve relative links/images against the post-redirect URL so an
	// http→https (or path) redirect doesn't leave stale links in the body.
	base := u
	if fu, perr := url.Parse(f.finalURL); perr == nil && fu.Host != "" {
		base = fu
	}
	p := c.render(base, f.contentType, f.body)
	p.URL = key
	p.FinalURL = f.finalURL
	p.ContentType = f.contentType
	p.Status = f.status
	p.BodyTruncated = f.truncated
	// Retain the unrendered body for web_fetch_raw, gzipped so a warm cache of
	// HTML pages stays cheap (HTML compresses ~5-10x).
	p.RawGzip = gzipBytes(f.body)
	c.cache.put(p)
	return p, nil
}

// gzipBytes returns b gzip-compressed. A nil/empty input yields nil. It runs on
// every fetch (to keep warm-cache entries small) but the raw body is consumed
// only by the comparatively rare web_fetch_raw, so it compresses at BestSpeed —
// most of the size win on HTML for a fraction of the CPU of the default level.
func gzipBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil
	}
	if _, err := w.Write(b); err != nil {
		w.Close()
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// gunzipBytes inflates bytes produced by gzipBytes.
func gunzipBytes(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// fetched is the raw result of an SSRF-guarded GET.
type fetched struct {
	body        []byte
	truncated   bool // body hit the byte cap
	contentType string
	finalURL    string // after redirects
	status      int
}

// download performs the SSRF-guarded GET and returns the body, capped at
// maxBytes (truncated set when the body hit the cap).
// HTTPClient exposes the SSRF-guarded client so callers (e.g. search backends)
// can reuse the same transport.
func (c *Client) HTTPClient() *http.Client { return c.http }

func (c *Client) download(ctx context.Context, u *url.URL, maxBytes int64, userAgent string) (fetched, error) {
	f, err := c.downloadOnce(ctx, u, maxBytes, userAgent)
	if err != nil && retryableFetchError(err) && ctx.Err() == nil {
		// One short-backoff retry absorbs most transient flake (a 502/503 from
		// a busy origin, a dropped connection) without meaningfully delaying
		// the hard-failure path.
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return fetched{}, classifyFetchError(ctx.Err())
		}
		return c.downloadOnce(ctx, u, maxBytes, userAgent)
	}
	return f, err
}

// fetchSem bounds concurrent downloads. Every tool call runs in its own
// goroutine (see proto.Run) and each in-flight download can buffer up to its
// byte cap (2 MiB pages, 25+ MiB image ceilings), so without a gate a burst of
// parallel calls could hold tens of MiB of bodies at once. Four is plenty for
// an agent's realistic call pattern; excess callers queue here briefly.
var fetchSem = make(chan struct{}, 4)

func (c *Client) downloadOnce(ctx context.Context, u *url.URL, maxBytes int64, userAgent string) (fetched, error) {
	select {
	case fetchSem <- struct{}{}:
		defer func() { <-fetchSem }()
	case <-ctx.Done():
		return fetched{}, classifyFetchError(ctx.Err())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fetched{}, err
	}
	req.Header.Set("User-Agent", resolveUserAgent(userAgent, c.userAgent))
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return fetched{}, classifyFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fetched{}, &HTTPStatusError{Status: resp.StatusCode, URL: u.String()}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fetched{}, classifyFetchError(err)
	}
	f := fetched{
		body:        body,
		contentType: resp.Header.Get("Content-Type"),
		finalURL:    resp.Request.URL.String(),
		status:      resp.StatusCode,
	}
	if int64(len(body)) > maxBytes {
		f.body = body[:maxBytes]
		f.truncated = true
	}
	return f, nil
}

// HTTPStatusError is a ≥400 response, rendered with per-class guidance so the
// model knows whether to fix the URL, change identity, back off, or give up.
type HTTPStatusError struct {
	Status int
	URL    string
}

func (e *HTTPStatusError) Error() string {
	hint := "client error"
	switch {
	case e.Status == 401 || e.Status == 403:
		hint = `access denied — the site may be blocking automated clients; retry with user_agent: "browser", or try another source`
	case e.Status == 404 || e.Status == 410:
		hint = "page not found — check the URL; the page may have moved or been removed"
	case e.Status == 429:
		hint = "rate limited by the site — wait before retrying this host"
	case e.Status >= 500:
		hint = "server error — usually transient; retrying later may succeed"
	}
	return fmt.Sprintf("http %d fetching %s (%s)", e.Status, e.URL, hint)
}

// retryableFetchError reports whether one immediate retry is worth it: a
// 502/503/504 from a flaky origin or a dropped connection. Timeouts are not
// retried (the overall deadline is already mostly spent) and neither are
// other 4xx/5xx (they would just repeat).
func retryableFetchError(err error) bool {
	var hs *HTTPStatusError
	if errors.As(err, &hs) {
		return hs.Status == http.StatusBadGateway || hs.Status == http.StatusServiceUnavailable || hs.Status == http.StatusGatewayTimeout
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") || strings.Contains(s, "unexpected EOF")
}

// classifyFetchError maps a transport error to a stable, recognizable prefix so
// agents can react to failure classes consistently. SSRFBlockedError passes
// through — its Error() message is model-safe.
func classifyFetchError(err error) error {
	if err == nil {
		return nil
	}
	var ssrf *SSRFBlockedError
	if errors.As(err, &ssrf) {
		return err
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "stopped after") && strings.Contains(s, "redirects"):
		return fmt.Errorf("redirect loop: %w", err)
	case strings.Contains(s, "no such host"), strings.Contains(s, "server misbehaving"),
		strings.Contains(s, "name resolution"):
		return fmt.Errorf("dns error: %w", err)
	case errors.Is(err, context.DeadlineExceeded),
		strings.Contains(s, "Client.Timeout"), strings.Contains(s, "deadline exceeded"),
		strings.Contains(s, "timeout"):
		return fmt.Errorf("timeout: %w", err)
	case strings.Contains(s, "connection refused"):
		return fmt.Errorf("connection refused: %w", err)
	}
	return err
}

// isTextual reports whether a body should be treated as readable text rather
// than binary (which web_fetch summarizes instead of dumping). It trusts a
// textual content-type, and otherwise sniffs for NUL bytes.
func isTextual(contentType string, body []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case ct == "":
		return !looksBinary(body)
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/json", ct == "application/xml", ct == "application/xhtml+xml",
		ct == "application/javascript", ct == "application/ecmascript", ct == "image/svg+xml",
		strings.HasSuffix(ct, "+json"), strings.HasSuffix(ct, "+xml"):
		return true
	}
	return false
}

// looksBinary reports whether the first kilobyte contains a NUL byte.
func looksBinary(body []byte) bool {
	if len(body) > 1024 {
		body = body[:1024]
	}
	return bytes.IndexByte(body, 0) >= 0
}

// displayType is the content-type without parameters, for a human-readable note.
func displayType(contentType string) string {
	ct := strings.TrimSpace(contentType)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		return "binary"
	}
	return ct
}

// render turns a response body into a page: readability isolates the main
// article, image URLs are indexed out to `[image:N]` placeholders, and
// html-to-markdown (with GFM tables) renders the result. On any failure it
// falls back to the heuristic tag-stripper. Non-HTML bodies pass through.
func (c *Client) render(u *url.URL, contentType string, body []byte) page {
	ct := strings.ToLower(contentType)
	isHTML := strings.Contains(ct, "html") ||
		(ct == "" && strings.Contains(strings.ToLower(string(body)), "<html"))
	if !isHTML {
		if !isTextual(contentType, body) {
			if isPDF(contentType, body) {
				if fp, ok := renderPDF(body); ok {
					return fp
				}
				return page{Markdown: fmt.Sprintf("[application/pdf content, %d bytes — no extractable text layer (encrypted, malformed, or scanned images); use web_fetch_raw to save the file]", len(body))}
			}
			return page{Markdown: fmt.Sprintf("[%s content, %d bytes — not rendered as text]", displayType(contentType), len(body))}
		}
		// RSS/Atom feeds get a structured per-entry render instead of being
		// dumped as raw XML text.
		if fp, ok := renderFeed(contentType, body); ok {
			return fp
		}
		return cappedPage(page{}, strings.TrimSpace(string(decodeToUTF8(body, contentType))))
	}
	// Decode legacy charsets (windows-1252, Shift_JIS, GBK, …) to UTF-8 before
	// any parsing: x/net/html and readability both assume UTF-8 input, so
	// without this non-UTF-8 pages render as mojibake. The raw cache entry
	// (web_fetch_raw) keeps the undecoded bytes as served.
	body = decodeToUTF8(body, contentType)

	// Parse the full document once for whole-page harvesting: links and many
	// images (nav thumbnails, og:image, lazy-loaded galleries) live outside the
	// readability article subtree, so we collect them from the full tree.
	var fullDoc *xhtml.Node
	if doc, perr := xhtml.Parse(bytes.NewReader(body)); perr == nil {
		fullDoc = doc
	}
	links := collectLinksOpt(fullDoc, u)

	art, err := readability.FromReader(bytes.NewReader(body), u)
	if err == nil && art.Node != nil {
		node := art.Node
		var images []Image
		inline := false
		if !c.inlineImages {
			images = indexImages(node, u)
			inline = len(images) > 0
			// Article had no images of its own (common on boards/forums/SPAs):
			// fall back to a whole-document harvest so web_images isn't empty.
			if len(images) == 0 && fullDoc != nil {
				images = collectImages(fullDoc, u)
			}
		}
		if md, err := convertNode(node); err == nil {
			if md = applyPlaceholders(strings.TrimSpace(md), images); md != "" {
				// readability strips <table> elements; recover the data
				// tables it dropped, unless the render already has one.
				if !hasMarkdownTable(md) {
					md += extractDataTables(body)
				}
				return cappedPage(page{Title: strings.TrimSpace(art.Title()), Images: images, ImagesInline: inline, Links: links}, md)
			}
		}
		// Readability found content but markdown conversion produced nothing;
		// use its plain-text rendering rather than dropping to the heuristic.
		var buf bytes.Buffer
		if art.RenderText(&buf) == nil {
			if t := strings.TrimSpace(buf.String()); t != "" {
				return cappedPage(page{Title: strings.TrimSpace(art.Title()), Images: images, Links: links}, t)
			}
		}
	}
	// Heuristic path: the tag-stripper drops all markup, so images can't be
	// placeholdered inline — but we still index them from the full DOM so
	// web_images works on pages readability can't parse.
	var images []Image
	if !c.inlineImages && fullDoc != nil {
		images = collectImages(fullDoc, u)
	}
	return cappedPage(page{Images: images, Links: links}, heuristicExtract(body))
}

// decodeToUTF8 converts body to UTF-8, determining the source encoding from
// the Content-Type charset parameter, a <meta charset> declaration, or content
// sniffing (in that order). UTF-8 input passes through cheaply; on any error
// the original bytes are returned unchanged (best effort).
func decodeToUTF8(body []byte, contentType string) []byte {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return body
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return body
	}
	return out
}

// collectLinksOpt is collectLinks guarded against a nil (unparseable) document.
func collectLinksOpt(root *xhtml.Node, base *url.URL) []Link {
	if root == nil {
		return nil
	}
	return collectLinks(root, base)
}

// convertNode renders an HTML node to Markdown with CommonMark + GFM tables.
// Tables use mirror span cells so rowspan/colspan headers (e.g. infoboxes)
// repeat their value into spanned cells rather than leaving blanks.
func convertNode(n *xhtml.Node) (string, error) {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(
			table.WithSpanCellBehavior(table.SpanBehaviorMirror),
			table.WithPresentationTables(false),
		),
	))
	b, err := conv.ConvertNode(n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// heuristicExtract is the fallback tag-stripper for bodies readability can't
// parse: it removes script/style, drops remaining tags, and unescapes entities.
var (
	reScriptStyle = regexp.MustCompile(`(?is)<(?:script|style|noscript|template)\b[^>]*>.*?</(?:script|style|noscript|template)\s*>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	reInlineWS    = regexp.MustCompile(`[ \t]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

func heuristicExtract(body []byte) string {
	s := string(body)
	s = reScriptStyle.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reInlineWS.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// maxRenderedRunes caps the Markdown a rendered page can produce, preventing
// a small HTML payload from expanding to enormous Markdown (e.g. deeply nested
// lists) that would blow up the cache and model context.
const maxRenderedRunes = 500_000

// capMarkdown truncates s at maxRenderedRunes runes, appending a note and
// reporting whether the cap fired (so the fetch header can surface it).
func capMarkdown(s string) (string, bool) {
	count := 0
	for i := range s {
		if count == maxRenderedRunes {
			return s[:i] + "\n\n…[Markdown output capped at " + fmt.Sprint(maxRenderedRunes) + " runes]", true
		}
		count++
	}
	return s, false
}

// cappedPage fills p.Markdown from md via capMarkdown, recording when the
// render cap fired.
func cappedPage(p page, md string) page {
	p.Markdown, p.MarkdownCapped = capMarkdown(md)
	return p
}

// cacheKey returns a normalized key for the page cache: the URL with common
// tracking parameters stripped. Keep ambiguous short/generic parameters intact
// because they can affect page content on some sites.
func cacheKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	dropped := false
	for _, p := range []string{
		"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content", "utm_id",
		"fbclid", "gclid", "dclid", "gbraid", "wbraid", "msclkid",
		"mc_cid", "mc_eid", "igshid",
	} {
		if q.Has(p) {
			q.Del(p)
			dropped = true
		}
	}
	if !dropped {
		return rawURL
	}
	u.RawQuery = q.Encode()
	return u.String()
}
