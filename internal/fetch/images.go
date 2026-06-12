package fetch

import (
	"fmt"
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"
)

// imgSentinel is the token left in the DOM in place of an <img>; it survives
// Markdown conversion unescaped and is swapped for the `[image:N]` placeholder
// afterward (so neither the brackets nor the alt text get Markdown-escaped).
func imgSentinel(id int) string { return fmt.Sprintf(" {{IMG:%d}} ", id) }

// placeholder is what the model sees inline for an image: a short, stable
// handle plus the alt text when present.
func (im Image) placeholder() string {
	if im.Alt != "" {
		return fmt.Sprintf("[image:%d: %s]", im.ID, im.Alt)
	}
	return fmt.Sprintf("[image:%d]", im.ID)
}

// FormatImages renders the image list for web_images as a compact, model-
// readable list keyed by the same `[image:N]` handles that appear in the
// web_fetch output.
func FormatImages(pageURL string, imgs []Image) string {
	if len(imgs) == 0 {
		return fmt.Sprintf("No images found on %s.", pageURL)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d image(s) on %s:\n", len(imgs), pageURL)
	if len(imgs) >= maxImages {
		fmt.Fprintf(&b, "(list capped at %d — the page may contain more; use web_fetch_raw to inspect the full source)\n", maxImages)
	}
	for _, im := range imgs {
		fmt.Fprintf(&b, "\n[image:%d] %s", im.ID, im.URL)
		if d := im.dimensions(); d != "" {
			fmt.Fprintf(&b, " (%s)", d)
		}
		alt := oneLine(im.Alt)
		if alt != "" {
			fmt.Fprintf(&b, "\n   alt: %s", alt)
		}
		if cap := oneLine(im.Caption); cap != "" && cap != alt {
			fmt.Fprintf(&b, "\n   caption: %s", cap)
		}
		if im.SourcePage != "" {
			fmt.Fprintf(&b, "\n   source: %s", im.SourcePage)
		}
	}
	return strings.TrimSpace(b.String())
}

// dimensions renders "W×H" when both are known.
func (im Image) dimensions() string {
	if im.Width != "" && im.Height != "" {
		return im.Width + "×" + im.Height
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// maxImages bounds how many images the harvesters retain, keeping a gallery- or
// sprite-heavy page from bloating the cache entry and the model's context.
const maxImages = 2000

// indexImages walks root, replacing each <img> with a sentinel text node and
// collecting the (absolute) image URLs. Identical URLs share one id. base is
// the page URL, used to resolve relative srcs.
func indexImages(root *xhtml.Node, base *url.URL) []Image {
	var images []Image
	byURL := map[string]int{}

	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		// Capture NextSibling before touching the node, since replacing an
		// <img> detaches it from the sibling chain.
		for ch := n.FirstChild; ch != nil; {
			next := ch.NextSibling
			if ch.Type == xhtml.ElementNode && ch.Data == "img" {
				if abs := imgURL(ch, base); abs != "" {
					id, ok := byURL[abs]
					if !ok && len(images) < maxImages {
						id = len(images) + 1
						byURL[abs] = id
						// Read surrounding metadata before the node is detached.
						images = append(images, Image{
							ID:         id,
							URL:        abs,
							Alt:        strings.TrimSpace(attrVal(ch, "alt")),
							Caption:    enclosingCaption(ch),
							Width:      strings.TrimSpace(attrVal(ch, "width")),
							Height:     strings.TrimSpace(attrVal(ch, "height")),
							SourcePage: enclosingLink(ch, base),
						})
						ok = true
					}
					// Only placeholder images we actually indexed; once the cap is
					// hit, leave the overflow <img> untouched (no stray sentinel).
					if ok {
						replaceWithSentinel(ch, id)
					}
				}
				// <img> is void; nothing to recurse into.
			} else {
				walk(ch)
			}
			ch = next
		}
	}
	walk(root)
	return images
}

// applyPlaceholders swaps each image's sentinel for its `[image:N]` placeholder.
func applyPlaceholders(md string, images []Image) string {
	for _, im := range images {
		md = strings.ReplaceAll(md, strings.TrimSpace(imgSentinel(im.ID)), im.placeholder())
	}
	return md
}

// imgURL resolves an <img>'s best source to an absolute URL, skipping inline
// data: URIs. A real `src` wins; otherwise it falls back to the common
// lazy-load attributes (data-src and friends) and finally the first srcset /
// data-srcset candidate — covering the many sites that defer image loading to
// JavaScript but still leave the real URL in the markup.
func imgURL(n *xhtml.Node, base *url.URL) string {
	src := strings.TrimSpace(attrVal(n, "src"))
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		// Lazy-load fallbacks, in rough order of prevalence.
		for _, key := range []string{"data-src", "data-original", "data-lazy-src", "data-url"} {
			if v := strings.TrimSpace(attrVal(n, key)); v != "" && !strings.HasPrefix(strings.ToLower(v), "data:") {
				src = v
				break
			}
		}
	}
	if src == "" {
		src = firstSrcset(attrVal(n, "srcset"))
	}
	if src == "" {
		src = firstSrcset(attrVal(n, "data-srcset"))
	}
	return resolveImageRef(src, base)
}

// resolveImageRef resolves a raw image reference against base, skipping empty
// and data: URIs. Returns "" when there's nothing usable.
func resolveImageRef(src string, base *url.URL) string {
	src = strings.TrimSpace(src)
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		return ""
	}
	ref, err := url.Parse(src)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// imageExts are the file extensions we treat as a direct image link when an
// <a href> points straight at one (e.g. a thumbnail linking to the full image,
// as on imageboards).
var imageExts = []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg", ".avif"}

// collectImages gathers every image referenced anywhere under root, without
// mutating the tree (unlike indexImages). It is the whole-page fallback used
// when readability's article subtree yields no images, or when extraction fell
// back to the heuristic stripper — so board/forum/SPA pages aren't a dead end.
// It pulls from <img> (including lazy-load attrs), <picture>'s <source srcset>,
// <a href> pointing directly at an image file, and social/meta image tags.
func collectImages(root *xhtml.Node, base *url.URL) []Image {
	var images []Image
	byURL := map[string]bool{}
	add := func(abs string, im Image) {
		if abs == "" || byURL[abs] || len(images) >= maxImages {
			return
		}
		byURL[abs] = true
		im.ID = len(images) + 1
		im.URL = abs
		images = append(images, im)
	}

	// Social/meta images first: they're the server-rendered URL many JS-driven
	// pages expose even when the gallery itself is built client-side.
	for _, u := range metaImageURLs(root, base) {
		add(u, Image{})
	}

	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.Data {
			case "img":
				add(imgURL(n, base), Image{
					Alt:        strings.TrimSpace(attrVal(n, "alt")),
					Caption:    enclosingCaption(n),
					Width:      strings.TrimSpace(attrVal(n, "width")),
					Height:     strings.TrimSpace(attrVal(n, "height")),
					SourcePage: enclosingLink(n, base),
				})
			case "source": // <picture><source srcset>
				src := firstSrcset(attrVal(n, "srcset"))
				if src == "" {
					src = firstSrcset(attrVal(n, "data-srcset"))
				}
				add(resolveImageRef(src, base), Image{})
			case "a":
				add(imageHref(n, base), Image{Alt: oneLine(nodeText(n))})
			case "noscript":
				// With scripting assumed on (x/net/html's default), <noscript>
				// content parses as one opaque text node — but it's where
				// lazy-load setups put their real <img> fallback. Re-parse it.
				if frag, err := xhtml.Parse(strings.NewReader(nodeText(n))); err == nil {
					walk(frag)
				}
			}
			// CSS background-image lazy-loaders stash the URL in data-bg-style
			// attributes on arbitrary elements (usually <div>).
			for _, key := range []string{"data-bg", "data-background", "data-background-image"} {
				if v := strings.TrimSpace(attrVal(n, key)); v != "" {
					add(resolveImageRef(stripCSSURL(v), base), Image{})
				}
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(root)
	return images
}

// stripCSSURL unwraps a `url(...)` value (some lazy-loaders store the full CSS
// function, others the bare URL).
func stripCSSURL(v string) string {
	s := strings.TrimSpace(v)
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "url(") && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[4 : len(s)-1])
		s = strings.Trim(s, `'"`)
	}
	return s
}

