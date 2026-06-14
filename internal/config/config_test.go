package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c := Load("")

	if c.SearchBackend != "tavily" {
		t.Errorf("SearchBackend = %q, want \"tavily\"", c.SearchBackend)
	}
	if c.FetchMaxBytes != 2<<20 {
		t.Errorf("FetchMaxBytes = %d, want %d", c.FetchMaxBytes, 2<<20)
	}
	if c.FetchImageMaxBytes != 5<<20 {
		t.Errorf("FetchImageMaxBytes = %d, want %d", c.FetchImageMaxBytes, 5<<20)
	}
	if c.FetchTimeoutSec != 25 {
		t.Errorf("FetchTimeoutSec = %d, want 25", c.FetchTimeoutSec)
	}
	if c.FetchCacheTTLSec != 600 {
		t.Errorf("FetchCacheTTLSec = %d, want 600", c.FetchCacheTTLSec)
	}
	if c.FetchCacheMaxEntries != 32 {
		t.Errorf("FetchCacheMaxEntries = %d, want 32", c.FetchCacheMaxEntries)
	}
	if c.FetchCacheMaxBytes != DefaultFetchCacheMaxBytes {
		t.Errorf("FetchCacheMaxBytes = %d, want %d", c.FetchCacheMaxBytes, DefaultFetchCacheMaxBytes)
	}
	if c.FetchInlineImages != false {
		t.Errorf("FetchInlineImages = %v, want false", c.FetchInlineImages)
	}
	if len(c.AllowLocalHosts) != 3 || c.AllowLocalHosts[0] != "localhost" ||
		c.AllowLocalHosts[1] != "127.0.0.1" || c.AllowLocalHosts[2] != "::1" {
		t.Errorf("AllowLocalHosts = %v, want loopback defaults [localhost 127.0.0.1 ::1]", c.AllowLocalHosts)
	}
}

