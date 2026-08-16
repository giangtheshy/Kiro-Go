package proxy

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// buildPDF assembles a one-page PDF around the given content stream, optionally
// Flate-compressing it the way real generators do.
func buildPDF(t *testing.T, contentStream string, compress bool) []byte {
	t.Helper()

	payload := []byte(contentStream)
	filter := ""
	if compress {
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			t.Fatalf("compressing content stream: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("closing zlib writer: %v", err)
		}
		payload = buf.Bytes()
		filter = " /Filter /FlateDecode"
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	pdf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")
	fmt.Fprintf(&pdf, "4 0 obj\n<< /Length %d%s >>\nstream\n", len(payload), filter)
	pdf.Write(payload)
	pdf.WriteString("\nendstream\nendobj\n")
	pdf.WriteString("trailer\n<< /Root 1 0 R /Size 5 >>\n%%EOF\n")
	return pdf.Bytes()
}

func documentBlock(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	return map[string]interface{}{
		"type": "document",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "application/pdf",
			"data":       base64.StdEncoding.EncodeToString(raw),
		},
	}
}

func TestExtractPDFTextUncompressed(t *testing.T) {
	pdf := buildPDF(t, "BT /F1 12 Tf 72 720 Td (VERIFY-8842) Tj ET", false)

	got, err := extractPDFText(pdf)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "VERIFY-8842") {
		t.Fatalf("magic string missing from extracted text: %q", got)
	}
}

func TestExtractPDFTextFlateCompressed(t *testing.T) {
	pdf := buildPDF(t, "BT /F1 12 Tf 72 720 Td (VERIFY-8842) Tj ET", true)

	got, err := extractPDFText(pdf)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "VERIFY-8842") {
		t.Fatalf("magic string missing from extracted text: %q", got)
	}
}

// TJ splits a run across array elements with kerning numbers in between; the
// pieces have to be rejoined without the numbers leaking in.
func TestExtractPDFTextTJArrayRejoinsSplitRun(t *testing.T) {
	pdf := buildPDF(t, "BT /F1 12 Tf 72 720 Td [(VER) -20 (IFY) -15 (-8842)] TJ ET", false)

	got, err := extractPDFText(pdf)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "VERIFY-8842") {
		t.Fatalf("split run not rejoined: %q", got)
	}
}

func TestExtractPDFTextHexStringAndEscapes(t *testing.T) {
	// <564552494659> is "VERIFY"; the literal uses an octal escape and an
	// escaped paren.
	pdf := buildPDF(t, `BT <564552494659> Tj (\055\1018842 \(ok\)) Tj ET`, false)

	got, err := extractPDFText(pdf)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "VERIFY") {
		t.Fatalf("hex string not decoded: %q", got)
	}
	if !strings.Contains(got, "-A8842") {
		t.Fatalf("octal escapes not decoded: %q", got)
	}
	if !strings.Contains(got, "(ok)") {
		t.Fatalf("escaped parens not decoded: %q", got)
	}
}

// A font name is an operand of Tf, not page text, and must not survive.
func TestExtractPDFTextDropsFontOperands(t *testing.T) {
	pdf := buildPDF(t, "BT /F1 12 Tf (Body text) Tj ET", false)

	got, err := extractPDFText(pdf)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if strings.Contains(got, "F1") {
		t.Fatalf("font name leaked into text: %q", got)
	}
}

func TestExtractPDFTextRejectsEncrypted(t *testing.T) {
	pdf := append(buildPDF(t, "BT (hi) Tj ET", false), []byte("\n/Encrypt 9 0 R\n")...)

	if _, err := extractPDFText(pdf); err == nil {
		t.Fatal("an encrypted PDF must report why it cannot be read, not return empty text")
	}
}

// A scanned page carries an image, not a text operator. Reporting that as an
// error is what keeps it out of the "silently dropped" bucket.
func TestExtractPDFTextReportsNoTextLayer(t *testing.T) {
	pdf := buildPDF(t, "q 100 0 0 100 72 720 cm /Im1 Do Q", false)

	if _, err := extractPDFText(pdf); err == nil {
		t.Fatal("a PDF with no text layer must return an error")
	}
}

func TestDocumentBlockTextWrapsExtractedPDF(t *testing.T) {
	block := documentBlock(t, buildPDF(t, "BT (VERIFY-8842) Tj ET", true))
	block["title"] = "report.pdf"

	got := documentBlockText(block)
	if !strings.Contains(got, "VERIFY-8842") {
		t.Fatalf("document text missing: %q", got)
	}
	if !strings.Contains(got, `<document title="report.pdf">`) {
		t.Fatalf("title not carried through: %q", got)
	}
}

func TestDocumentBlockTextPlainTextSource(t *testing.T) {
	got := documentBlockText(map[string]interface{}{
		"type": "document",
		"source": map[string]interface{}{
			"type":       "text",
			"media_type": "text/plain",
			"data":       "VERIFY-8842",
		},
	})
	if !strings.Contains(got, "VERIFY-8842") {
		t.Fatalf("text source not rendered: %q", got)
	}
}

func TestDocumentBlockTextBase64PlainText(t *testing.T) {
	got := documentBlockText(map[string]interface{}{
		"type": "document",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "text/plain",
			"data":       base64.StdEncoding.EncodeToString([]byte("VERIFY-8842")),
		},
	})
	if !strings.Contains(got, "VERIFY-8842") {
		t.Fatalf("base64 text source not rendered: %q", got)
	}
}

// Fetching a caller-supplied URL server-side would turn the proxy into an SSRF
// vector, so a url source yields nothing rather than a request.
func TestDocumentBlockTextDoesNotFetchURLSource(t *testing.T) {
	got := documentBlockText(map[string]interface{}{
		"type": "document",
		"source": map[string]interface{}{
			"type": "url",
			"url":  "http://169.254.169.254/latest/meta-data/",
		},
	})
	if got != "" {
		t.Fatalf("url source must not be resolved, got %q", got)
	}
}

// The whole point of the change: a document block used to fall through the
// content switch and vanish, taking the request's only user content with it.
func TestExtractClaudeUserContentCarriesDocument(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "What code is in this file?"},
		documentBlock(t, buildPDF(t, "BT (VERIFY-8842) Tj ET", true)),
	}

	text, _, _ := extractClaudeUserContent(content)
	if !strings.Contains(text, "VERIFY-8842") {
		t.Fatalf("document content dropped: %q", text)
	}
	if !strings.Contains(text, "What code is in this file?") {
		t.Fatalf("sibling text block lost: %q", text)
	}
}

func TestValidateClaudeRequestAcceptsDocumentOnlyTurn(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 64,
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: []interface{}{documentBlock(t, buildPDF(t, "BT (VERIFY-8842) Tj ET", true))},
		}},
	}

	if msg := validateClaudeRequestShape(req); msg != "" {
		t.Fatalf("a turn carrying only a document is valid, got %q", msg)
	}
}
