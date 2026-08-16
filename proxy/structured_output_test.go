package proxy

import (
	"encoding/json"
	"testing"
)

func mustStructuredRaw(body string) map[string]interface{} {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		panic(err)
	}
	return raw
}

const personSchema = `{
  "type": "object",
  "properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
  "required": ["name", "age"],
  "additionalProperties": false
}`

func TestParseStructuredOutputNestedForm(t *testing.T) {
	raw := mustStructuredRaw(`{"output_config":{"format":{"type":"json_schema","schema":` + personSchema + `}}}`)

	spec := parseStructuredOutput(raw)
	if spec == nil {
		t.Fatal("output_config.format was not recognised")
	}
	if spec.Schema["type"] != "object" {
		t.Fatalf("schema not carried through: %v", spec.Schema)
	}
}

// Clients pinned to the earlier beta still send the flat spelling.
func TestParseStructuredOutputLegacyFlatForm(t *testing.T) {
	raw := mustStructuredRaw(`{"output_format":{"type":"json_schema","schema":` + personSchema + `}}`)

	if spec := parseStructuredOutput(raw); spec == nil {
		t.Fatal("legacy output_format was not recognised")
	}
}

func TestParseStructuredOutputIgnoresPlainText(t *testing.T) {
	raw := mustStructuredRaw(`{"output_config":{"format":{"type":"text"}}}`)

	if spec := parseStructuredOutput(raw); spec != nil {
		t.Fatal(`format type "text" means unconstrained and must not force a tool`)
	}
}

func TestParseStructuredOutputIgnoresEmptySchema(t *testing.T) {
	raw := mustStructuredRaw(`{"output_config":{"format":{"type":"json_schema","schema":{}}}}`)

	if spec := parseStructuredOutput(raw); spec != nil {
		t.Fatal("an empty schema constrains nothing")
	}
}

func TestParseStructuredOutputAbsent(t *testing.T) {
	if spec := parseStructuredOutput(mustStructuredRaw(`{"model":"claude-sonnet-4-5"}`)); spec != nil {
		t.Fatal("a request with no output_config must not be treated as structured")
	}
}

func TestApplyToRequestForcesTheSchemaTool(t *testing.T) {
	spec := parseStructuredOutput(mustStructuredRaw(
		`{"output_config":{"format":{"type":"json_schema","schema":` + personSchema + `}}}`))
	req := &ClaudeRequest{Model: "claude-sonnet-4-5", MaxTokens: 256}

	if !spec.applyToRequest(req) {
		t.Fatal("applyToRequest should have taken effect on a request with no tools")
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != structuredOutputToolName {
		t.Fatalf("schema tool not installed: %+v", req.Tools)
	}
	if req.Tools[0].InputSchema == nil {
		t.Fatal("the tool must carry the caller's schema as its input_schema")
	}

	choice, ok := req.ToolChoice.(map[string]interface{})
	if !ok || choice["type"] != "tool" || choice["name"] != structuredOutputToolName {
		t.Fatalf("tool_choice not forced to the schema tool: %+v", req.ToolChoice)
	}
}

// Forcing the schema tool would suppress the caller's own tools, so the
// constraint is dropped instead of the tools.
func TestApplyToRequestDeclinesWhenCallerHasTools(t *testing.T) {
	spec := parseStructuredOutput(mustStructuredRaw(
		`{"output_config":{"format":{"type":"json_schema","schema":` + personSchema + `}}}`))
	req := &ClaudeRequest{
		Model: "claude-sonnet-4-5",
		Tools: []ClaudeTool{{Name: "get_weather", InputSchema: map[string]interface{}{"type": "object"}}},
	}

	if spec.applyToRequest(req) {
		t.Fatal("must not override a request that already declares tools")
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("caller tools were modified: %+v", req.Tools)
	}
}

func TestRenderResultProducesParseableJSON(t *testing.T) {
	spec := &structuredOutputSpec{}

	rendered := spec.renderResult(map[string]interface{}{"name": "Ada", "age": 36})

	var back map[string]interface{}
	if err := json.Unmarshal([]byte(rendered), &back); err != nil {
		t.Fatalf("rendered output is not valid JSON: %v\n%s", err, rendered)
	}
	if back["name"] != "Ada" {
		t.Fatalf("field lost in rendering: %v", back)
	}
}

// The response path calls these on a nil spec for every ordinary request.
func TestStructuredSpecMethodsAreNilSafe(t *testing.T) {
	var spec *structuredOutputSpec

	if spec.isStructuredCall(structuredOutputToolName) {
		t.Fatal("a nil spec must not claim tool calls")
	}
	if spec.applyToRequest(&ClaudeRequest{}) {
		t.Fatal("a nil spec must not modify a request")
	}
}

func TestIsStructuredCallOnlyMatchesTheSyntheticTool(t *testing.T) {
	spec := &structuredOutputSpec{Schema: map[string]interface{}{"type": "object"}}

	if !spec.isStructuredCall(structuredOutputToolName) {
		t.Fatal("the synthetic tool must be recognised")
	}
	if spec.isStructuredCall("get_weather") {
		t.Fatal("a caller's tool must never be unwrapped as structured output")
	}
}
