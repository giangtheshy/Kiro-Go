package proxy

// Structured outputs (`output_config.format`) on a backend that has no such
// parameter.
//
// Anthropic constrains decoding to a JSON Schema and returns the result as an
// ordinary text block. Kiro/CodeWhisperer exposes no equivalent knob, and an
// unrecognised top-level field is simply ignored by encoding/json — so the
// request used to succeed and come back as free prose, which is the worst of
// the three possible outcomes: the caller gets a 200 and unparseable output.
//
// The one constraint mechanism the upstream does honour is a tool's input
// schema. So the schema is handed over as a synthetic single-purpose tool, the
// model is forced to call it, and the arguments it produces are unwrapped back
// into the text block the caller asked for. The client never sees the tool: it
// is not in the request it sent and it is not in the response it gets.

import (
	"encoding/json"
	"strings"
)

// structuredOutputToolName is the synthetic tool the schema travels on. The
// leading namespace makes a collision with a caller's own tool implausible.
const structuredOutputToolName = "respond_with_structured_output"

type structuredOutputSpec struct {
	// Schema is the caller's JSON Schema, used verbatim as the tool's
	// input_schema.
	Schema map[string]interface{}
}

// parseStructuredOutput reads output_config.format off a request.
//
// Both the current nesting (output_config.format) and the earlier flat spelling
// (output_format) are accepted, because clients pinned to the beta still send
// the old one.
func parseStructuredOutput(raw map[string]interface{}) *structuredOutputSpec {
	format := structuredOutputFormat(raw)
	if format == nil {
		return nil
	}
	if kind, _ := format["type"].(string); kind != "" && kind != "json_schema" {
		// "text" is the default and means no constraint.
		return nil
	}
	schema, _ := format["schema"].(map[string]interface{})
	if len(schema) == 0 {
		return nil
	}
	return &structuredOutputSpec{Schema: schema}
}

func structuredOutputFormat(raw map[string]interface{}) map[string]interface{} {
	if cfg, ok := raw["output_config"].(map[string]interface{}); ok {
		if format, ok := cfg["format"].(map[string]interface{}); ok {
			return format
		}
	}
	if format, ok := raw["output_format"].(map[string]interface{}); ok {
		return format
	}
	return nil
}

// tool renders the spec as the tool definition sent upstream.
func (s *structuredOutputSpec) tool() ClaudeTool {
	return ClaudeTool{
		Name: structuredOutputToolName,
		Description: "Return the final answer. Every field must satisfy the " +
			"provided schema exactly. Do not answer in prose.",
		InputSchema: s.Schema,
	}
}

// applyToRequest swaps the caller's tool configuration for the synthetic one.
//
// Returns false when the request already carries tools: forcing the schema tool
// would then suppress the caller's own, and letting both through means the model
// may answer with either. Callers fall back to leaving the request untouched, so
// tools keep working and only the schema constraint is lost.
func (s *structuredOutputSpec) applyToRequest(req *ClaudeRequest) bool {
	if s == nil || len(req.Tools) > 0 {
		return false
	}
	req.Tools = []ClaudeTool{s.tool()}
	req.ToolChoice = map[string]interface{}{
		"type": "tool",
		"name": structuredOutputToolName,
	}
	return true
}

// instruction renders the schema as a plain requirement for the system prompt.
//
// This is the weaker of the two mechanisms — the model can ignore prose in a
// way it cannot ignore a tool's input_schema — but it is the only one available
// once the caller has tools of its own, and an instruction the model usually
// follows beats dropping the constraint outright.
func (s *structuredOutputSpec) instruction() string {
	encoded, err := json.Marshal(s.Schema)
	if err != nil {
		return ""
	}
	return "Your final reply must be a single JSON value that validates against " +
		"this JSON Schema:\n" + string(encoded) +
		"\n\nEmit every required property. Do not wrap the JSON in Markdown code " +
		"fences and do not add any prose before or after it."
}

// applyAsInstruction appends the schema requirement to the system prompt.
//
// The existing prompt is extended rather than replaced, and an array-shaped
// system field gains a block instead of being flattened to a string — the
// blocks carry cache_control markers, and collapsing them would silently
// invalidate the caller's prompt cache.
func (s *structuredOutputSpec) applyAsInstruction(req *ClaudeRequest) bool {
	if s == nil {
		return false
	}
	text := s.instruction()
	if text == "" {
		return false
	}
	switch existing := req.System.(type) {
	case nil:
		req.System = text
	case string:
		if strings.TrimSpace(existing) == "" {
			req.System = text
		} else {
			req.System = existing + "\n\n" + text
		}
	case []interface{}:
		req.System = append(existing, map[string]interface{}{"type": "text", "text": text})
	default:
		// An unrecognised shape is left alone rather than overwritten: losing the
		// caller's system prompt is worse than losing the schema constraint.
		return false
	}
	return true
}

// isStructuredCall reports whether a tool call is the synthetic one and should
// be unwrapped rather than forwarded.
func (s *structuredOutputSpec) isStructuredCall(name string) bool {
	return s != nil && name == structuredOutputToolName
}

// renderResult serialises the tool arguments as the text the caller expects.
//
// Indented, because a schema-constrained answer is usually read by a person as
// often as by a parser, and json.Marshal's single line is unreadable past a few
// fields. Whitespace is insignificant to every JSON parser.
func (s *structuredOutputSpec) renderResult(input map[string]interface{}) string {
	encoded, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		// The arguments came off the wire as JSON, so this cannot fail for any
		// value the upstream can produce.
		return ""
	}
	return strings.TrimSpace(string(encoded))
}
