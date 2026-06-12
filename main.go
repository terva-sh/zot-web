// Command zot-web is a zot extension that gives the agent web tools:
//
//	web_search(query, count?)        -> ranked results (title, url, snippet)
//	web_fetch(url, max_chars?, ...)  -> the page's main content as Markdown
//	web_images(url)                  -> resolve a page's [image:N] placeholders
//	web_links(url)                   -> every link on a page (absolute URL + text)
//	web_fetch_image(url, ...)        -> an image for multimodal viewing / save to disk
//	web_fetch_raw(url, save_path)    -> the page's unrendered source, saved to a file
//
// Search is pluggable (Tavily default, SearXNG alternate). Fetching is
// SSRF-guarded with a configurable local-address allowlist. See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/terva-sh/zot-web/internal/config"
	"github.com/terva-sh/zot-web/internal/fetch"
	"github.com/terva-sh/zot-web/internal/proto"
	"github.com/terva-sh/zot-web/internal/search"
	"github.com/terva-sh/zot-web/internal/version"
)

const searchSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The search query."},
    "count": {"type": "integer", "description": "Number of results (default 5, max 10).", "minimum": 1, "maximum": 10},
    "freshness": {"type": "string", "enum": ["day", "week", "month", "year"], "description": "Only results published within this window. Use for current events and anything time-sensitive."},
    "include_domains": {"type": "array", "items": {"type": "string"}, "description": "Restrict results to these domains (e.g. [\"docs.python.org\"]). Subdomains match."},
    "exclude_domains": {"type": "array", "items": {"type": "string"}, "description": "Drop results from these domains."},
    "depth": {"type": "string", "enum": ["basic", "advanced"], "description": "\"advanced\" requests a deeper, higher-quality (slower) search where the backend supports it."}
  },
  "required": ["query"]
}`

const userAgentParam = `"user_agent": {"type": "string", "description": "Optional User-Agent override for this request: \"browser\" for a common desktop-browser UA (useful when a site blocks or degrades content for automated clients), or a literal UA string. Forces a fresh fetch (bypasses the cached snapshot)."}`

const fetchSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http(s) URL to fetch."},
    "max_chars": {"type": "integer", "description": "Max characters of extracted text to return (default 20000)."},
    "offset": {"type": "integer", "description": "Skip this many characters into the page, to continue reading after a previous truncated fetch (default 0).", "minimum": 0},
    ` + userAgentParam + `
  },
  "required": ["url"]
}`

const imagesSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "URL of a page already retrieved with web_fetch."}
  },
  "required": ["url"]
}`

const linksSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "URL of a page (ideally one already retrieved with web_fetch)."}
  },
  "required": ["url"]
}`

const webFetchRawSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http(s) URL to fetch."},
    "save_path": {"type": "string", "description": "Workspace-relative path to write the unrendered page source to (e.g. \"tmp/thread.html\"). Must stay within the workspace; parent directories are created as needed."},
    "overwrite": {"type": "boolean", "description": "Allow overwriting save_path if it already exists (default false)."},
    ` + userAgentParam + `
  },
  "required": ["url", "save_path"]
}`

const webFetchImageSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http(s) URL of an image (PNG, JPEG, GIF, or WebP)."},
    "max_dimension": {"type": "integer", "description": "If set, downscale so the image's longest edge is at most this many pixels (preserves aspect ratio, never upscales). Use this to bring an oversized image under the size limit.", "minimum": 1},
    "save_path": {"type": "string", "description": "Optional workspace-relative path to write the image to (e.g. \"assets/logo.png\"). Must stay within the workspace; parent directories are created as needed."},
    "overwrite": {"type": "boolean", "description": "Allow overwriting save_path if it already exists (default false)."},
    "inject": {"type": "boolean", "description": "Whether to return the image to you for viewing (default true). Set false to only download/save it without spending context on the pixels."},
    ` + userAgentParam + `
  },
  "required": ["url"]
}`

