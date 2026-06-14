// Package config loads the extension's configuration from its data_dir
// config.json (persisted beside the binary) with environment-variable
// overrides taking precedence — secrets in particular are best passed via env.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultFetchMaxBytes        int64 = 2 << 20 // 2 MiB
	DefaultFetchImageMaxBytes   int64 = 5 << 20 // 5 MiB
	DefaultFetchTimeoutSec            = 25
	DefaultFetchCacheTTLSec           = 600
	DefaultFetchCacheMaxEntries       = 32
	DefaultFetchCacheMaxBytes   int64 = 64 << 20 // 64 MiB

	MaxFetchMaxBytes        int64 = 32 << 20 // 32 MiB
	MaxFetchImageMaxBytes   int64 = 20 << 20 // 20 MiB
	MaxFetchTimeoutSec            = 60
	MaxFetchCacheTTLSec           = 3600 // 1 hour
	MaxFetchCacheMaxEntries       = 128
	MaxFetchCacheMaxBytes   int64 = 256 << 20 // 256 MiB
)

// DefaultAllowLocalHosts is the out-of-the-box SSRF allowlist: loopback only,
// by name and by literal address (the hostname entry already covers whatever
// "localhost" resolves to; the IPs cover URLs that dial 127.0.0.1/[::1]
// directly). Everything else private/reserved stays blocked until the user
// opts in.
var DefaultAllowLocalHosts = []string{"localhost", "127.0.0.1", "::1"}

// Config is the effective settings for the web extension.
type Config struct {
	// SearchBackend selects the web_search provider: "tavily" (default) or
	// "searxng".
	SearchBackend string `json:"search_backend"`
	// TavilyAPIKey authenticates the Tavily backend.
	TavilyAPIKey string `json:"tavily_api_key"`
	// SearxngURL is the base URL of a self-hosted SearXNG instance (JSON
	// format must be enabled in its settings.yml).
	SearxngURL string `json:"searxng_url"`

	// FetchMaxBytes caps a fetched response body. Default 2 MiB; max 32 MiB.
	FetchMaxBytes int64 `json:"fetch_max_bytes"`
	// FetchImageMaxBytes caps the encoded size of an image returned by
	// web_fetch_image for multimodal injection. Default 5 MiB; max 20 MiB (≈ provider limits).
	// Images larger than this (after any requested resize) are rejected with a
	// hint to resubmit with a smaller max_dimension. The raw download is allowed
	// to exceed this so an oversized original can be decoded and resized down.
	FetchImageMaxBytes int64 `json:"fetch_image_max_bytes"`
	// FetchTimeoutSec is the overall per-fetch timeout. Default 25s; max 60s.
	FetchTimeoutSec int `json:"fetch_timeout_sec"`

	// FetchInlineImages keeps image URLs inline in web_fetch output. Default
	// false: images are replaced with `[image:N]` placeholders and the URLs
	// are retrieved separately via the web_images tool.
	FetchInlineImages bool `json:"fetch_inline_images"`
	// FetchCacheTTLSec is how long a fetched+rendered page stays cached so a
	// follow-up web_images call needs no network. Default 600s. 0 disables.
	FetchCacheTTLSec int `json:"fetch_cache_ttl_sec"`
	// FetchCacheMaxEntries bounds the in-memory page cache (LRU). Default 32; max 128.
	FetchCacheMaxEntries int `json:"fetch_cache_max_entries"`
	// FetchCacheMaxBytes bounds the page cache by total retained bytes (rendered
	// Markdown + compressed raw body + harvested links/images), evicting LRU
	// entries until under budget. This is the real memory backstop: a handful of
	// large pages can dominate long before the entry count does. Default 64 MiB;
	// max 256 MiB. 0 disables the byte bound (entry count still applies).
	FetchCacheMaxBytes int64 `json:"fetch_cache_max_bytes"`

	// UserAgent overrides the User-Agent sent on every fetch. Empty means the
	// default "zot-web/<version>". The special value "browser" expands to a
	// common desktop-browser UA, for sites that block non-browser clients.
	// A per-call user_agent tool parameter takes precedence over this.
	UserAgent string `json:"user_agent"`

	// AllowLocalHosts is the SSRF escape hatch: targets that resolve to
	// private/reserved addresses are refused UNLESS they match an entry here.
	// Each entry is a hostname (matched against the request host), an IP, or
	// a CIDR (matched against the resolved IP). Defaults to loopback
	// (DefaultAllowLocalHosts); a config.json key REPLACES the default — write
	// the full list to extend it, or [] to lock loopback back down. The env
	// override appends instead.
	AllowLocalHosts []string `json:"allow_local_hosts"`
}

