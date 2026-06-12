package fetch

import (
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

func firstTable(t *testing.T, html string) *xhtml.Node {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	var found *xhtml.Node
	var w func(*xhtml.Node)
	w = func(n *xhtml.Node) {
		if found != nil {
			return
		}
		if n.Type == xhtml.ElementNode && n.Data == "table" {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w(c)
		}
	}
	w(doc)
	if found == nil {
		t.Fatal("no table found")
	}
	return found
}

func TestExtractDataTables(t *testing.T) {
	html := `<html><body>
<table class="wikitable sortable">
<caption>Populations</caption>
<tr><th>Country</th><th>% <span>of</span><br><span>world</span></th></tr>
<tr><td><img src="india-flag.png" alt="flag">India</td><td>17%</td></tr>
<tr><td>China</td><td>17%</td></tr>
<tr><td>USA</td><td>4%</td></tr>
</table></body></html>`
	got := extractDataTables([]byte(html))

	for _, want := range []string{"## Tables", "### Populations", "| Country |", "| --- | --- |", "| India | 17% |", "| China | 17% |"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "% of world") {
		t.Errorf("split header cell not space-joined (expected '%% of world'):\n%s", got)
	}
	if strings.Contains(got, "india-flag.png") {
		t.Errorf("image leaked into table cell:\n%s", got)
	}
}

func TestExtractDataTablesSkipsInfobox(t *testing.T) {
	html := `<html><body>
<table class="infobox">
<tr><th>Born</th><td>1900</td></tr>
<tr><th>Died</th><td>1980</td></tr>
<tr><th>Job</th><td>Scientist</td></tr>
</table></body></html>`
	if got := extractDataTables([]byte(html)); got != "" {
		t.Errorf("infobox should not be extracted as a data table, got:\n%s", got)
	}
}

func TestRenderTableRowCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<table class="wikitable"><tr><th>N</th><th>V</th></tr>`)
	for range 5 {
		sb.WriteString("<tr><td>r</td><td>x</td></tr>")
	}
	sb.WriteString("</table>")
	tbl := firstTable(t, "<html><body>"+sb.String()+"</body></html>")

	md := renderTableMarkdown(tbl, 2)
	if !strings.Contains(md, "truncated to the first 2 of 5 rows") {
		t.Errorf("missing truncation note:\n%s", md)
	}
	// header + separator + 2 data rows = 4 pipe lines
	pipes := 0
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "|") {
			pipes++
		}
	}
	if pipes != 4 {
		t.Errorf("expected 4 pipe lines (header+sep+2 rows), got %d:\n%s", pipes, md)
	}
}