func TestLoadFromConfigJSON(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"search_backend": "searxng",
		"tavily_api_key": "tvly-from-file",
		"searxng_url": "http://searxng.local",
		"fetch_max_bytes": 1048576,
		"fetch_image_max_bytes": 2097152,
		"fetch_timeout_sec": 10,
		"fetch_inline_images": true,
		"fetch_cache_ttl_sec": 300,
		"fetch_cache_max_entries": 16,
		"allow_local_hosts": ["host1", "10.0.0.1"]
	}`)

	c := Load(dir)

	if c.SearchBackend != "searxng" {
		t.Errorf("SearchBackend = %q, want \"searxng\"", c.SearchBackend)
	}
	if c.TavilyAPIKey != "tvly-from-file" {
		t.Errorf("TavilyAPIKey = %q, want \"tvly-from-file\"", c.TavilyAPIKey)
	}
	if c.SearxngURL != "http://searxng.local" {
		t.Errorf("SearxngURL = %q, want \"http://searxng.local\"", c.SearxngURL)
	}
	if c.FetchMaxBytes != 1048576 {
		t.Errorf("FetchMaxBytes = %d, want 1048576", c.FetchMaxBytes)
	}
	if c.FetchImageMaxBytes != 2097152 {
		t.Errorf("FetchImageMaxBytes = %d, want 2097152", c.FetchImageMaxBytes)
	}
	if c.FetchTimeoutSec != 10 {
		t.Errorf("FetchTimeoutSec = %d, want 10", c.FetchTimeoutSec)
	}
	if c.FetchInlineImages != true {
		t.Errorf("FetchInlineImages = %v, want true", c.FetchInlineImages)
	}
	if c.FetchCacheTTLSec != 300 {
		t.Errorf("FetchCacheTTLSec = %d, want 300", c.FetchCacheTTLSec)
	}
	if c.FetchCacheMaxEntries != 16 {
		t.Errorf("FetchCacheMaxEntries = %d, want 16", c.FetchCacheMaxEntries)
	}
	if len(c.AllowLocalHosts) != 2 || c.AllowLocalHosts[0] != "host1" || c.AllowLocalHosts[1] != "10.0.0.1" {
		t.Errorf("AllowLocalHosts = %v, want [host1 10.0.0.1]", c.AllowLocalHosts)
	}
}

func TestEnvOverrides(t *testing.T) {
	env := map[string]string{
		"ZOT_WEB_SEARCH_BACKEND":          "SEARXNG",
		"TAVILY_API_KEY":                  "tvly-env",
		"ZOT_WEB_SEARXNG_URL":             "http://searxng.env:8888",
		"ZOT_WEB_FETCH_MAX_BYTES":         "4194304",
		"ZOT_WEB_FETCH_IMAGE_MAX_BYTES":   "10485760",
		"ZOT_WEB_FETCH_TIMEOUT_SEC":       "45",
		"ZOT_WEB_FETCH_INLINE_IMAGES":     "true",
		"ZOT_WEB_FETCH_CACHE_TTL_SEC":     "1200",
		"ZOT_WEB_FETCH_CACHE_MAX_ENTRIES": "64",
		"ZOT_WEB_ALLOW_LOCAL_HOSTS":       "env-host,192.168.1.0/24, , 10.0.0.1",
	}
	for k, v := range env {
		os.Setenv(k, v)
		defer os.Unsetenv(k)
	}

	c := Load("")

	if c.SearchBackend != "searxng" {
		t.Errorf("SearchBackend = %q, want \"searxng\" (lowercased env)", c.SearchBackend)
	}
	if c.TavilyAPIKey != "tvly-env" {
		t.Errorf("TavilyAPIKey = %q, want \"tvly-env\"", c.TavilyAPIKey)
	}
	if c.SearxngURL != "http://searxng.env:8888" {
		t.Errorf("SearxngURL = %q, want \"http://searxng.env:8888\"", c.SearxngURL)
	}
	if c.FetchMaxBytes != 4194304 {
		t.Errorf("FetchMaxBytes = %d, want 4194304", c.FetchMaxBytes)
	}
	if c.FetchImageMaxBytes != 10485760 {
		t.Errorf("FetchImageMaxBytes = %d, want 10485760", c.FetchImageMaxBytes)
	}
	if c.FetchTimeoutSec != 45 {
		t.Errorf("FetchTimeoutSec = %d, want 45", c.FetchTimeoutSec)
	}
	if c.FetchInlineImages != true {
		t.Errorf("FetchInlineImages = %v, want true", c.FetchInlineImages)
	}
	if c.FetchCacheTTLSec != 1200 {
		t.Errorf("FetchCacheTTLSec = %d, want 1200", c.FetchCacheTTLSec)
	}
	if c.FetchCacheMaxEntries != 64 {
		t.Errorf("FetchCacheMaxEntries = %d, want 64", c.FetchCacheMaxEntries)
	}
	if len(c.AllowLocalHosts) != 6 {
		t.Errorf("AllowLocalHosts len = %d, want 6 (3 defaults + 3 from env, empty trimmed)", len(c.AllowLocalHosts))
	}
}

func TestFetchCacheMaxEntriesNegativeClampedToZero(t *testing.T) {
	// From config.json: JSON unmarshals the negative value, then clamping zeros it.
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_cache_max_entries": -5}`)
	c := Load(dir)
	if c.FetchCacheMaxEntries != 0 {
		t.Errorf("FetchCacheMaxEntries = %d, want 0 (negative clamped)", c.FetchCacheMaxEntries)
	}

	// From env: the env parser already guards n >= 0, so negative values are
	// silently rejected and the default is kept.
	t.Setenv("ZOT_WEB_FETCH_CACHE_MAX_ENTRIES", "-1")
	c = Load("")
	if c.FetchCacheMaxEntries != 32 {
		t.Errorf("FetchCacheMaxEntries = %d, want 32 (negative env rejected, stays default)", c.FetchCacheMaxEntries)
	}
}

func TestFetchMaxBytesZeroOrNegativeDefaults(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_max_bytes": 0}`)
	c := Load(dir)
	if c.FetchMaxBytes != 2<<20 {
		t.Errorf("FetchMaxBytes = %d, want %d (zero → default)", c.FetchMaxBytes, 2<<20)
	}

	writeJSON(t, dir, `{"fetch_max_bytes": -100}`)
	c = Load(dir)
	if c.FetchMaxBytes != 2<<20 {
		t.Errorf("FetchMaxBytes = %d, want %d (negative → default)", c.FetchMaxBytes, 2<<20)
	}
}

func TestFetchImageMaxBytesZeroOrNegativeDefaults(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_image_max_bytes": 0}`)
	c := Load(dir)
	if c.FetchImageMaxBytes != 5<<20 {
		t.Errorf("FetchImageMaxBytes = %d, want %d (zero → default)", c.FetchImageMaxBytes, 5<<20)
	}

	writeJSON(t, dir, `{"fetch_image_max_bytes": -1}`)
	c = Load(dir)
	if c.FetchImageMaxBytes != 5<<20 {
		t.Errorf("FetchImageMaxBytes = %d, want %d (negative → default)", c.FetchImageMaxBytes, 5<<20)
	}
}