// Load reads config.json (if present), then applies env overrides. It
// looks in dataDir first and falls back to extensionDir.
//
// On a terva host that split the dirs, dataDir is the writable
// $TERVA_HOME/ext-data/web and extensionDir is the read-only install
// dir. Older hosts (and the zot protocol) report both as the same install
// dir, where config.json historically lived — so the fallback keeps an
// existing config.json readable across that host change, and the lookup
// is identical (a harmless double-read) on an old host. Pass "" for
// extensionDir if the host didn't provide one.
func Load(dataDir, extensionDir string) Config {
	c := Config{
		SearchBackend:        "tavily",
		FetchMaxBytes:        DefaultFetchMaxBytes,
		FetchImageMaxBytes:   DefaultFetchImageMaxBytes,
		FetchTimeoutSec:      DefaultFetchTimeoutSec,
		FetchCacheTTLSec:     DefaultFetchCacheTTLSec,
		FetchCacheMaxEntries: DefaultFetchCacheMaxEntries,
		FetchCacheMaxBytes:   DefaultFetchCacheMaxBytes,
		// Copy: Unmarshal overwrites the slice in place when the key is
		// present, and the package-level default must not be clobbered.
		AllowLocalHosts: append([]string(nil), DefaultAllowLocalHosts...),
	}
	for _, dir := range []string{dataDir, extensionDir} {
		if dir == "" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			_ = json.Unmarshal(b, &c)
			break
		}
	}

	if v := os.Getenv("ZOT_WEB_SEARCH_BACKEND"); v != "" {
		c.SearchBackend = v
	}
	if v := os.Getenv("TAVILY_API_KEY"); v != "" {
		c.TavilyAPIKey = v
	}
	if v := os.Getenv("ZOT_WEB_SEARXNG_URL"); v != "" {
		c.SearxngURL = v
	}
	if v := os.Getenv("ZOT_WEB_USER_AGENT"); v != "" {
		c.UserAgent = v
	}
	if v := os.Getenv("ZOT_WEB_ALLOW_LOCAL_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowLocalHosts = append(c.AllowLocalHosts, h)
			}
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.FetchMaxBytes = n
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_IMAGE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.FetchImageMaxBytes = n
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.FetchTimeoutSec = n
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_INLINE_IMAGES"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.FetchInlineImages = b
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_CACHE_TTL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.FetchCacheTTLSec = n
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_CACHE_MAX_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.FetchCacheMaxEntries = n
		}
	}
	if v := os.Getenv("ZOT_WEB_FETCH_CACHE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.FetchCacheMaxBytes = n
		}
	}

	c.SearchBackend = strings.ToLower(strings.TrimSpace(c.SearchBackend))
	if c.SearchBackend == "" {
		c.SearchBackend = "tavily"
	}
	if c.FetchMaxBytes <= 0 {
		c.FetchMaxBytes = DefaultFetchMaxBytes
	}
	if c.FetchMaxBytes > MaxFetchMaxBytes {
		c.FetchMaxBytes = MaxFetchMaxBytes
	}
	if c.FetchImageMaxBytes <= 0 {
		c.FetchImageMaxBytes = DefaultFetchImageMaxBytes
	}
	if c.FetchImageMaxBytes > MaxFetchImageMaxBytes {
		c.FetchImageMaxBytes = MaxFetchImageMaxBytes
	}
	if c.FetchTimeoutSec <= 0 {
		c.FetchTimeoutSec = DefaultFetchTimeoutSec
	}
	if c.FetchTimeoutSec > MaxFetchTimeoutSec {
		c.FetchTimeoutSec = MaxFetchTimeoutSec
	}
	if c.FetchCacheTTLSec < 0 {
		c.FetchCacheTTLSec = 0
	}
	if c.FetchCacheTTLSec > MaxFetchCacheTTLSec {
		c.FetchCacheTTLSec = MaxFetchCacheTTLSec
	}
	if c.FetchCacheMaxEntries < 0 {
		c.FetchCacheMaxEntries = 0
	}
	if c.FetchCacheMaxEntries > MaxFetchCacheMaxEntries {
		c.FetchCacheMaxEntries = MaxFetchCacheMaxEntries
	}
	if c.FetchCacheMaxBytes < 0 {
		c.FetchCacheMaxBytes = 0
	}
	if c.FetchCacheMaxBytes > MaxFetchCacheMaxBytes {
		c.FetchCacheMaxBytes = MaxFetchCacheMaxBytes
	}
	return c
}