func main() {
	// The normal mode is the stdio extension protocol, which blocks silently
	// on stdin — so give a bare `zot-web --version` invocation a way to
	// identify the installed build instead of appearing to hang.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			fmt.Println(versionString())
			return
		}
	}

	e := proto.New("web", version.Version)

	// Providers are built lazily on first tool call, by which point the
	// hello_ack (and thus data_dir for config.json) has arrived.
	var (
		once     sync.Once
		provider search.Provider
		provErr  error
		fetcher  *fetch.Client
		rl       = newRateLimiter(10) // 10 burst, refilled per-tool at different rates
	)
	ensure := func() {
		once.Do(func() {
			cfg := config.Load(e.Host().DataDir)
			fetcher = fetch.New(cfg, fetch.ParseAllowList(cfg.AllowLocalHosts))
			provider, provErr = search.New(cfg, fetcher.HTTPClient())
		})
	}

	e.Tool("web_search",
		"Search the web and return ranked results (title, URL, snippet). Use for current events, facts, documentation, or to find pages to read with web_fetch.",
		json.RawMessage(searchSchema),
		func(args json.RawMessage) proto.Result {
			ensure()
			if provErr != nil {
				return proto.Errorf("web_search is not configured: %v", provErr)
			}
			if !rl.allow("web_search", 5*time.Second) {
				e.Notify("warn", "web_search rate limit hit; backing off")
				return proto.Errorf("web_search: rate limit reached; wait a few seconds")
			}
			var in struct {
				Query          string   `json:"query"`
				Count          int      `json:"count"`
				Freshness      string   `json:"freshness"`
				IncludeDomains []string `json:"include_domains"`
				ExcludeDomains []string `json:"exclude_domains"`
				Depth          string   `json:"depth"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return proto.Errorf("invalid args: %v", err)
			}
			q := search.Query{
				Text:           in.Query,
				Count:          in.Count,
				Freshness:      in.Freshness,
				IncludeDomains: in.IncludeDomains,
				ExcludeDomains: in.ExcludeDomains,
				Depth:          in.Depth,
			}
			if err := q.Normalize(); err != nil {
				return proto.Errorf("%v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			results, err := provider.Search(ctx, q)
			if err != nil {
				return proto.Errorf("search failed: %v", logSSRF(e, err))
			}
			return proto.Text(search.Format(in.Query, results))
		})

	e.Tool("web_fetch",
		"Fetch a web page (http/https) and return its main text content. Results are cached briefly: paging with offset (or repeating the call) within that window reads the same snapshot, so it won't drift mid-read; after the cache expires a re-fetch may differ, with new content typically appended at the end. Private/internal addresses are blocked unless explicitly allowlisted.",
		json.RawMessage(fetchSchema),
		func(args json.RawMessage) proto.Result {
			ensure()
			if !rl.allow("web_fetch", 2*time.Second) {
				e.Notify("warn", "web_fetch rate limit hit; backing off")
				return proto.Errorf("web_fetch: rate limit reached; wait a few seconds")
			}
			var in struct {
				URL       string `json:"url"`
				MaxChars  int    `json:"max_chars"`
				Offset    int    `json:"offset"`
				UserAgent string `json:"user_agent"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return proto.Errorf("invalid args: %v", err)
			}
			if strings.TrimSpace(in.URL) == "" {
				return proto.Errorf("url is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			text, err := fetcher.Fetch(ctx, in.URL, in.MaxChars, in.Offset, in.UserAgent)
			if err != nil {
				return proto.Errorf("fetch failed: %v", logSSRF(e, err))
			}
			return proto.Text(text)
		})

	e.Tool("web_images",
		"List the image URLs on a page that web_fetch represented as [image:N] placeholders. Cheap when the page was recently fetched (it is served from cache).",
		json.RawMessage(imagesSchema),
		func(args json.RawMessage) proto.Result {
			ensure()
			var in struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return proto.Errorf("invalid args: %v", err)
			}
			if strings.TrimSpace(in.URL) == "" {
				return proto.Errorf("url is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			imgs, err := fetcher.Images(ctx, in.URL)
			if err != nil {
				return proto.Errorf("web_images failed: %v", logSSRF(e, err))
			}
			return proto.Text(fetch.FormatImages(in.URL, imgs))
		})

	e.Tool("web_links",
		"List every hyperlink on a page (absolute URL plus anchor text). Use to enumerate a page's outbound links without scraping the fetched text yourself. Cheap when the page was recently fetched (served from cache).",
		json.RawMessage(linksSchema),
		func(args json.RawMessage) proto.Result {
			ensure()
			var in struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return proto.Errorf("invalid args: %v", err)
			}
			if strings.TrimSpace(in.URL) == "" {
				return proto.Errorf("url is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			links, err := fetcher.Links(ctx, in.URL)
			if err != nil {
				return proto.Errorf("web_links failed: %v", logSSRF(e, err))
			}
			return proto.Text(fetch.FormatLinks(in.URL, links))
		})

	e.Tool("web_fetch_raw",
		"Fetch a page and save its UNRENDERED source (HTML/JSON/text, exactly as the server sent it) to a workspace file for you to grep or parse yourself. A fallback for when web_fetch/web_images/web_links don't surface what you need. Served from the same cache as web_fetch. Private/internal addresses are blocked unless explicitly allowlisted.",
		json.RawMessage(webFetchRawSchema),
		func(args json.RawMessage) proto.Result {
			ensure()
			var in struct {
				URL       string `json:"url"`
				SavePath  string `json:"save_path"`
				Overwrite bool   `json:"overwrite"`
				UserAgent string `json:"user_agent"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return proto.Errorf("invalid args: %v", err)
			}
			if strings.TrimSpace(in.URL) == "" {
				return proto.Errorf("url is required")
			}
			if strings.TrimSpace(in.SavePath) == "" {
				return proto.Errorf("save_path is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			raw, err := fetcher.Raw(ctx, in.URL, in.UserAgent)
			if err != nil {
				return proto.Errorf("web_fetch_raw failed: %v", logSSRF(e, err))
			}
			rel, werr := saveToWorkspace(e.Host().CWD, in.SavePath, raw.Body, in.Overwrite)
			if werr != nil {
				return proto.Errorf("fetched the page but could not save it: %v", werr)
			}
			ctype := strings.TrimSpace(raw.ContentType)
			if ctype == "" {
				ctype = "unknown type"
			}
			var meta strings.Builder
			fmt.Fprintf(&meta, "Saved unrendered source to %s\n%s, %d bytes", rel, ctype, len(raw.Body))
			if raw.FinalURL != "" && raw.FinalURL != in.URL {
				fmt.Fprintf(&meta, " (final: %s)", raw.FinalURL)
			}
			if raw.Truncated {
				meta.WriteString("\n…source was capped at the fetch byte limit before saving")
			}
			return proto.Text(meta.String())
		})

	e.Tool("web_fetch_image",
		"Fetch an image (PNG/JPEG/GIF/WebP) by URL and return it for you to view, and/or save it into the workspace. Use max_dimension to downscale a large image. Private/internal addresses are blocked unless explicitly allowlisted.",
		json.RawMessage(webFetchImageSchema),
		func(args json.RawMessage) proto.Result {
			ensure()
			var in struct {
				URL          string `json:"url"`
				MaxDimension int    `json:"max_dimension"`
				SavePath     string `json:"save_path"`
				Overwrite    bool   `json:"overwrite"`
				Inject       *bool  `json:"inject"`
				UserAgent    string `json:"user_agent"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return proto.Errorf("invalid args: %v", err)
			}
			if strings.TrimSpace(in.URL) == "" {
				return proto.Errorf("url is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			img, err := fetcher.FetchImage(ctx, in.URL, in.MaxDimension, in.UserAgent)
			if err != nil {
				// ImageTooLargeError's message already tells the model how to
				// resubmit (with a suggested max_dimension), so pass it through.
				var tooBig *fetch.ImageTooLargeError
				if errors.As(err, &tooBig) {
					return proto.Errorf("web_fetch_image failed: %v", err)
				}
				return proto.Errorf("web_fetch_image failed: %v", logSSRF(e, err))
			}

			var meta strings.Builder
			fmt.Fprintf(&meta, "Fetched %s", in.URL)
			if img.FinalURL != "" && img.FinalURL != in.URL {
				fmt.Fprintf(&meta, " (final: %s)", img.FinalURL)
			}
			fmt.Fprintf(&meta, "\n%s, %d×%d, %.1f KiB", img.MimeType, img.Width, img.Height, float64(len(img.Data))/1024)
			if img.Resized {
				fmt.Fprintf(&meta, " (resized from %d×%d)", img.OrigW, img.OrigH)
			}

			if strings.TrimSpace(in.SavePath) != "" {
				rel, werr := saveToWorkspace(e.Host().CWD, in.SavePath, img.Data, in.Overwrite)
				if werr != nil {
					return proto.Errorf("fetched the image but could not save it: %v", werr)
				}
				fmt.Fprintf(&meta, "\nSaved to %s", rel)
			}

			inject := in.Inject == nil || *in.Inject
			if inject {
				return proto.Image(img.MimeType, img.Data, meta.String())
			}
			return proto.Text(meta.String())
		})

	e.Command("web-cache",
		"inspect the web page cache (`/web-cache`) or empty it (`/web-cache clear`)",
		func(args string) proto.CommandResult {
			ensure()
			switch strings.TrimSpace(args) {
			case "clear":
				n := fetcher.CacheClear()
				return proto.Display(fmt.Sprintf("web cache cleared (%d entries dropped)", n))
			case "", "list":
				entries := fetcher.CacheList()
				if len(entries) == 0 {
					return proto.Display("web cache is empty")
				}
				var b strings.Builder
				var total int64
				for _, en := range entries {
					total += en.Size
				}
				fmt.Fprintf(&b, "web cache: %d entries, %.1f KiB total\n", len(entries), float64(total)/1024)
				for _, en := range entries {
					age := time.Since(en.Stored).Round(time.Second)
					fmt.Fprintf(&b, "  %s  (%.1f KiB, %s old)", en.URL, float64(en.Size)/1024, age)
					if en.Title != "" {
						fmt.Fprintf(&b, "  — %s", en.Title)
					}
					b.WriteString("\n")
				}
				return proto.Display(strings.TrimRight(b.String(), "\n"))
			default:
				return proto.CommandResult{Action: "noop", Err: fmt.Sprintf("unknown argument %q (use `/web-cache` or `/web-cache clear`)", args)}
			}
		})

	if err := e.Run(); err != nil {
		e.Logf("fatal: %v", err)
	}
}

// versionString is what --version prints: version plus the toolchain and
// platform, which is exactly what's needed when debugging a mismatched or
// stale installed binary.
func versionString() string {
	return fmt.Sprintf("zot-web %s (%s, %s/%s)", version.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// saveToWorkspace writes data to savePath resolved under the workspace cwd. It
// refuses absolute paths, lexical/symlink escapes, writes through symlinks, and
// .git/ targets; creates parent directories within the workspace; and (unless
// overwrite) refuses to clobber an existing file. Returns the cleaned
// workspace-relative path written.
func saveToWorkspace(cwd, savePath string, data []byte, overwrite bool) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("no workspace directory available to save into")
	}
	if filepath.IsAbs(savePath) {
		return "", fmt.Errorf("save_path must be relative to the workspace, not absolute")
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace directory is not accessible: %w", err)
	}
	target := filepath.Join(root, filepath.Clean(savePath))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("save_path escapes the workspace")
	}
	// Refuse writes into .git/ — a prompt-injected model could overwrite
	// .git/config or other control files.
	if strings.HasPrefix(rel, ".git"+string(filepath.Separator)) || rel == ".git" {
		return "", fmt.Errorf("writing to .git/ is not permitted")
	}
	if err := mkdirAllNoSymlink(root, rootReal, filepath.Dir(rel)); err != nil {
		return "", err
	}
	if st, err := os.Lstat(target); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("save_path points to a symlink, which is not permitted")
		}
		if !overwrite {
			return "", fmt.Errorf("%s already exists (set overwrite=true to replace it)", rel)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// O_NOFOLLOW closes the TOCTOU between the Lstat symlink check above and this
	// open: even if a symlink is swapped into place in that window, the kernel
	// refuses to follow it for the final path component (overwrite uses O_TRUNC,
	// which would otherwise write through a symlink). oNoFollow is 0 on Windows.
	flag := os.O_WRONLY | os.O_CREATE | oNoFollow
	if overwrite {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_EXCL
	}
	f, err := os.OpenFile(target, flag, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return rel, nil
}

// mkdirAllNoSymlink creates relDir below root and rejects symlinked parent
// components. This keeps model-triggered saves from escaping the workspace via
// pre-existing symlinks such as "workspace/out -> /tmp/out".
func mkdirAllNoSymlink(root, rootReal, relDir string) error {
	if relDir == "." || relDir == "" {
		return nil
	}
	cur := root
	for _, elem := range strings.Split(filepath.Clean(relDir), string(filepath.Separator)) {
		if elem == "." || elem == "" {
			continue
		}
		cur = filepath.Join(cur, elem)
		st, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			if err := os.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			st, err = os.Lstat(cur)
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("save_path parent %q is a symlink, which is not permitted", elem)
		}
		if !st.IsDir() {
			return fmt.Errorf("save_path parent %q is not a directory", elem)
		}
		real, err := filepath.EvalSymlinks(cur)
		if err != nil {
			return err
		}
		if !pathWithin(rootReal, real) {
			return fmt.Errorf("save_path escapes the workspace")
		}
	}
	return nil
}

func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// logSSRF logs the full SSRF block details to the extension log when an error
// chain contains an SSRFBlockedError, and returns the model-safe message.
func logSSRF(e *proto.Extension, err error) string {
	var ssrf *fetch.SSRFBlockedError
	if errors.As(err, &ssrf) {
		e.Logf("%s", ssrf.Full())
		return ssrf.Error()
	}
	return err.Error()
}

// rateLimiter is a simple per-tool token-bucket rate limiter: it allows
// burst tools per toolKey with a refill rate of refillSec seconds.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]int
	burst   int
}

func newRateLimiter(burst int) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]int{},
		burst:   burst,
	}
}

// allow reports whether a call for key is within limits, consuming one token.
// The bucket refills by one token every refillSec seconds (lazily on each
// call).
func (rl *rateLimiter) allow(key string, refillSec time.Duration) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	tokens, ok := rl.buckets[key]
	if !ok {
		tokens = rl.burst
	}
	if tokens <= 0 {
		return false
	}
	rl.buckets[key] = tokens - 1
	// Start a goroutine to refill one token after refillSec.
	go func(k string) {
		time.Sleep(refillSec)
		rl.mu.Lock()
		if n := rl.buckets[k]; n < rl.burst {
			rl.buckets[k] = n + 1
		}
		rl.mu.Unlock()
	}(key)
	return true
}
