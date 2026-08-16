package proxy

import (
	"crypto/rand"
	"strings"
)

// Anthropic identifiers are a fixed prefix, the literal generation marker "01",
// and 22 alphanumeric characters — e.g. msg_01XFDUDYJgAACzvnptvVoYEL. Clients
// and relay detectors key off the prefix, so a raw uuid.New().String() (36
// chars, hyphenated) is recognisably not an API-issued id.
const (
	anthropicIDBodyLen = 22
	anthropicIDCharset = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// 62*4 = 248. Bytes at or above this would map unevenly onto the charset,
	// so they are redrawn instead of folded in.
	anthropicIDRejectAbove = 248
)

// newAnthropicID builds prefix + "01" + 22 random alphanumerics.
func newAnthropicID(prefix string) string {
	var sb strings.Builder
	sb.Grow(len(prefix) + 2 + anthropicIDBodyLen)
	sb.WriteString(prefix)
	sb.WriteString("01")

	buf := make([]byte, anthropicIDBodyLen)
	for written := 0; written < anthropicIDBodyLen; {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand is documented never to fail on the platforms we build
			// for; if it somehow does, a short id is still better than a panic
			// in the middle of a response.
			for ; written < anthropicIDBodyLen; written++ {
				sb.WriteByte(anthropicIDCharset[0])
			}
			break
		}
		for _, b := range buf {
			if b >= anthropicIDRejectAbove {
				continue
			}
			sb.WriteByte(anthropicIDCharset[int(b)%len(anthropicIDCharset)])
			written++
			if written == anthropicIDBodyLen {
				break
			}
		}
	}
	return sb.String()
}

// newMessageID returns a message id shaped like msg_01XFDUDYJgAACzvnptvVoYEL.
func newMessageID() string { return newAnthropicID("msg_") }

// newToolUseID returns a client-tool id shaped like toolu_01T1x1fJ34qAmk2tNTrN.
func newToolUseID() string { return newAnthropicID("toolu_") }

// newServerToolUseID returns a server-tool id shaped like srvtoolu_01WYG3ziw53X.
func newServerToolUseID() string { return newAnthropicID("srvtoolu_") }

// toolInputChunkBytes is the target size of one input_json_delta fragment.
// Small enough that a client rendering a tool call sees it fill in, large
// enough that a big argument object does not turn into hundreds of frames.
const toolInputChunkBytes = 48

// chunkToolInputJSON splits a serialized tool-argument object into the
// partial_json fragments an input_json_delta stream carries.
//
// The upstream hands the arguments over only once they are complete and parse
// clean, so there is nothing to stream incrementally the way the Messages API
// does. Emitting the whole object as a single fragment is legal — a client
// concatenates the fragments and parses the result either way — but it means a
// tool call materialises all at once after a silent gap, with no progressive
// render. Splitting restores that without changing what the client assembles.
//
// Fragments break on rune boundaries: a multi-byte character cut in half would
// corrupt the reassembled JSON.
func chunkToolInputJSON(encoded string) []string {
	if encoded == "" {
		return nil
	}
	chunks := make([]string, 0, len(encoded)/toolInputChunkBytes+1)
	start := 0
	for i := range encoded {
		// i advances one rune at a time, so it is always a safe cut point.
		if i-start >= toolInputChunkBytes {
			chunks = append(chunks, encoded[start:i])
			start = i
		}
	}
	return append(chunks, encoded[start:])
}

// normalizeToolUseID keeps an upstream id when it already looks like one the
// Messages API would issue, and mints a replacement when it does not.
//
// The Kiro/CodeWhisperer backend hands back Bedrock-shaped ids (tooluse_…),
// which fail the toolu_ prefix rule every Anthropic client and relay detector
// applies. Rewriting is safe because the mapping never has to be reversed: each
// request carries the whole conversation, so the assistant turn that announced
// the call and the user turn that answers it both travel with the rewritten id.
func normalizeToolUseID(upstream string) string {
	if strings.HasPrefix(upstream, "toolu_") {
		return upstream
	}
	return newToolUseID()
}
