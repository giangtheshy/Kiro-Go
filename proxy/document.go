package proxy

// Support for Anthropic `document` content blocks.
//
// The Kiro/CodeWhisperer upstream accepts text and images only — there is no
// field on the wire that can carry a PDF. So a document block has to be turned
// into text before the request leaves this proxy, or the attachment silently
// disappears and the model answers as though the user had sent nothing.
//
// That is what used to happen: the content-block switch in translator.go had no
// `document` case and no default, so the block fell through and was dropped
// without a log line. A turn carrying only a document then failed request
// validation with "at least one non-empty user message is required".

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"kiro-go/logger"
)

// logDocumentSkip records a document the proxy could not render. A dropped
// attachment is invisible in the answer, so it must at least be visible in the
// log — the previous behaviour left no trace at all.
func logDocumentSkip(reason string) {
	logger.Warnf("[Document] attachment not forwarded: %s", reason)
}

// maxDocumentTextBytes caps how much extracted text one document contributes.
// A large PDF can outweigh the whole rest of the conversation, and the payload
// truncator upstream of this would then start dropping history to make room.
const maxDocumentTextBytes = 400_000

// documentBlockText renders one `document` content block as plain text.
//
// Returns "" when the block carries nothing usable, so the caller can decide
// whether that leaves the message empty.
func documentBlockText(block map[string]interface{}) string {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return ""
	}

	title, _ := block["title"].(string)
	body := documentSourceText(source)
	if strings.TrimSpace(body) == "" {
		return ""
	}
	if len(body) > maxDocumentTextBytes {
		body = body[:maxDocumentTextBytes] + "\n[document truncated]"
	}

	// The context field is the caller's own note about the attachment, so it
	// travels with it.
	if ctx, _ := block["context"].(string); strings.TrimSpace(ctx) != "" {
		body = strings.TrimSpace(ctx) + "\n\n" + body
	}
	if strings.TrimSpace(title) != "" {
		return "<document title=\"" + strings.TrimSpace(title) + "\">\n" + body + "\n</document>"
	}
	return "<document>\n" + body + "\n</document>"
}

// documentSourceText handles the source shapes the Messages API defines:
// base64, text, and content. A url source is not fetched — see below.
func documentSourceText(source map[string]interface{}) string {
	mediaType, _ := source["media_type"].(string)
	if mediaType == "" {
		if mt, ok := source["mediaType"].(string); ok {
			mediaType = mt
		}
	}

	switch sourceType, _ := source["type"].(string); sourceType {
	case "text":
		data, _ := source["data"].(string)
		return data

	case "content":
		// A pre-chunked document: an array of text blocks.
		items, _ := source["content"].([]interface{})
		var parts []string
		for _, item := range items {
			switch v := item.(type) {
			case string:
				parts = append(parts, v)
			case map[string]interface{}:
				if text, ok := v["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")

	case "url":
		// Deliberately not fetched. Resolving a caller-supplied URL server-side
		// would let any API key use this proxy to reach hosts it cannot reach
		// itself, including cloud metadata endpoints on the loopback interface.
		logDocumentSkip("url sources are not fetched")
		return ""

	case "file":
		// Requires the Files API, which this proxy does not host.
		logDocumentSkip("file_id sources require the Files API")
		return ""
	}

	// base64, or a source that omitted `type` but carries data anyway.
	encoded, _ := source["data"].(string)
	if encoded == "" {
		return ""
	}
	raw, err := decodeFlexibleBase64(encoded)
	if err != nil {
		logDocumentSkip("source data is not valid base64")
		return ""
	}

	if strings.Contains(strings.ToLower(mediaType), "pdf") || bytes.HasPrefix(raw, []byte("%PDF")) {
		text, err := extractPDFText(raw)
		if err != nil {
			logDocumentSkip(err.Error())
			return ""
		}
		return text
	}

	// Anything else that is already legible as text (text/plain, text/markdown,
	// application/json, CSV…) passes through as-is.
	if utf8.Valid(raw) {
		return string(raw)
	}
	logDocumentSkip("unsupported media type " + strconv.Quote(mediaType))
	return ""
}

func decodeFlexibleBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, ";base64,"); idx >= 0 {
		s = s[idx+len(";base64,"):]
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if out, err := enc.DecodeString(s); err == nil {
			return out, nil
		}
	}
	return nil, fmt.Errorf("not base64")
}

// ---------------------------------------------------------------------------
// PDF text extraction
// ---------------------------------------------------------------------------