func TestFetchTimeoutSecZeroOrNegativeDefaults(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_timeout_sec": 0}`)
	c := Load(dir)
	if c.FetchTimeoutSec != 25 {
		t.Errorf("FetchTimeoutSec = %d, want 25 (zero → default)", c.FetchTimeoutSec)
	}
}

func TestOversizedConfigValuesAreClamped(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{
		"fetch_max_bytes": 999999999,
		"fetch_image_max_bytes": 999999999,
		"fetch_timeout_sec": 999,
		"fetch_cache_max_entries": 999
	}`)
	c := Load(dir)
	if c.FetchMaxBytes != MaxFetchMaxBytes {
		t.Errorf("FetchMaxBytes = %d, want max %d", c.FetchMaxBytes, MaxFetchMaxBytes)
	}
	if c.FetchImageMaxBytes != MaxFetchImageMaxBytes {
		t.Errorf("FetchImageMaxBytes = %d, want max %d", c.FetchImageMaxBytes, MaxFetchImageMaxBytes)
	}
	if c.FetchTimeoutSec != MaxFetchTimeoutSec {
		t.Errorf("FetchTimeoutSec = %d, want max %d", c.FetchTimeoutSec, MaxFetchTimeoutSec)
	}
	if c.FetchCacheMaxEntries != MaxFetchCacheMaxEntries {
		t.Errorf("FetchCacheMaxEntries = %d, want max %d", c.FetchCacheMaxEntries, MaxFetchCacheMaxEntries)
	}
}

func TestOversizedEnvValuesAreClamped(t *testing.T) {
	t.Setenv("ZOT_WEB_FETCH_MAX_BYTES", "999999999")
	t.Setenv("ZOT_WEB_FETCH_IMAGE_MAX_BYTES", "999999999")
	t.Setenv("ZOT_WEB_FETCH_TIMEOUT_SEC", "999")
	t.Setenv("ZOT_WEB_FETCH_CACHE_MAX_ENTRIES", "999")
	c := Load("")
	if c.FetchMaxBytes != MaxFetchMaxBytes || c.FetchImageMaxBytes != MaxFetchImageMaxBytes ||
		c.FetchTimeoutSec != MaxFetchTimeoutSec || c.FetchCacheMaxEntries != MaxFetchCacheMaxEntries {
		t.Fatalf("oversized env values not clamped: %+v", c)
	}
}

func TestFetchCacheTTLSecClamped(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_cache_ttl_sec": 999999}`)
	c := Load(dir)
	if c.FetchCacheTTLSec != MaxFetchCacheTTLSec {
		t.Errorf("FetchCacheTTLSec = %d, want max %d", c.FetchCacheTTLSec, MaxFetchCacheTTLSec)
	}

	writeJSON(t, dir, `{"fetch_cache_ttl_sec": -5}`)
	c = Load(dir)
	if c.FetchCacheTTLSec != 0 {
		t.Errorf("FetchCacheTTLSec = %d, want 0 (negative clamped)", c.FetchCacheTTLSec)
	}
}

func TestFetchCacheMaxBytesClampedAndDefaulted(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_cache_max_bytes": 9999999999}`)
	if c := Load(dir); c.FetchCacheMaxBytes != MaxFetchCacheMaxBytes {
		t.Errorf("FetchCacheMaxBytes = %d, want max %d", c.FetchCacheMaxBytes, MaxFetchCacheMaxBytes)
	}

	// Negative from JSON clamps to 0 (byte bound disabled); env negative is
	// rejected by the n >= 0 guard and keeps the default.
	writeJSON(t, dir, `{"fetch_cache_max_bytes": -1}`)
	if c := Load(dir); c.FetchCacheMaxBytes != 0 {
		t.Errorf("FetchCacheMaxBytes = %d, want 0 (negative clamped)", c.FetchCacheMaxBytes)
	}

	t.Setenv("ZOT_WEB_FETCH_CACHE_MAX_BYTES", "33554432")
	if c := Load(""); c.FetchCacheMaxBytes != 33554432 {
		t.Errorf("FetchCacheMaxBytes = %d, want 33554432 (env override)", c.FetchCacheMaxBytes)
	}
}