// imageHref returns the absolute href of an <a> that points straight at an
// image file, else "". (Imageboard thumbnails link to the full-res image this
// way, so the full image is recoverable even when only a thumbnail <img> shows.)
func imageHref(a *xhtml.Node, base *url.URL) string {
	abs := resolveHref(attrVal(a, "href"), base)
	if abs == "" {
		return ""
	}
	p, err := url.Parse(abs)
	if err != nil {
		return ""
	}
	path := strings.ToLower(p.Path)
	for _, ext := range imageExts {
		if strings.HasSuffix(path, ext) {
			return abs
		}
	}
	return ""
}

// metaImageURLs collects the page's social/preview image URLs from
// og:image, twitter:image, and <link rel="image_src">.
func metaImageURLs(root *xhtml.Node, base *url.URL) []string {
	var out []string
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.Data {
			case "meta":
				prop := strings.ToLower(strings.TrimSpace(attrVal(n, "property")))
				name := strings.ToLower(strings.TrimSpace(attrVal(n, "name")))
				if prop == "og:image" || prop == "og:image:url" || prop == "og:image:secure_url" ||
					name == "twitter:image" || name == "twitter:image:src" {
					if u := resolveImageRef(attrVal(n, "content"), base); u != "" {
						out = append(out, u)
					}
				}
			case "link":
				if strings.Contains(strings.ToLower(attrVal(n, "rel")), "image_src") {
					if u := resolveImageRef(attrVal(n, "href"), base); u != "" {
						out = append(out, u)
					}
				}
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(root)
	return out
}

// firstSrcset returns the first URL from a srcset attribute (ignoring its
// width/density descriptor).
func firstSrcset(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	first := strings.SplitN(s, ",", 2)[0]
	if fields := strings.Fields(first); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func attrVal(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// ancestorElement returns the nearest ancestor element named name, searching up
// to maxUp levels up (so we don't latch onto a page-wide wrapper).
func ancestorElement(n *xhtml.Node, name string, maxUp int) *xhtml.Node {
	for p := n.Parent; p != nil && maxUp > 0; p, maxUp = p.Parent, maxUp-1 {
		if p.Type == xhtml.ElementNode && p.Data == name {
			return p
		}
	}
	return nil
}

// enclosingLink returns the absolute href of the nearest wrapping <a>, if any
// (commonly a Wikimedia File: page). Skips fragment and data: links.
func enclosingLink(n *xhtml.Node, base *url.URL) string {
	a := ancestorElement(n, "a", 3)
	if a == nil {
		return ""
	}
	href := strings.TrimSpace(attrVal(a, "href"))
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "data:") {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// enclosingCaption returns the text of the nearest enclosing <figure>'s
// <figcaption>, collapsed to a single line.
func enclosingCaption(n *xhtml.Node) string {
	fig := ancestorElement(n, "figure", 4)
	if fig == nil {
		return ""
	}
	cap := firstDescendant(fig, "figcaption")
	if cap == nil {
		return ""
	}
	return strings.Join(strings.Fields(nodeText(cap)), " ")
}

// firstDescendant returns the first descendant element named name (depth-first).
func firstDescendant(n *xhtml.Node, name string) *xhtml.Node {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == xhtml.ElementNode && ch.Data == name {
			return ch
		}
		if found := firstDescendant(ch, name); found != nil {
			return found
		}
	}
	return nil
}

// nodeText concatenates all text under n.
func nodeText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

// replaceWithSentinel swaps node for a text node carrying its image sentinel.
func replaceWithSentinel(node *xhtml.Node, id int) {
	parent := node.Parent
	if parent == nil {
		return
	}
	parent.InsertBefore(&xhtml.Node{Type: xhtml.TextNode, Data: imgSentinel(id)}, node)
	parent.RemoveChild(node)
}
