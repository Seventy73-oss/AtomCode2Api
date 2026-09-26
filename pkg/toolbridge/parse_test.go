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

func TestProtocolPromptIncludesSchemaAndRules(t *testing.T) {
	p := ProtocolPrompt(sampleTools(), true)
	for _, want := range []string{"get_weather", "read_file", "parameters:", "<tool_call>", "MUST call"} {
		if !strings.Contains(p, want) {
			t.Errorf("protocol prompt missing %q", want)
		}
	}
	if ProtocolPrompt(nil, false) != "" {
		t.Error("expected empty prompt for no tools")
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
