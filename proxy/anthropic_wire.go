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
//
// Sized against what the Messages API actually emits: fragments there follow
// the model's tokens, which land in the 2–20 byte range, so a typical argument
// object arrives over several frames. A larger window would deliver the common
// short object — {"location":"Paris","unit":"celsius"} is 37 bytes — as a
// single frame, which is exactly the all-at-once materialisation the chunking
// exists to avoid.
const toolInputChunkBytes = 16

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

// isAnthropicID reports whether id is exactly prefix + "01" + 22 alphanumerics.
//
// A prefix test alone is not enough. The Kiro/CodeWhisperer backend returns ids
// such as toolu_bdrk_01BZSE8P8Bm5SVnRJAUmFySz, which carries the right prefix
// and would pass any HasPrefix check while announcing the Bedrock backend in
// the middle of the id. The length and the alphabet are what actually separate
// an API-issued id from a look-alike.
func isAnthropicID(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || len(rest) != 2+anthropicIDBodyLen || rest[0] != '0' || rest[1] != '1' {
		return false
	}
	for i := 2; i < len(rest); i++ {
		c := rest[i]
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			continue
		}
		return false
	}
	return true
}

// normalizeToolUseID keeps an upstream id when it already looks like one the
// Messages API would issue, and mints a replacement when it does not.
//
// The Kiro/CodeWhisperer backend hands back Bedrock-shaped ids — bare
// tooluse_… as well as the toolu_bdrk_… form that mimics the Anthropic prefix.
// Rewriting is safe because the mapping never has to be reversed: each request
// carries the whole conversation, so the assistant turn that announced the call
// and the user turn that answers it both travel with the rewritten id.
func normalizeToolUseID(upstream string) string {
	if isAnthropicID(upstream, "toolu_") {
		return upstream
	}
	return newToolUseID()
}

// newRequestID returns the value of the request-id response header, which the
// Messages API sets on every reply and clients quote in bug reports.
func newRequestID() string { return newAnthropicID("req_") }
