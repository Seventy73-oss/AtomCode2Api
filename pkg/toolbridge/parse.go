package toolbridge

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ToolCall is a parsed client-side tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON object
}

// toolCallPattern matches a complete <tool_call>...</tool_call> block.
// DOTALL is implied by the (?s) flag so pretty-printed JSON also matches.
var toolCallPattern = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// fencedPattern matches a <tool_call> block wrapped in a markdown code fence,
// which models emit despite being told not to.
var fencedPattern = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*(<tool_call>.*?</tool_call>)\\s*```")

// legacyPattern accepts the OpenAI-ish <tool_use> spelling some models produce.
var legacyPattern = regexp.MustCompile(`(?s)<tool_use>\s*(.*?)\s*</tool_use>`)

// rawCall is the payload inside a <tool_call> block.
type rawCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// Some models emit `parameters` or `input` instead of `arguments`.
	Parameters json.RawMessage `json:"parameters"`
	Input      json.RawMessage `json:"input"`
}

// HasToolCallMarker reports whether the text contains the start of a tool call
// block. Used to decide whether to hold back streamed text.
func HasToolCallMarker(s string) bool {
	return strings.Contains(s, "<tool_call>") || strings.Contains(s, "<tool_use>")
}

// ContainsCompleteToolCall reports whether a full block is present.
func ContainsCompleteToolCall(s string) bool {
	return toolCallPattern.MatchString(s) || legacyPattern.MatchString(s)
}

// Parse extracts tool calls from a complete assistant message.
//
// `valid` is the set of tool names the client actually declared; a block naming
// anything else is rejected (models sometimes hallucinate names, and ordinary
// prose could otherwise trip the parser). Returns the cleaned text with all
// tool-call blocks removed, plus the parsed calls.
func Parse(text string, valid map[string]bool) (clean string, calls []ToolCall) {
	clean = text

	// Unwrap fenced blocks first so their contents are parsed normally.
	clean = fencedPattern.ReplaceAllString(clean, "$1")

	matches := toolCallPattern.FindAllStringSubmatch(clean, -1)
	for _, m := range matches {
		if call, ok := parseBlock(m[1], valid); ok {
			calls = append(calls, call)
		}
	}
	if len(matches) == 0 {
		// Fall back to the <tool_use> spelling.
		for _, m := range legacyPattern.FindAllStringSubmatch(clean, -1) {
			if call, ok := parseBlock(m[1], valid); ok {
				calls = append(calls, call)
			}
		}
	}

	// Strip every tool-call block (valid or not) from the visible text so the
	// client never sees raw protocol markup.
	clean = toolCallPattern.ReplaceAllString(clean, "")
	clean = legacyPattern.ReplaceAllString(clean, "")
	clean = strings.TrimSpace(clean)

	return clean, calls
}

// parseBlock decodes one block body into a ToolCall.
func parseBlock(body string, valid map[string]bool) (ToolCall, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return ToolCall{}, false
	}

	var rc rawCall
	if err := json.Unmarshal([]byte(body), &rc); err != nil {
		// Tolerate a bare function-call spelling: tool_name({"k":"v"})
		return parseCallSyntax(body, valid)
	}

	name := strings.TrimSpace(rc.Name)
	if name == "" {
		return ToolCall{}, false
	}
	if valid != nil && !valid[name] {
		// Hallucinated or unknown tool name: drop it rather than forwarding a
		// call the client cannot execute.
		return ToolCall{}, false
	}

	args := rc.Arguments
	if len(args) == 0 {
		args = rc.Parameters
	}
	if len(args) == 0 {
		args = rc.Input
	}
	argStr := strings.TrimSpace(string(args))
	if argStr == "" || argStr == "null" {
		argStr = "{}"
	}
	// Arguments must be a JSON object; if the model sent a string, re-wrap it.
	if !strings.HasPrefix(argStr, "{") && !strings.HasPrefix(argStr, "[") {
		var s string
		if json.Unmarshal(args, &s) == nil {
			argStr = s
		}
		if !json.Valid([]byte(argStr)) {
			argStr = "{}"
		}
	}

	return ToolCall{Name: name, Arguments: argStr}, true
}

// callSyntaxPattern matches `name({...})` or `name({...})` style output.
var callSyntaxPattern = regexp.MustCompile(`(?s)^([A-Za-z_][A-Za-z0-9_.-]*)\s*\(\s*(\{.*\})\s*\)\s*$`)

func parseCallSyntax(body string, valid map[string]bool) (ToolCall, bool) {
	m := callSyntaxPattern.FindStringSubmatch(body)
	if m == nil {
		return ToolCall{}, false
	}
	name := m[1]
	if valid != nil && !valid[name] {
		return ToolCall{}, false
	}
	args := strings.TrimSpace(m[2])
	if !json.Valid([]byte(args)) {
		return ToolCall{}, false
	}
	return ToolCall{Name: name, Arguments: args}, true
}

// SplitStreamBuffer decides how much of an accumulating stream buffer is safe
// to emit as visible text.
//
// When a `<tool_call>` marker appears, everything from that marker onward is
// withheld so the protocol block never reaches the client. `held` reports
// whether any text is being withheld.
func SplitStreamBuffer(buf string) (emit string, held bool) {
	idx := strings.Index(buf, "<tool_call>")
	legacyIdx := strings.Index(buf, "<tool_use>")
	if legacyIdx >= 0 && (idx < 0 || legacyIdx < idx) {
		idx = legacyIdx
	}
	if idx < 0 {
		// Guard against a partial marker split across chunks (e.g. "<tool_c").
		if tail := partialMarkerTail(buf); tail > 0 {
			return buf[:len(buf)-tail], true
		}
		return buf, false
	}
	return buf[:idx], true
}

// partialMarkerTail returns the length of a trailing fragment that could be the
// beginning of a tool-call marker, so it is not emitted prematurely.
func partialMarkerTail(buf string) int {
	for _, marker := range []string{"<tool_call>", "<tool_use>"} {
		// Check the longest suffix of buf that is a prefix of marker.
		max := len(marker) - 1
		if max > len(buf) {
			max = len(buf)
		}
		for n := max; n > 0; n-- {
			if strings.HasSuffix(buf, marker[:n]) {
				return n
			}
		}
	}
	return 0
}
