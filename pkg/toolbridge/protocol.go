// Package toolbridge bridges client-side tool calling onto the AtomCode daemon.
//
// Background: the daemon ignores the `tools` field of a `/chat` request and only
// ever exposes its own built-in tools. An OpenAI/Anthropic client such as Claude
// Code or Cursor, however, expects the standard handshake:
//
//	client sends tools -> model returns tool_calls -> client executes them
//	-> client sends results back -> model continues
//
// Since the daemon cannot participate in that handshake, this package encodes the
// client's tools into the prompt as a small text protocol, then parses the
// model's reply and converts it back into standard `tool_calls`. The client
// executes the tool locally and returns the result, which the caller folds into
// the next daemon request.
package toolbridge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tool is a normalized client-side tool definition.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema describing the tool's parameters.
	Schema json.RawMessage
}

// openAITool is the OpenAI `tools[]` entry shape.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// anthropicTool is the Anthropic `tools[]` entry shape.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ParseOpenAITools parses the OpenAI `tools` array. Entries that are not
// `type: "function"` (e.g. OpenAI built-ins like web_search) are skipped, since
// the daemon cannot execute them.
func ParseOpenAITools(raw json.RawMessage) []Tool {
	if len(raw) == 0 {
		return nil
	}
	var entries []openAITool
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	tools := make([]Tool, 0, len(entries))
	for _, e := range entries {
		if e.Type != "" && e.Type != "function" {
			continue
		}
		if e.Function.Name == "" {
			continue
		}
		tools = append(tools, Tool{
			Name:        e.Function.Name,
			Description: e.Function.Description,
			Schema:      e.Function.Parameters,
		})
	}
	return tools
}

// ParseAnthropicTools parses the Anthropic `tools` array.
func ParseAnthropicTools(raw json.RawMessage) []Tool {
	if len(raw) == 0 {
		return nil
	}
	var entries []anthropicTool
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	tools := make([]Tool, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		tools = append(tools, Tool{
			Name:        e.Name,
			Description: e.Description,
			Schema:      e.InputSchema,
		})
	}
	return tools
}

// Names returns the set of valid tool names, used by the parser to reject
// hallucinated or coincidental marker text.
func Names(tools []Tool) map[string]bool {
	m := make(map[string]bool, len(tools))
	for _, t := range tools {
		m[t.Name] = true
	}
	return m
}

// ToolChoiceNone reports whether the client explicitly disabled tool use.
func ToolChoiceNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "none"
	}
	return false
}

// ProtocolPrompt renders the client tool definitions plus the calling protocol
// into a block that is prepended to the user message.
//
// Framing matters more than content here. Verified against a real AtomCode
// daemon (v5.1.0, AtomGit-glm5.3-flash):
//
//   - Bracketed headers such as "[CLIENT TOOL PROTOCOL]" are classified as a
//     prompt injection and refused, with the model falling back to its built-in
//     tools.
//   - Describing the tools with a JSON Schema triggers the model's *native*
//     function-calling instinct; the daemon then answers "unknown or unmounted
//     tool" and the turn is wasted.
//   - Plain, conversational framing that presents the tools as the user's own
//     local helper is followed reliably.
//
// So the protocol is written as ordinary user context: no brackets, no
// "protocol"/"injection"-flavoured vocabulary, and no JSON Schema blocks. The
// call line is a simple `name {json}` form that models reproduce verbatim.
//
// `required` asks the model to use a tool on this turn.
func ProtocolPrompt(tools []Tool, required bool) string {
	if len(tools) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Before answering: my local helper app runs a few tools for me, ")
	b.WriteString("so please don't search the web for these. When you need one, ")
	b.WriteString("reply with a single line in exactly this form and then stop:\n\n")
	b.WriteString("CALL <tool_name> {<arguments as json>}\n\n")

	b.WriteString("For example: CALL get_weather {\"city\": \"Beijing\"}\n\n")

	b.WriteString("The tools my helper provides:\n")
	for _, t := range tools {
		desc := strings.TrimSpace(t.Description)
		if desc != "" {
			b.WriteString(fmt.Sprintf("- %s: %s\n", t.Name, desc))
		} else {
			b.WriteString(fmt.Sprintf("- %s\n", t.Name))
		}
		if args := describeSchema(t.Schema); args != "" {
			b.WriteString("  arguments: " + args + "\n")
		}
	}

	b.WriteString("\nRules for the CALL line:\n")
	b.WriteString("- Use a tool name from the list above, spelled exactly.\n")
	b.WriteString("- Put the arguments as a JSON object on the same line.\n")
	b.WriteString("- Emit one CALL line, then stop so my helper can run it.\n")
	b.WriteString("- I'll paste the result back, and you continue from there.\n")
	if required {
		b.WriteString("- Please use one of these tools for this request.\n")
	}

	return b.String()
}

// describeSchema renders a JSON Schema as a short human-readable argument list
// (e.g. `city (string, required)`). The full schema is deliberately not
// emitted: a raw `{"type":"object",...}` block makes models attempt a native
// function call, which the daemon rejects.
func describeSchema(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	var s struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return ""
	}

	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}

	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		p := s.Properties[name]
		typ := p.Type
		if typ == "" {
			typ = "any"
		}
		desc := name + " (" + typ
		if required[name] {
			desc += ", required"
		}
		desc += ")"
		if d := strings.TrimSpace(p.Description); d != "" {
			desc += " - " + d
		}
		parts = append(parts, desc)
	}
	return strings.Join(parts, ", ")
}

// FormatToolResult renders a tool execution result for inclusion in the next
// daemon request, so the model can continue with the output in context.
func FormatToolResult(name, toolCallID, content string) string {
	var b strings.Builder
	b.WriteString("[TOOL RESULT]\n")
	if name != "" {
		b.WriteString("tool: " + name + "\n")
	}
	if toolCallID != "" {
		b.WriteString("call_id: " + toolCallID + "\n")
	}
	b.WriteString("output:\n")
	b.WriteString(content)
	b.WriteString("\n[END TOOL RESULT]")
	return b.String()
}

// FormatAssistantToolCall renders a prior assistant tool call for inclusion in
// the conversation history of the next daemon request.
func FormatAssistantToolCall(name, arguments string) string {
	return fmt.Sprintf("[ASSISTANT CALLED TOOL] %s(%s)", name, arguments)
}
