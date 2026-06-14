# zot-web

A [zot](https://github.com/patriceckhart/zot) extension that gives the agent
web access through six LLM-callable tools:

- **`web_search(query, count?, freshness?, include_domains?, exclude_domains?, depth?)`**
  — ranked results (title, URL, snippet, publication date when the backend
  reports one). `freshness` (`day`/`week`/`month`/`year`) windows results by
  publication date; `include_domains`/`exclude_domains` filter by site
  (Tavily natively; SearXNG via a `site:` hint plus post-filtering);
  `depth: "advanced"` requests Tavily's deeper search tier.
- **`web_fetch(url, max_chars?, offset?, user_agent?)`** — a page's main content as Markdown,
  led by a metadata block. Image URLs are replaced with compact `[image:N]`
  placeholders to save tokens; `offset` pages through long documents. The
  rendered page is cached, so paging with `offset` (or re-fetching) within the
  cache window reads the same snapshot and won't drift mid-read.
- **`web_images(url)`** — resolve the `[image:N]` placeholders from a fetched
  page back to their URLs (plus dimensions, caption, and source page). Served
  from cache when warm; fetches on a cold cache, so it also works standalone.
  Discovery covers lazy-load attributes (`data-src`, `srcset`, `data-bg`/`data-background-image`), `<noscript>` fallbacks, `<picture>`
  sources, `<a>` links straight to an image, and `og:image`/`twitter:image`,
  and falls back to a whole-page scan on pages readability can't article-ify.
- **`web_links(url)`** — every hyperlink on a page (absolute URL + anchor text),
  de-duplicated. Lets the model enumerate a page's links without scraping the
  fetched text. Cache-backed like `web_images`.
- **`web_fetch_image(url, max_dimension?, save_path?, overwrite?, inject?, user_agent?)`** —
  fetch an image and return it for native multimodal viewing and/or save it into
  the workspace. `max_dimension` downscales oversized images.
- **`web_fetch_raw(url, save_path, overwrite?, user_agent?)`** — save a page's *unrendered*
  source (HTML/JSON/text, exactly as served) to a workspace file for the model
  to grep/parse itself — an escape hatch when the structured tools miss
  something. Reuses the same SSRF guard and page cache as `web_fetch`.

Single static Go binary, no external runtime services. It implements the zot
extension wire protocol directly (no dependency on the zot module).

> **Status: v0.** Search (Tavily + SearXNG), the SSRF-guarded fetcher, and
> article extraction (readability via the maintained
> `codeberg.org/readeck/go-readability` fork → `html-to-markdown` with GFM
> tables, image indexing, and a heuristic tag-stripper fallback) are all
> functional. Design rationale lives in the zot repo at
> `docs/plans/web-tools-extension-research.md`.

## Quick start (`just`)

With [`just`](https://github.com/casey/just) installed, the whole flow is two
commands — repeatable on any machine:

```bash
just install                 # build, then (re)install into $ZOT_HOME/extensions/
just configure-searxng       # point it at the default local SearXNG (127.0.0.1:11984)
# or target a specific instance:
just configure-searxng https://searx.example/
```

`just configure-searxng` writes `config.json` into the installed extension's
data dir, resolving that dir from `ext list` so it works regardless of OS
(macOS, Linux) or a custom home. A bare `host:port` is accepted and gets
an `http://` prefix. The written config allowlists the SearXNG host alongside
the loopback defaults, so the SSRF guard permits that deliberate backend even
when it lives on a LAN or VPN address. The default instance is the
`SEARXNG_URL` variable at the top of the `justfile`.

Both recipes target [terva](#host-integration-zot-and-terva) by default; pass
`zot` (`just install zot`) or override `HOST` to target stock zot.

`just install` removes and recopies the install dir, but **preserves an
existing `config.json`** across the reinstall — so you only need
`configure-searxng` once (or to change instances).

See `just --list` for the rest (`try`, `lint`, `test`, …).

## Manual install & the `run.sh` launcher

```bash
# From a git URL — zot shallow-clones the repo (it does NOT build Go sources):
zot ext install https://github.com/terva-sh/zot-web.git
# From a local checkout:
zot ext install /path/to/zot-web
# Or, for one session straight from the working copy:
zot --ext /path/to/zot-web
```

`extension.json` points `exec` at **`./run.sh`**, a launcher that compiles the
binary on first launch (and after any source change) and then execs it. zot
never builds Go sources itself (`language` is informational), so this is what
makes a git-URL install work without committing a platform-specific binary — at
the cost of needing a **Go 1.25+ toolchain on `PATH`** on the host and a
one-time build before the extension responds. The build is **offline**
(`go build -mod=vendor` against the committed `vendor/` tree), so the first
launch needs no network and can't hang on a module fetch — which matters because
zot blocks its startup until the extension sends its `hello`. Build chatter goes
to stderr (zot captures it to `$ZOT_HOME/logs/ext-web.log`); the compiled
`./zot-web` is gitignored.

`just install` still builds locally and copies the binary into the install dir,
pre-seeding it so the first launch skips the build. The install dir is named
after the source folder's basename (here, `zot-web`), not the manifest `name`
(`web`); `zot --ext` runs from the working copy directly.

### Dependencies are vendored

`vendor/` is committed so the first-launch build is fast and offline (see
above). After changing dependencies, refresh it with `just vendor` (runs
`go mod tidy` + `go mod vendor`) and commit the result alongside
`go.mod`/`go.sum`. **Re-evaluate this approach if `vendor/` grows large**
(currently ~6 MB across a handful of modules): past some point, shipping
prebuilt per-platform binaries (goreleaser) beats carrying a big vendor tree.

## Configure

Settings come from `config.json` in the extension's data dir
(`$ZOT_HOME/extensions/zot-web/config.json`), with environment variables taking
precedence. `just configure-searxng` (above) writes this file for you; to do it
by hand, start from the default Tavily backend:

```bash
export TAVILY_API_KEY=tvly-...
```

Or switch to a self-hosted SearXNG instance (no key, private):

```jsonc
// $ZOT_HOME/extensions/zot-web/config.json
{
  "search_backend": "searxng",
  "searxng_url": "http://127.0.0.1:11984"
}
```

> SearXNG must have `json` listed under `search.formats` in its `settings.yml`,
> otherwise its API returns `403`.
>
> SearXNG queries run through the **same SSRF guard** as `web_fetch`. Loopback
> is allowed out of the box, so the example above just works — but an instance
> on a LAN/VPN address must be in `allow_local_hosts` or every search is
> blocked (see [the allowlist](#security-ssrf-protection--the-local-allowlist)).
> `just configure-searxng` writes that entry for you.

### All settings

| config.json key | env override | default | meaning |
|---|---|---|---|
| `search_backend` | `ZOT_WEB_SEARCH_BACKEND` | `tavily` | `tavily` or `searxng` |
| `tavily_api_key` | `TAVILY_API_KEY` | — | Tavily bearer token |
| `searxng_url` | `ZOT_WEB_SEARXNG_URL` | — | SearXNG base URL |
| `fetch_max_bytes` | `ZOT_WEB_FETCH_MAX_BYTES` | `2097152` | response body cap (clamped to max `33554432`) |
| `fetch_image_max_bytes` | `ZOT_WEB_FETCH_IMAGE_MAX_BYTES` | `5242880` | max encoded size of a `web_fetch_image` result after resize (clamped to max `20971520`) |
| `fetch_timeout_sec` | `ZOT_WEB_FETCH_TIMEOUT_SEC` | `25` | per-fetch timeout (clamped to max `60`) |
| `fetch_inline_images` | `ZOT_WEB_FETCH_INLINE_IMAGES` | `false` | keep image URLs inline instead of `[image:N]` placeholders |
| `fetch_cache_ttl_sec` | `ZOT_WEB_FETCH_CACHE_TTL_SEC` | `600` | how long a rendered page stays cached (`0` = no expiry; clamped to max `3600`) |
| `fetch_cache_max_entries` | `ZOT_WEB_FETCH_CACHE_MAX_ENTRIES` | `32` | max cached pages, LRU-evicted (`0` = caching off; clamped to max `128`) |
| `fetch_cache_max_bytes` | `ZOT_WEB_FETCH_CACHE_MAX_BYTES` | `67108864` | total bytes the page cache may retain, LRU-evicted (`0` = no byte bound; clamped to max `268435456`) |
| `user_agent` | `ZOT_WEB_USER_AGENT` | `zot-web/<version>` | User-Agent for every fetch; `browser` expands to a common desktop-browser UA |
| `allow_local_hosts` | `ZOT_WEB_ALLOW_LOCAL_HOSTS` (comma-sep) | `localhost, 127.0.0.1, ::1` | SSRF escape hatch (see below); the config key replaces the default, the env var appends |

### User-Agent

Fetches identify themselves honestly as `zot-web/<version>` by default. Some
sites block or degrade content for non-browser clients; for those, the UA can
be overridden at three levels (most specific wins):

1. a per-call `user_agent` parameter on `web_fetch`, `web_fetch_raw`, and
   `web_fetch_image` — the model can retry a blocked page with
   `user_agent: "browser"`. An explicit per-call UA always forces a fresh
   fetch (bypassing the cached snapshot) so the retry actually hits the site;
2. the `user_agent` config setting / `ZOT_WEB_USER_AGENT` env var;
3. the built-in default.

The value `browser` (any case) expands to a current desktop-Chrome UA string;
anything else is sent literally.

**robots.txt policy.** Every fetch this extension makes is a single,
user-/model-initiated page retrieval — the moral equivalent of a person
opening the URL — so `robots.txt` is deliberately not consulted, and the
default UA identifies the client honestly instead. If a bulk/multi-page
crawl path is ever added, it must check `robots.txt` before fetching.
(This resolves the open question in the design doc: lenient for single
on-demand fetches, compliant for anything crawl-shaped.)

### `web_fetch` output

The output leads with a small metadata block so the model can tell a short page
from a truncated dense one:

```text
# Artificial intelligence
https://en.wikipedia.org/wiki/Artificial_intelligence
Content-Type: text/html; charset=UTF-8
Chars: 0-500 of 397898
Images: 17 (shown as [image:N]; resolve with web_images)

**Artificial intelligence** (AI) is the capability of …

…[397398 more chars; continue with offset=500]
```

- A `Final-URL:` line appears only when redirects landed somewhere other than
  the requested URL.
- `Chars: start-end of total` reports the returned window against the full
  rendered length. When `end < total`, the trailing hint gives the exact
  `offset` to pass to the next `web_fetch` call to keep reading — the page is
  already cached, so continuation costs no extra network request.
- Relative links and image sources are resolved against the **final** URL after
  redirects, so an `http→https` redirect doesn't leave stale links in the body.

RSS and Atom feeds (detected by content type or XML root element) render as a
per-entry list — title, date, link, summary — instead of raw XML, capped at
100 entries.

Pages in legacy encodings (windows-1252, Shift_JIS, GBK, …) are transcoded to
UTF-8 before rendering, using the `Content-Type` charset, the page's
`<meta charset>`, or content sniffing — in that order. `web_fetch_raw` still
returns the bytes exactly as served.

PDFs (by content type or `%PDF-` magic bytes) get their text layer extracted
and rendered with per-page markers through the normal paging pipeline. There
is no OCR: encrypted, malformed, or scanned image-only PDFs fall back to a
summary that suggests `web_fetch_raw` to save the file instead.

Other binary responses (images, octet-streams) are not dumped as raw bytes —
`web_fetch` returns a one-line summary like
`[image/png content, 40075 bytes — not rendered as text]` instead. Textual
types (`text/*`, JSON, XML, SVG) pass through normally.

readability drops `<table>` elements from article content, so data tables
(e.g. large sortable Wikipedia tables) are recovered separately and appended
under a `## Tables` heading, rendered leniently (cell text flattened, images
dropped, ragged rows padded). Each table is capped at 50 rows with a
truncation note; the dropped rows are not stored, so they are not reachable via
`offset`.

### Images and the page cache

By default `web_fetch` strips image URLs out of its Markdown, leaving a short
`[image:N: alt]` handle where each image was. This keeps long CDN URLs out of
the model's context. To get the actual links, the model calls `web_images(url)`,
which returns each handle's URL plus dimensions, the nearest `<figcaption>`
caption, and the enclosing source-page link (e.g. a Wikimedia `File:` page).

**The placeholder contract:** `[image:N]` in `web_fetch` maps to `[image:N]` in
`web_images` for the same URL. Ids are assigned in document order and are stable
for a cached page; identical image URLs are de-duplicated to a single id.

Every fetched page is cached (in memory, per the TTL/size settings above), so
`web_images`, `web_links`, and `web_fetch_raw` normally cost no network request.
If called for a URL that was never fetched (or whose cache entry expired), they
transparently fetch and render the page first — they do **not** error, so they
are safe to call directly. Set `fetch_inline_images: true` to restore inline
image URLs and disable the indexing (and the `web_images` workflow).

On pages readability can't reduce to an article (boards, forums, JS-heavy
SPAs), `web_fetch` falls back to a tag-stripper for the text, but `web_images`
still harvests image URLs from the whole document — so it returns results even
when no `[image:N]` placeholders appear inline (the `web_fetch` header notes
this with `not inlined; list URLs with web_images`). The cache also retains each
page's unrendered body (gzip-compressed) so `web_fetch_raw` can hand it back for
manual grepping without a second fetch.

The cache is bounded by **both** entry count (`fetch_cache_max_entries`) and
total retained bytes (`fetch_cache_max_bytes`), evicting least-recently-used
pages once either is exceeded — so a handful of large pages can't grow memory
without limit. Per page, the harvested link and image lists are themselves
capped (5000 links, 2000 images) so a link-farm page can't bloat one entry. The
cache is process-global: a page fetched once is served from cache to every
subsequent tool call in that extension process (it is single-user, so this is a
warm-cache win, not a cross-tenant concern).

## Fetching images for viewing (`web_fetch_image`)

`web_fetch`/`web_images` deal in image *URLs*; `web_fetch_image` retrieves the
image *bytes* and hands them to the model as a **native image content block** — the
model sees the picture, not a base64 blob. It accepts PNG, JPEG, GIF, and WebP
(detected by content-type, falling back to byte sniffing) and runs through the
same SSRF guard as `web_fetch`.

```text
web_fetch_image(url, max_dimension?, save_path?, overwrite?, inject?)
```

- **`max_dimension`** — downscale so the longest edge is at most this many
  pixels, preserving aspect ratio and never upscaling (CatmullRom resample).
  PNG/JPEG/GIF keep their format; WebP transcodes to PNG on resize (Go has no
  WebP encoder).
- **`save_path`** — write the (possibly resized) image into the workspace at
  this relative path. Writes are confined under the workspace: absolute paths
  and `..` escapes are refused, parent directories are created as needed, and an
  existing file is **not** overwritten unless **`overwrite: true`**.
- **`inject`** — defaults `true` (return the image for viewing). Set `false` for
  a token-free download when you only want the file on disk.

**Size limit and the resize loop.** An image whose encoded size exceeds
`fetch_image_max_bytes` (default 5 MiB, ≈ provider limits) is rejected with its
dimensions and a recommended `max_dimension` — the model then resubmits with
that value to bring it under the cap. The original is allowed to download past
the cap so it can be decoded and resized down. Decoded images are also capped at
40 million pixels before any full decode/resize to reject image decompression
bombs (a 25 MiB file can otherwise unpack into a multi-hundred-MiB pixel
buffer), and no more than three decode/resize operations run at once so a burst
of large images can't exhaust memory.

## The `/web-cache` command and status notes

`/web-cache` (a zot slash command, run by you rather than the model) lists the
cached pages — URL, size, age, title — and `/web-cache clear` empties the
cache, which is handy when a page changed and you want the model's next fetch
to see the live version before the TTL expires. The extension also pushes
one-shot status notes into the TUI (e.g. when a tool's rate limit trips) so
backoff is visible without digging through `$ZOT_HOME/logs/ext-web.log`.

## Security: SSRF protection + the local allowlist

Because the model chooses the URL, `web_fetch` is the main attack surface
(prompt-injected pages can try to make it hit internal services). By default it:

- allows `http`/`https` only;
- resolves the host and **refuses private/reserved/loopback/link-local,
  documentation, benchmarking, CGNAT, multicast, and other special-use
  addresses** — including the cloud metadata address `169.254.169.254`
  (loopback is exempted by the default allowlist below);
- dials the validated IP directly (closing the DNS-rebinding gap) and re-checks
  on every redirect; caps redirects, time, and response size;
- refuses a short list of well-known non-web service ports (SSH, SMTP, MySQL,
  Redis, RDP, …) outright, so the fetcher can't be steered into poking those
  services even on a public host.

The escape hatch is **`allow_local_hosts`**. It ships with loopback already
allowed — `["localhost", "127.0.0.1", "::1"]` — so locally hosted services (a
dev server, a local SearXNG) work without ceremony. To reach anything beyond
loopback, set the key in `config.json`; it **replaces** the default, so restate
the loopback entries alongside your additions:

```jsonc
"allow_local_hosts": [
  "localhost", "127.0.0.1", "::1",   // the shipped default
  "grafana.internal",                // a hostname on your LAN
  "192.168.1.0/24",                  // a home subnet
  "100.64.0.0/10"                    // e.g. a tailnet (CGNAT range)
]
```

Each entry is one of:

- a **hostname** — matched against the request host (e.g. `localhost`,
  `grafana.internal`). Hostname entries trust that name's DNS: any blocked-range
  IP the name resolves to is permitted;
- an **IP** — matched against the resolved address (e.g. `127.0.0.1`);
- a **CIDR** — matched against the resolved address (e.g. `192.168.1.0/24`).

An explicit `"allow_local_hosts": []` locks loopback back down for hardened
setups. The `ZOT_WEB_ALLOW_LOCAL_HOSTS` env var (comma-separated) *appends* to
whatever the file produced rather than replacing it.

This is a precise escape hatch, not an "allow all local" switch: only the
targets you list are exempted.

## Host integration (zot and terva)

This extension speaks the plain zot extension protocol, so it runs unchanged on
both zot and [terva](https://github.com/terva-sh/terva) (a zot-compatible fork).
On terva it also opts into two newer, additive niceties — both invisible to
stock zot, which simply ignores the extra fields.

**Read-only tools and approval modes.** The four reading tools — `web_search`,
`web_fetch`, `web_images`, `web_links` — advertise themselves as side-effect
free (a `read_only` hint on their registration). The two writing tools —
`web_fetch_raw` and `web_fetch_image` — do not, because they save files into the
workspace. terva's approval modes use that: in `--approval plan` the reading
tools stay available (so a planning/research session can browse) while the
writing tools are withheld, and in `--approval auto-edit` the reading tools run
without a prompt. On zot the hint is ignored and all six behave as before.

**A bundled research skill.** The repo ships `skills/web-research/SKILL.md`,
which terva discovers automatically once the extension is installed — a routine
for chaining search → read → links/images with citations. (zot does not load
extension-bundled skills; it's a no-op there.)

**Confirm-before-write, by default (terva).** The manifest ships a small,
restrict-only permission contribution: `web_fetch_raw` and `web_fetch_image`
default to **ask** before they run, because they write files into your
workspace. terva honors that even in `--approval yolo`, so installing the
extension can't quietly start writing files. An extension may only ever
*tighten* the policy this way (it can never `allow` itself a tool — only your
own config can grant), and your config wins: if you trust the writers, add an
`allow` to `$TERVA_HOME/config.json` and it overrides the manifest default —

```json
{
  "permissions": [
    { "tool": "web_fetch_raw",   "decision": "allow" },
    { "tool": "web_fetch_image", "decision": "allow" }
  ]
}
```

The four reading tools carry no manifest rule; they follow your approval mode
(allowed outright in `yolo`/`auto-edit`, prompted in `ask`). On zot the
`permissions` key is an unknown manifest field and is simply ignored.

## Roadmap

- [x] Replace the heuristic HTML extractor with readability +
      `JohannesKaufmann/html-to-markdown` (heuristic kept as a fallback).
      _(Uses the maintained `codeberg.org/readeck/go-readability/v2` fork —
      `go-shiori/go-readability` is now deprecated.)_
- [x] GFM table rendering + image indexing (`[image:N]` + `web_images`) with an
      in-memory page cache.
- [x] Broaden image discovery (lazy-load attrs, `<picture>`, `<a>`→image,
      `og:image`) + whole-page fallback for non-article pages; `web_links` for
      link enumeration; `web_fetch_raw` to dump unrendered source for manual
      grepping (raw body cached gzip-compressed).
- [x] Recover data tables that readability strips, rendered leniently under a
      `## Tables` section (row-capped). Tables land at the end, not inline.
- [x] Legacy-charset transcoding; PDF text-layer extraction; RSS/Atom feeds as
      structured entry lists; per-class HTTP error guidance with one transient
      retry; truncation caps surfaced in tool output.
- [x] Search filters (freshness, include/exclude domains, depth) + published
      dates; configurable User-Agent with `browser` alias and per-call
      override; `/web-cache` command and TUI status notes.
- [ ] Infobox / vertical key-value tables → cleaner key/value lists (irregular
      tables still degrade to spaced blocks today).
- [ ] More search backends (Brave, Serper, Exa) behind the same interface.
- [ ] Optional JS rendering fallback (e.g. Jina Reader) — deferred for now.
- [x] Prebuilt per-platform release archives (goreleaser, Forgejo CI): pushing
      a `v*` tag publishes linux/darwin/windows builds whose archives unpack
      into a ready-to-run extension dir (binary + `extension.json` + `run.sh`,
      no Go toolchain needed). CI runs gofmt/vet/race-tests plus a vendor-sync
      gate on every push, and a goreleaser snapshot on `main`.
- [ ] Teach `run.sh` / the install flow to consume those prebuilt archives, so
      a plain `zot ext install <git-url>` also skips the on-host build.