// extractPDFText pulls the visible text out of a PDF.
//
// This is a deliberately small extractor, not a PDF engine: it walks the file
// for stream objects, inflates the ones that are Flate-compressed, and reads the
// text-showing operators out of the content streams. That covers PDFs produced
// by the usual generators. It does not handle encrypted documents, and text
// drawn with a font whose glyphs do not map back to Latin-1 or UTF-16 will come
// out garbled — both are reported rather than silently returning empty.
func extractPDFText(raw []byte) (string, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(raw, " \r\n\t"), []byte("%PDF")) {
		return "", fmt.Errorf("not a PDF")
	}
	if bytes.Contains(raw, []byte("/Encrypt")) {
		return "", fmt.Errorf("encrypted PDFs are not supported")
	}

	var out strings.Builder
	for _, stream := range pdfStreams(raw) {
		text := pdfContentStreamText(stream)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(text)
		if out.Len() > maxDocumentTextBytes {
			break
		}
	}

	if strings.TrimSpace(out.String()) == "" {
		return "", fmt.Errorf("no extractable text (scanned image, or an unsupported font encoding)")
	}
	return out.String(), nil
}

// pdfStreams returns the decoded payload of every stream object in the file.
func pdfStreams(raw []byte) [][]byte {
	var streams [][]byte
	pos := 0
	for {
		rel := bytes.Index(raw[pos:], []byte("stream"))
		if rel < 0 {
			break
		}
		at := pos + rel
		// "endstream" also contains "stream"; skip those matches.
		if at >= 3 && bytes.Equal(raw[at-3:at], []byte("end")) {
			pos = at + len("stream")
			continue
		}

		body := at + len("stream")
		// The keyword is followed by CRLF or LF, never by CR alone.
		if body < len(raw) && raw[body] == '\r' {
			body++
		}
		if body < len(raw) && raw[body] == '\n' {
			body++
		}

		endRel := bytes.Index(raw[body:], []byte("endstream"))
		if endRel < 0 {
			break
		}
		end := body + endRel

		dict := pdfStreamDict(raw, at)
		if decoded, ok := pdfDecodeStream(dict, raw[body:end]); ok {
			streams = append(streams, decoded)
		}
		pos = end + len("endstream")
	}
	return streams
}

// pdfStreamDict returns the object dictionary preceding a stream keyword.
func pdfStreamDict(raw []byte, streamAt int) []byte {
	start := bytes.LastIndex(raw[:streamAt], []byte("<<"))
	if start < 0 {
		return nil
	}
	return raw[start:streamAt]
}

func pdfDecodeStream(dict, payload []byte) ([]byte, bool) {
	// Filters this extractor cannot read. Their payload is not text, so
	// forwarding the raw bytes would only inject noise.
	for _, unsupported := range [][]byte{
		[]byte("/DCTDecode"), []byte("/JPXDecode"), []byte("/CCITTFaxDecode"),
		[]byte("/JBIG2Decode"), []byte("/LZWDecode"), []byte("/RunLengthDecode"),
	} {
		if bytes.Contains(dict, unsupported) {
			return nil, false
		}
	}

	if !bytes.Contains(dict, []byte("/FlateDecode")) {
		return payload, true
	}

	payload = bytes.TrimLeft(payload, "\r\n")
	if zr, err := zlib.NewReader(bytes.NewReader(payload)); err == nil {
		defer zr.Close()
		if out, err := io.ReadAll(zr); err == nil && len(out) > 0 {
			return out, true
		}
	}
	// Some writers emit raw deflate with no zlib wrapper.
	fr := flate.NewReader(bytes.NewReader(payload))
	defer fr.Close()
	out, err := io.ReadAll(fr)
	if err != nil && len(out) == 0 {
		return nil, false
	}
	return out, true
}

