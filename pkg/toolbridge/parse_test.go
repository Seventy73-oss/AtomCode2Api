package toolbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleTools() []Tool {
	return []Tool{
		{
			Name:        "get_weather",
			Description: "Get the weather for a city",
			Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		},
		{
			Name:        "read_file",
			Description: "Read a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	}
}

func TestParseOpenAITools(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}},
		{"type":"web_search"}
	]`)
	tools := ParseOpenAITools(raw)
	if len(tools) != 1 {
		t.Fatalf("expected 1 function tool, got %d", len(tools))
	}
	if tools[0].Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", tools[0].Name)
	}
}

func TestParseAnthropicTools(t *testing.T) {
	raw := json.RawMessage(`[{"name":"read_file","description":"r","input_schema":{"type":"object"}}]`)
	tools := ParseAnthropicTools(raw)
	if len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("unexpected parse result: %+v", tools)
	}
}

func TestParseSimpleToolCall(t *testing.T) {
	text := `I'll check the weather.
<tool_call>{"name":"get_weather","arguments":{"city":"Beijing"}}</tool_call>`

	clean, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", calls[0].Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["city"] != "Beijing" {
		t.Errorf("expected Beijing, got %q", args["city"])
	}
	// The protocol block must not leak into the visible text.
	if strings.Contains(clean, "tool_call") {
		t.Errorf("protocol markup leaked into clean text: %q", clean)
	}
	if !strings.Contains(clean, "check the weather") {
		t.Errorf("expected prose preserved, got %q", clean)
	}
}

func TestParseRejectsUnknownTool(t *testing.T) {
	// A hallucinated tool name must be dropped, not forwarded.
	text := `<tool_call>{"name":"rm_rf_everything","arguments":{}}</tool_call>`
	clean, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 0 {
		t.Fatalf("expected unknown tool to be rejected, got %+v", calls)
	}
	if strings.Contains(clean, "tool_call") {
		t.Errorf("markup should still be stripped, got %q", clean)
	}
}

func TestParseHandlesFencedAndPrettyPrinted(t *testing.T) {
	text := "```json\n<tool_call>\n{\n  \"name\": \"read_file\",\n  \"arguments\": {\"path\": \"/etc/hosts\"}\n}\n</tool_call>\n```"
	_, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 1 {
		t.Fatalf("expected 1 call from fenced block, got %d", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Errorf("expected read_file, got %s", calls[0].Name)
	}
}

func TestParseAlternateArgumentKeys(t *testing.T) {
	// Some models emit `parameters` or `input` instead of `arguments`.
	for _, body := range []string{
		`{"name":"read_file","parameters":{"path":"a"}}`,
		`{"name":"read_file","input":{"path":"a"}}`,
	} {
		_, calls := Parse("<tool_call>"+body+"</tool_call>", Names(sampleTools()))
		if len(calls) != 1 {
			t.Fatalf("body %s: expected 1 call, got %d", body, len(calls))
		}
		if !strings.Contains(calls[0].Arguments, "path") {
			t.Errorf("body %s: lost arguments: %s", body, calls[0].Arguments)
		}
	}
}

func TestParseCallSyntax(t *testing.T) {
	text := `<tool_call>get_weather({"city":"Shanghai"})</tool_call>`
	_, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("expected call-syntax parse, got %+v", calls)
	}
}

func TestParsePlainTextUnaffected(t *testing.T) {
	text := "Here is a normal answer with no tool usage at all."
	clean, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %+v", calls)
	}
	if clean != text {
		t.Errorf("plain text should pass through unchanged:\n got %q\nwant %q", clean, text)
	}
}

func TestParseNoToolsDeclared(t *testing.T) {
	// With no tools declared, nothing should be parsed as a call.
	text := `<tool_call>{"name":"get_weather","arguments":{}}</tool_call>`
	clean, calls := Parse(text, map[string]bool{})
	if len(calls) != 0 {
		t.Fatalf("expected no calls when none declared, got %+v", calls)
	}
	if strings.Contains(clean, "tool_call") {
		t.Errorf("markup should be stripped, got %q", clean)
	}
}

func TestSplitStreamBufferHoldsMarker(t *testing.T) {
	// Text before the marker is emitted; the marker onward is withheld.
	emit, held := SplitStreamBuffer("Hello <tool_call>{\"name\":")
	if emit != "Hello " {
		t.Errorf("expected 'Hello ', got %q", emit)
	}
	if !held {
		t.Error("expected held=true when a marker is present")
	}
}

func TestSplitStreamBufferPartialMarker(t *testing.T) {
	// A marker split across chunks must not be emitted prematurely.
	emit, held := SplitStreamBuffer("Answer text <tool_c")
	if emit != "Answer text " {
		t.Errorf("expected partial marker withheld, got %q", emit)
	}
	if !held {
		t.Error("expected held=true for a partial marker")
	}
}

func TestSplitStreamBufferNoMarker(t *testing.T) {
	emit, held := SplitStreamBuffer("Just normal text")
	if emit != "Just normal text" || held {
		t.Errorf("expected full passthrough, got emit=%q held=%v", emit, held)
	}
}