func TestEmptySearchBackendDefaultsToTavily(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"search_backend": ""}`)
	c := Load(dir)
	if c.SearchBackend != "tavily" {
		t.Errorf("SearchBackend = %q, want \"tavily\"", c.SearchBackend)
	}
}

func TestAllowLocalHostsCommaSeparated(t *testing.T) {
	dir := t.TempDir()
	// A config.json key replaces the loopback defaults wholesale.
	writeJSON(t, dir, `{"allow_local_hosts": ["one", "two"]}`)
	c := Load(dir)
	if len(c.AllowLocalHosts) != 2 {
		t.Fatalf("len = %d, want 2 (file replaces defaults)", len(c.AllowLocalHosts))
	}
	if c.AllowLocalHosts[0] != "one" || c.AllowLocalHosts[1] != "two" {
		t.Errorf("AllowLocalHosts = %v", c.AllowLocalHosts)
	}

	// Env appends to file entries (uses append, not replace).
	t.Setenv("ZOT_WEB_ALLOW_LOCAL_HOSTS", "a,b,c")
	c = Load(dir)
	if len(c.AllowLocalHosts) != 5 {
		t.Fatalf("len = %d, want 5 (file + env append)", len(c.AllowLocalHosts))
	}
	want := []string{"one", "two", "a", "b", "c"}
	for i, w := range want {
		if c.AllowLocalHosts[i] != w {
			t.Errorf("AllowLocalHosts[%d] = %q, want %q", i, c.AllowLocalHosts[i], w)
		}
	}
}

func TestAllowLocalHostsEmptyArrayOptsOut(t *testing.T) {
	// An explicit [] is the lockdown switch: it replaces the loopback
	// defaults with nothing, restoring block-everything-private behavior.
	dir := t.TempDir()
	writeJSON(t, dir, `{"allow_local_hosts": []}`)
	c := Load(dir)
	if len(c.AllowLocalHosts) != 0 {
		t.Errorf("AllowLocalHosts = %v, want empty (explicit [] opts out of defaults)", c.AllowLocalHosts)
	}
}

func TestFetchInlineImagesBoolParsing(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"1", true},
		{"0", false},
		{"True", true},
		{"TRUE", true},
	}
	for _, tt := range tests {
		t.Setenv("ZOT_WEB_FETCH_INLINE_IMAGES", tt.val)
		c := Load("")
		if c.FetchInlineImages != tt.want {
			t.Errorf("FetchInlineImages(%q) = %v, want %v", tt.val, c.FetchInlineImages, tt.want)
		}
	}
}

func TestFetchCacheTTLSecParsing(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"fetch_cache_ttl_sec": 123}`)
	c := Load(dir)
	if c.FetchCacheTTLSec != 123 {
		t.Errorf("FetchCacheTTLSec = %d, want 123", c.FetchCacheTTLSec)
	}

	t.Setenv("ZOT_WEB_FETCH_CACHE_TTL_SEC", "456")
	c = Load("")
	if c.FetchCacheTTLSec != 456 {
		t.Errorf("FetchCacheTTLSec = %d, want 456", c.FetchCacheTTLSec)
	}
}

func TestSearchBackendLowercaseNormalization(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"SEARXNG", "searxng"},
		{"Tavily", "tavily"},
		{"SearXNG", "searxng"},
		{"  searxng  ", "searxng"},
		{"SEARXNG  ", "searxng"},
	}
	for _, tt := range tests {
		t.Setenv("ZOT_WEB_SEARCH_BACKEND", tt.in)
		c := Load("")
		if c.SearchBackend != tt.want {
			t.Errorf("SearchBackend(%q) = %q, want %q", tt.in, c.SearchBackend, tt.want)
		}
	}
}

func TestSearchBackendTrimSpace(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, `{"search_backend": "  searxng  "}`)
	c := Load(dir)
	if c.SearchBackend != "searxng" {
		t.Errorf("SearchBackend = %q after TrimSpace, want \"searxng\"", c.SearchBackend)
	}
}

func TestEnvFetchMaxBytesInvalidFallsBack(t *testing.T) {
	t.Setenv("ZOT_WEB_FETCH_MAX_BYTES", "not-a-number")
	c := Load("")
	if c.FetchMaxBytes != 2<<20 {
		t.Errorf("FetchMaxBytes = %d, want default %d", c.FetchMaxBytes, 2<<20)
	}
}

func TestEnvFetchTimeoutSecInvalidFallsBack(t *testing.T) {
	t.Setenv("ZOT_WEB_FETCH_TIMEOUT_SEC", "abc")
	c := Load("")
	if c.FetchTimeoutSec != 25 {
		t.Errorf("FetchTimeoutSec = %d, want default 25", c.FetchTimeoutSec)
	}
}

func TestMalformedJSONFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{bad json!!!"), 0644)
	c := Load(dir)
	if c.SearchBackend != "tavily" {
		t.Errorf("SearchBackend = %q, want \"tavily\" (malformed JSON → defaults)", c.SearchBackend)
	}
}

func writeJSON(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