// pdfContentStreamText reads the text-showing operators of a content stream.
//
// Only four operators put glyphs on the page: Tj and ' take one string, " takes
// a string after two numbers, and TJ takes an array of strings interleaved with
// kerning offsets. Everything else is positioning, graphics state, or drawing,
// and is skipped — but Td/TD/T*/ET move the cursor, so they become whitespace.
func pdfContentStreamText(stream []byte) string {
	var out strings.Builder
	var pending []string // strings collected since the last operator

	flush := func(sep string) {
		if len(pending) > 0 {
			out.WriteString(strings.Join(pending, ""))
			pending = pending[:0]
		}
		out.WriteString(sep)
	}

	for i := 0; i < len(stream); {
		switch c := stream[i]; {
		case c == '(':
			text, next := pdfLiteralString(stream, i)
			pending = append(pending, text)
			i = next

		case c == '<' && i+1 < len(stream) && stream[i+1] != '<':
			text, next := pdfHexString(stream, i)
			pending = append(pending, text)
			i = next

		case c == '%':
			for i < len(stream) && stream[i] != '\n' {
				i++
			}

		case isPDFRegular(c):
			start := i
			for i < len(stream) && isPDFRegular(stream[i]) {
				i++
			}
			switch string(stream[start:i]) {
			case "Tj", "TJ", "'", "\"":
				flush("")
			case "Td", "TD", "T*", "ET", "BT":
				flush("\n")
			case "Tc", "Tw", "TL", "Ts", "Tz", "Tf", "Tr", "Tm", "cm":
				// Positioning and font selection: drop any operands that were
				// strings (a font name is not page text).
				pending = pending[:0]
			}

		default:
			i++
		}
	}
	flush("")

	return collapsePDFWhitespace(out.String())
}

// isPDFRegular reports whether a byte can appear in a PDF token: everything
// except whitespace and the delimiter characters.
func isPDFRegular(c byte) bool {
	switch c {
	case 0, '\t', '\n', '\f', '\r', ' ',
		'(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return false
	}
	return true
}

// pdfLiteralString reads a (...) string starting at open, returning its decoded
// text and the index just past the closing paren.
func pdfLiteralString(b []byte, open int) (string, int) {
	var buf []byte
	depth := 1
	i := open + 1
	for i < len(b) {
		c := b[i]
		switch c {
		case '\\':
			i++
			if i >= len(b) {
				break
			}
			switch e := b[i]; e {
			case 'n':
				buf = append(buf, '\n')
			case 'r':
				buf = append(buf, '\r')
			case 't':
				buf = append(buf, '\t')
			case 'b':
				buf = append(buf, '\b')
			case 'f':
				buf = append(buf, '\f')
			case '\n':
				// Line continuation: the newline is not part of the string.
			case '\r':
				if i+1 < len(b) && b[i+1] == '\n' {
					i++
				}
			default:
				if e >= '0' && e <= '7' {
					val := 0
					digits := 0
					for digits < 3 && i < len(b) && b[i] >= '0' && b[i] <= '7' {
						val = val*8 + int(b[i]-'0')
						i++
						digits++
					}
					i--
					buf = append(buf, byte(val))
				} else {
					buf = append(buf, e)
				}
			}
			i++

		case '(':
			depth++
			buf = append(buf, c)
			i++

		case ')':
			depth--
			if depth == 0 {
				return decodePDFText(buf), i + 1
			}
			buf = append(buf, c)
			i++

		default:
			buf = append(buf, c)
			i++
		}
	}
	return decodePDFText(buf), i
}

// pdfHexString reads a <...> string starting at open.
func pdfHexString(b []byte, open int) (string, int) {
	end := bytes.IndexByte(b[open:], '>')
	if end < 0 {
		return "", len(b)
	}
	end += open

	var digits []byte
	for _, c := range b[open+1 : end] {
		if isHexDigit(c) {
			digits = append(digits, c)
		}
	}
	if len(digits)%2 == 1 {
		digits = append(digits, '0') // an odd final digit is padded with zero
	}

	buf := make([]byte, 0, len(digits)/2)
	for i := 0; i+1 < len(digits); i += 2 {
		hi, _ := strconv.ParseUint(string(digits[i:i+2]), 16, 8)
		buf = append(buf, byte(hi))
	}
	return decodePDFText(buf), end + 1
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// decodePDFText converts one PDF string's bytes to UTF-8. A leading byte-order
// mark means UTF-16BE; otherwise the bytes are PDFDocEncoding, which agrees
// with Latin-1 across the printable range.
func decodePDFText(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		units := make([]uint16, 0, (len(b)-2)/2)
		for i := 2; i+1 < len(b); i += 2 {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return string(utf16.Decode(units))
	}
	if utf8.Valid(b) {
		return string(b)
	}
	runes := make([]rune, 0, len(b))
	for _, c := range b {
		runes = append(runes, rune(c))
	}
	return string(runes)
}

// collapsePDFWhitespace tidies the operator-driven output: content streams
// produce a newline per text-positioning operator, which is far more than the
// document actually contains.
func collapsePDFWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimRight(line, " \t\r"); strings.TrimSpace(trimmed) != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, "\n")
}
