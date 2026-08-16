package proxy

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// captureReasoning runs one reasoningContentEvent frame through the stream
// parser and reports what reached the callbacks.
func captureReasoning(t *testing.T, payload map[string]interface{}) (text, signature string, sigCalls int) {
	t.Helper()

	stream := bytes.NewReader(awsEventStreamFrame(t, "reasoningContentEvent", payload))
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(chunk string, isThinking bool) {
			if isThinking {
				text += chunk
			}
		},
		OnReasoningSignature: func(sig string) {
			signature = sig
			sigCalls++
		},
	})
	if err != nil {
		t.Fatalf("parseEventStream: %v", err)
	}
	return text, signature, sigCalls
}

func TestReasoningSignatureFlatShape(t *testing.T) {
	text, sig, _ := captureReasoning(t, map[string]interface{}{
		"text":      "Let me work through this.",
		"signature": "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pk",
	})

	if text != "Let me work through this." {
		t.Fatalf("reasoning text not forwarded, got %q", text)
	}
	if sig != "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pk" {
		t.Fatalf("signature not forwarded, got %q", sig)
	}
}

func TestReasoningSignatureNestedShape(t *testing.T) {
	text, sig, _ := captureReasoning(t, map[string]interface{}{
		"reasoningText": map[string]interface{}{
			"text":      "Nested variant.",
			"signature": "EtkCClkIDRgCKkCwip+FOIHQC91mq6Pl",
		},
	})

	if text != "Nested variant." {
		t.Fatalf("nested reasoning text not forwarded, got %q", text)
	}
	if sig != "EtkCClkIDRgCKkCwip+FOIHQC91mq6Pl" {
		t.Fatalf("nested signature not forwarded, got %q", sig)
	}
}

// A reasoning frame may carry the attestation and no text at all. Reading the
// signature only when text is present would drop it.
func TestReasoningSignatureOnlyFrame(t *testing.T) {
	text, sig, calls := captureReasoning(t, map[string]interface{}{
		"signature": "EtkCClkIDRgCKkCwip+FOIHQC91mq6Pl",
	})

	if text != "" {
		t.Fatalf("expected no reasoning text, got %q", text)
	}
	if calls != 1 || sig == "" {
		t.Fatalf("signature-only frame must still deliver the signature (calls=%d sig=%q)", calls, sig)
	}
}

// The signature is an upstream artifact. When it is absent it must stay absent:
// a fabricated value is rejected the moment a client replays the block.
func TestReasoningWithoutSignatureFabricatesNothing(t *testing.T) {
	_, sig, calls := captureReasoning(t, map[string]interface{}{
		"text": "No attestation on this frame.",
	})

	if calls != 0 || sig != "" {
		t.Fatalf("no signature must be invented (calls=%d sig=%q)", calls, sig)
	}
}

// ---------------------------------------------------------------------------
// Identifier shapes
// ---------------------------------------------------------------------------

var anthropicIDPattern = regexp.MustCompile(`^(msg|toolu|srvtoolu)_01[0-9A-Za-z]{22}$`)

func TestGeneratedIDsMatchAnthropicShape(t *testing.T) {
	for _, id := range []string{newMessageID(), newToolUseID(), newServerToolUseID()} {
		if !anthropicIDPattern.MatchString(id) {
			t.Fatalf("id %q does not match prefix_01 + 22 alphanumerics", id)
		}
	}
}

func TestGeneratedIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 512)
	for i := 0; i < 512; i++ {
		id := newMessageID()
		if seen[id] {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = true
	}
}

func TestNormalizeToolUseIDKeepsAnthropicShapedIDs(t *testing.T) {
	if got := normalizeToolUseID("toolu_01T1x1fJ34qAmk2tNTrN7Up6"); got != "toolu_01T1x1fJ34qAmk2tNTrN7Up6" {
		t.Fatalf("an id already carrying the toolu_ prefix must pass through, got %q", got)
	}
}

// Bedrock hands back tooluse_-prefixed ids, which fail the prefix rule every
// Anthropic client applies.
func TestNormalizeToolUseIDRewritesBedrockShape(t *testing.T) {
	got := normalizeToolUseID("tooluse_Xk29fAqLTZ2bQ")
	if !strings.HasPrefix(got, "toolu_01") {
		t.Fatalf("bedrock-shaped id not rewritten, got %q", got)
	}
	if !anthropicIDPattern.MatchString(got) {
		t.Fatalf("rewritten id has the wrong shape: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Tool argument chunking
// ---------------------------------------------------------------------------

func TestChunkToolInputJSONReassembles(t *testing.T) {
	encoded := `{"city":"Tokyo","unit":"celsius","note":"a reasonably long value so the split actually happens"}`

	chunks := chunkToolInputJSON(encoded)
	if len(chunks) < 2 {
		t.Fatalf("expected the payload to be split, got %d chunk(s)", len(chunks))
	}
	if joined := strings.Join(chunks, ""); joined != encoded {
		t.Fatalf("chunks do not reassemble:\n got %q\nwant %q", joined, encoded)
	}
}

// Cutting mid-rune would corrupt the JSON a client rebuilds from the fragments.
func TestChunkToolInputJSONSplitsOnRuneBoundaries(t *testing.T) {
	encoded := `{"q":"` + strings.Repeat("日本語テキスト", 8) + `"}`

	chunks := chunkToolInputJSON(encoded)
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i, chunk)
		}
	}
	if joined := strings.Join(chunks, ""); joined != encoded {
		t.Fatalf("multibyte payload did not reassemble")
	}
}

func TestChunkToolInputJSONEmpty(t *testing.T) {
	if got := chunkToolInputJSON(""); got != nil {
		t.Fatalf("empty input should produce no fragments, got %v", got)
	}
}
