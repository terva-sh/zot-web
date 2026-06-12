package fetch

import (
	"bytes"
	"fmt"
	"strings"

	xhtml "golang.org/x/net/html"
)

// go-readability strips <table> elements from article content (keeping only the
// cell text), so data-heavy pages lose their tables. And html-to-markdown's
// table plugin only grids "regular" tables — it linearizes complex real-world
// ones (multi-line headers, images in cells, ragged rows). extractDataTables
// recovers them: it re-parses the raw body, finds the real data tables, and
// renders each with a lenient row/cell walker that tolerates that messiness.
// The result is appended under a "## Tables" heading when the main render
// produced no table of its own.
const (
	maxTableRows = 50 // per-table data-row cap (rendered output, not re-fetchable)
	maxTables    = 12 // safety cap on how many tables to append
)

// hasMarkdownTable reports whether md already contains a GFM table (a header
// separator row like `| --- | --- |`), so we don't duplicate tables that did
// survive the main render.
func hasMarkdownTable(md string) bool {
	for _, line := range strings.Split(md, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "|") && strings.Contains(l, "-") && strings.Trim(l, "|-: ") == "" {
			return true
		}
	}
	return false
}

// extractDataTables parses body, renders each data table to Markdown, and
// returns a "## Tables" section (or "" if there are none worth showing).
func extractDataTables(body []byte) string {
	doc, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}

	var tables []*xhtml.Node
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "table" {
			if isDataTable(n) {
				tables = append(tables, n)
				return // don't descend into nested tables
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)
	if len(tables) == 0 {
		return ""
	}

	var b strings.Builder
	n := 0
	for _, t := range tables {
		if n >= maxTables {
			break
		}
		md := renderTableMarkdown(t, maxTableRows)
		if md == "" {
			continue
		}
		n++
		if cap := tableCaption(t); cap != "" {
			fmt.Fprintf(&b, "\n### %s\n\n%s\n", cap, md)
		} else {
			fmt.Fprintf(&b, "\n### Table %d\n\n%s\n", n, md)
		}
	}
	if n == 0 {
		return ""
	}
	return "\n\n## Tables\n" + b.String()
}

// isDataTable distinguishes a genuine data table from layout/infobox/navbox
// chrome: it must have header cells, a few rows, and several cells, and must not
// be a known non-data table class or a presentation table.
func isDataTable(t *xhtml.Node) bool {
	switch strings.ToLower(attrVal(t, "role")) {
	case "presentation", "none":
		return false
	}
	class := strings.ToLower(attrVal(t, "class"))
	for _, bad := range []string{"infobox", "navbox", "vertical-navbox", "sidebar", "metadata", "ambox", "mbox", "toccolours", "plainlinks"} {
		if strings.Contains(class, bad) {
			return false
		}
	}
	return countTag(t, "th") > 0 && countTag(t, "tr") >= 3 && countTag(t, "td") >= 4
}

// renderTableMarkdown renders a <table> as a GFM pipe table, leniently: the
// first row is the header, each row's cells are flattened to single-line text
// (images contribute nothing, so flag icons drop out), and ragged rows are
// padded to the widest row. Data rows beyond maxRows are dropped with a note.
func renderTableMarkdown(t *xhtml.Node, maxRows int) string {
	var rows [][]string
	var trs []*xhtml.Node
	var collect func(n *xhtml.Node)
	collect = func(n *xhtml.Node) {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			switch {
			case ch.Type == xhtml.ElementNode && ch.Data == "tr":
				trs = append(trs, ch)
			case ch.Type == xhtml.ElementNode && ch.Data == "table":
				// skip nested tables
			default:
				collect(ch)
			}
		}
	}
	collect(t)

	for _, tr := range trs {
		var cells []string
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == xhtml.ElementNode && (c.Data == "td" || c.Data == "th") {
				cells = append(cells, cellText(c))
			}
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}
	if len(rows) < 2 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols < 2 {
		return ""
	}

	row := func(cells []string) string {
		padded := make([]string, cols)
		copy(padded, cells)
		return "| " + strings.Join(padded, " | ") + " |"
	}
	var b strings.Builder
	b.WriteString(row(rows[0]) + "\n")
	b.WriteString("|" + strings.Repeat(" --- |", cols) + "\n")
	data := rows[1:]
	total := len(data)
	if total > maxRows {
		data = data[:maxRows]
	}
	for _, r := range data {
		b.WriteString(row(r) + "\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if total > maxRows {
		out += fmt.Sprintf("\n\n*(table truncated to the first %d of %d rows; the rest are not reachable via offset — use web_fetch_raw for the full table)*", maxRows, total)
	}
	return out
}

// cellText flattens a cell to single-line text, joining text from separate
// child elements with a space (so "% of"+"world" doesn't glue to "% ofworld")
// and escaping pipes. Images contribute nothing, dropping flag icons etc.
func cellText(n *xhtml.Node) string {
	var parts []string
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				parts = append(parts, t)
			}
			return
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	s := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// tableCaption returns the table's <caption> text, if any.
func tableCaption(t *xhtml.Node) string {
	cap := firstDescendant(t, "caption")
	if cap == nil {
		return ""
	}
	return strings.Join(strings.Fields(nodeText(cap)), " ")
}

// countTag counts descendant elements named name.
func countTag(n *xhtml.Node, name string) int {
	c := 0
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == name {
			c++
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return c
}
