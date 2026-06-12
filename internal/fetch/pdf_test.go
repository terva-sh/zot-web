package fetch

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// minimalPDF builds a valid one-page PDF showing text, computing the xref
// table from the real object offsets.
func minimalPDF(text string) []byte {
	content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func TestRenderPDFExtractsText(t *testing.T) {
	body := minimalPDF("Hello from the zot-web PDF extractor")
	u, _ := url.Parse("https://example.com/doc.pdf")
	p := testClient().render(u, "application/pdf", body)
	if !strings.Contains(p.Markdown, "PDF document, 1 pages") {
		t.Errorf("missing PDF header:\n%.300s", p.Markdown)
	}
	if !strings.Contains(p.Markdown, "Hello from the zot-web PDF extractor") {
		t.Errorf("extracted text missing:\n%.500s", p.Markdown)
	}
}

func TestRenderPDFSniffsMagicBytes(t *testing.T) {
	// Served as octet-stream: detection must fall back to the %PDF- prefix.
	body := minimalPDF("Sniffed")
	u, _ := url.Parse("https://example.com/download")
	p := testClient().render(u, "application/octet-stream", body)
	if !strings.Contains(p.Markdown, "Sniffed") {
		t.Errorf("PDF behind octet-stream not extracted:\n%.300s", p.Markdown)
	}
}

func TestRenderPDFFallbackOnGarbage(t *testing.T) {
	u, _ := url.Parse("https://example.com/bad.pdf")
	p := testClient().render(u, "application/pdf", []byte("%PDF-1.4 this is not a real pdf"))
	if !strings.Contains(p.Markdown, "no extractable text layer") {
		t.Errorf("want graceful fallback summary, got:\n%.300s", p.Markdown)
	}
}
