package fetch

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDFs are a constant target of agent fetches but used to yield only a
// "[application/pdf content, N bytes — not rendered as text]" stub. renderPDF
// extracts the text layer (no OCR: scanned image-only PDFs still come up
// empty) and feeds it through the normal metadata/paging pipeline.

func isPDF(contentType string, body []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "application/pdf" || bytes.HasPrefix(body, []byte("%PDF-"))
}

// renderPDF extracts a PDF's text layer page by page. ok is false when nothing
// could be extracted (encrypted, malformed, or image-only documents), in which
// case the caller falls back to the binary summary.
func renderPDF(body []byte) (p page, ok bool) {
	text, pages, err := extractPDFText(body)
	if err != nil || text == "" {
		return page{}, false
	}
	header := fmt.Sprintf("[PDF document, %d pages — extracted text layer]\n\n", pages)
	return cappedPage(page{}, header+text), true
}

// extractPDFText walks every page and concatenates its plain text under a
// per-page marker. The pdf library panics on some malformed inputs, so the
// whole walk runs under a recover.
func extractPDFText(body []byte) (text string, pages int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse panic: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", 0, err
	}
	pages = r.NumPage()
	var b strings.Builder
	for i := 1; i <= pages; i++ {
		pg := r.Page(i)
		if pg.V.IsNull() {
			continue
		}
		t, perr := pg.GetPlainText(nil)
		t = strings.TrimSpace(t)
		if perr != nil || t == "" {
			continue
		}
		fmt.Fprintf(&b, "--- page %d ---\n\n%s\n\n", i, t)
	}
	return strings.TrimSpace(b.String()), pages, nil
}
