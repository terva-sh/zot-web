package fetch

import (
	"sort"
	"sync"
	"time"
)

// Image is one image found on a fetched page. ID is the page-local handle the
// model sees as `[image:ID]`; URL is resolved absolute against the page URL.
type Image struct {
	ID         int    `json:"id"`
	URL        string `json:"url"`
	Alt        string `json:"alt,omitempty"`
	Caption    string `json:"caption,omitempty"`     // nearest <figcaption>
	Width      string `json:"width,omitempty"`       // from the <img width> attr
	Height     string `json:"height,omitempty"`      // from the <img height> attr
	SourcePage string `json:"source_page,omitempty"` // enclosing <a href> (e.g. a Wikimedia File: page)
}

// page is the fully rendered result of a fetch, cached so a follow-up
// web_images / web_links / web_fetch_raw call needs no network. Markdown is
// untruncated (carrying `[image:N]` placeholders unless inline images are
// configured); per-request character limits are applied at response time.
type page struct {
	URL            string // the requested URL (cache key)
	FinalURL       string // URL after redirects
	Title          string
	ContentType    string
	Status         int
	Markdown       string
	Images         []Image
	ImagesInline   bool   // Images appear in Markdown as [image:N] placeholders
	Links          []Link // every <a href> on the page (whole document)
	RawGzip        []byte // gzip-compressed unrendered response body (for web_fetch_raw)
	BodyTruncated  bool   // raw body hit the byte cap before rendering
	MarkdownCapped bool   // rendered Markdown hit maxRenderedRunes (tail dropped)
}

// cache is a small, concurrency-safe, TTL + LRU page cache bounded by both entry
// count and total retained bytes. Tool handlers run in their own goroutines (see
// proto.Run), so every access takes the lock.
type cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	max      int
	maxBytes int64
	bytes    int64 // sum of entry sizes currently held
	entries  map[string]*entry
}

type entry struct {
	page     page
	size     int64
	stored   time.Time
	accessed time.Time
}

// newCache builds a cache. A non-positive max disables caching entirely (get
// always misses, put is a no-op); a non-positive ttl means entries never
// expire by age; a non-positive maxBytes disables the byte bound (entry count
// still applies).
func newCache(ttl time.Duration, max int, maxBytes int64) *cache {
	return &cache{ttl: ttl, max: max, maxBytes: maxBytes, entries: map[string]*entry{}}
}

// pageSize estimates the heap a cached page retains, so the cache can evict on
// bytes rather than just entry count. It counts the two big buffers (compressed
// raw body + rendered Markdown) plus the harvested link/image strings, which are
// otherwise unbounded on link-farm pages.
func pageSize(p page) int64 {
	n := int64(len(p.RawGzip) + len(p.Markdown))
	for _, l := range p.Links {
		n += int64(len(l.URL)+len(l.Text)) + 16
	}
	for _, im := range p.Images {
		n += int64(len(im.URL)+len(im.Alt)+len(im.Caption)+len(im.SourcePage)+len(im.Width)+len(im.Height)) + 32
	}
	return n
}

// get returns the cached page for url and whether it was a live hit. Expired
// entries are dropped and reported as a miss.
func (c *cache) get(url string) (page, bool) {
	if c.max <= 0 {
		return page{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[url]
	if !ok {
		return page{}, false
	}
	if c.ttl > 0 && time.Since(e.stored) > c.ttl {
		c.remove(url)
		return page{}, false
	}
	e.accessed = time.Now()
	return e.page, true
}

// put stores p, evicting the least-recently-accessed entries until both the
// entry-count and byte budgets are satisfied.
func (c *cache) put(p page) {
	if c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[p.URL]; ok {
		c.bytes -= old.size
	}
	now := time.Now()
	size := pageSize(p)
	c.entries[p.URL] = &entry{page: p, size: size, stored: now, accessed: now}
	c.bytes += size
	c.evict(p.URL)
}

// remove deletes key and decrements the byte total. Caller holds the lock.
func (c *cache) remove(key string) {
	if e, ok := c.entries[key]; ok {
		c.bytes -= e.size
		delete(c.entries, key)
	}
}

// CacheEntryInfo describes one cached page, for the /web-cache command.
type CacheEntryInfo struct {
	URL    string
	Title  string
	Size   int64
	Stored time.Time
}

// list snapshots the cache contents, newest-stored first.
func (c *cache) list() []CacheEntryInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CacheEntryInfo, 0, len(c.entries))
	for k, e := range c.entries {
		out = append(out, CacheEntryInfo{URL: k, Title: e.page.Title, Size: e.size, Stored: e.stored})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stored.After(out[j].Stored) })
	return out
}

// clear empties the cache, returning how many entries were dropped.
func (c *cache) clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	c.entries = map[string]*entry{}
	c.bytes = 0
	return n
}

// evict drops least-recently-accessed entries until the cache is within both
// bounds. keep is never evicted (the entry just inserted), so a single page
// larger than the whole byte budget is still cached as the sole entry rather
// than thrashing. Caller holds the lock.
func (c *cache) evict(keep string) {
	for len(c.entries) > c.max || (c.maxBytes > 0 && c.bytes > c.maxBytes) {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.entries {
			if k == keep {
				continue
			}
			if oldestKey == "" || e.accessed.Before(oldest) {
				oldestKey, oldest = k, e.accessed
			}
		}
		if oldestKey == "" { // only keep remains
			return
		}
		c.remove(oldestKey)
	}
}