func TestContainsCompleteToolCall(t *testing.T) {
	if !ContainsCompleteToolCall(`<tool_call>{"name":"x","arguments":{}}</tool_call>`) {
		t.Error("expected complete call detected")
	}
	if ContainsCompleteToolCall(`<tool_call>{"name":"x"`) {
		t.Error("incomplete block must not be reported complete")
	}
}

func TestProtocolPromptShape(t *testing.T) {
	p := ProtocolPrompt(sampleTools(), true)

	// Both tool names and their argument hints must be present.
	for _, want := range []string{"get_weather", "read_file", "CALL "} {
		if !strings.Contains(p, want) {
			t.Errorf("protocol prompt missing %q", want)
		}
	}
	// Argument hints are rendered human-readably, not as a raw JSON Schema.
	if !strings.Contains(p, "city (string, required)") {
		t.Errorf("expected readable argument hint, got:\n%s", p)
	}
	if ProtocolPrompt(nil, false) != "" {
		t.Error("expected empty prompt for no tools")
	}
}

// The prompt must avoid the two framings that a real AtomCode daemon rejected:
// bracketed "protocol" headers (classified as prompt injection) and raw JSON
// Schema blocks (which trigger a doomed native function call).
func TestProtocolPromptAvoidsRejectedFramings(t *testing.T) {
	p := ProtocolPrompt(sampleTools(), false)

	for _, bad := range []string{"[CLIENT TOOL PROTOCOL]", "TOOL PROTOCOL", "[END "} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt contains framing rejected as injection: %q", bad)
		}
	}
	if strings.Contains(p, `"type":"object"`) || strings.Contains(p, `"properties"`) {
		t.Errorf("prompt embeds a raw JSON Schema, which triggers native tool calls:\n%s", p)
	}
}

func TestDescribeSchema(t *testing.T) {
	got := describeSchema(json.RawMessage(`{
		"type":"object",
		"properties":{
			"city":{"type":"string","description":"City name"},
			"days":{"type":"integer"}
		},
		"required":["city"]
	}`))
	if !strings.Contains(got, "city (string, required)") {
		t.Errorf("missing required marker: %s", got)
	}
	if !strings.Contains(got, "days (integer)") {
		t.Errorf("missing optional arg: %s", got)
	}
	if !strings.Contains(got, "City name") {
		t.Errorf("missing description: %s", got)
	}
	// A schema with no properties yields nothing rather than an empty shell.
	if describeSchema(json.RawMessage(`{"type":"object"}`)) != "" {
		t.Error("expected empty description for property-less schema")
	}
	if describeSchema(nil) != "" {
		t.Error("expected empty description for nil schema")
	}
}

func TestParseCallLine(t *testing.T) {
	// The primary protocol form emitted by real models.
	text := "CALL get_weather {\"city\": \"Beijing\"}"
	clean, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d (clean=%q)", len(calls), clean)
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", calls[0].Name)
	}
	if calls[0].Arguments != `{"city": "Beijing"}` {
		t.Errorf("unexpected arguments: %s", calls[0].Arguments)
	}
	if strings.TrimSpace(clean) != "" {
		t.Errorf("call line should be stripped from visible text, got %q", clean)
	}
}

func TestParseBareCallLine(t *testing.T) {
	// Without the CALL keyword, as some models emit.
	_, calls := Parse("get_weather {\"city\":\"Shanghai\"}", Names(sampleTools()))
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("expected bare call line to parse, got %+v", calls)
	}
}

func TestParseCallLineRejectsUnknownTool(t *testing.T) {
	_, calls := Parse("CALL delete_everything {\"path\":\"/\"}", Names(sampleTools()))
	if len(calls) != 0 {
		t.Fatalf("unknown tool must be rejected, got %+v", calls)
	}
}

func TestParseProseNotMistakenForCall(t *testing.T) {
	// Ordinary prose containing braces must not be parsed as a call.
	text := "I will explain the function {\"a\":1} syntax in my answer."
	_, calls := Parse(text, Names(sampleTools()))
	if len(calls) != 0 {
		t.Fatalf("prose must not be parsed as a call, got %+v", calls)
	}
}

func TestSplitStreamBufferHoldsCallLine(t *testing.T) {
	emit, held := SplitStreamBuffer("Let me check.\nCALL get_weather {\"city\":")
	if emit != "Let me check.\n" {
		t.Errorf("expected text before CALL line, got %q", emit)
	}
	if !held {
		t.Error("expected held=true once the CALL line starts")
	}
}

func TestToolChoiceNone(t *testing.T) {
	if !ToolChoiceNone(json.RawMessage(`"none"`)) {
		t.Error("expected none detected")
	}
	if ToolChoiceNone(json.RawMessage(`"auto"`)) {
		t.Error("auto is not none")
	}
	if ToolChoiceNone(nil) {
		t.Error("empty is not none")
	}
}
