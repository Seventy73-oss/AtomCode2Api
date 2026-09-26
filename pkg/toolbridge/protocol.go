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
// `required` forces a tool call on this turn (tool_choice=required / any).
func ProtocolPrompt(tools []Tool, required bool) string {
	if len(tools) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[CLIENT TOOL PROTOCOL]\n")
	b.WriteString("The tools listed below are provided by the CLIENT and execute on the\n")
	b.WriteString("CLIENT's machine — not on yours. You do not have them built in.\n")
	b.WriteString("Do NOT use your own built-in tools for these operations.\n\n")

	b.WriteString("Available client tools:\n")
	for _, t := range tools {
		schema := strings.TrimSpace(string(t.Schema))
		if schema == "" || schema == "null" {
			schema = `{"type":"object","properties":{}}`
		}
		desc := strings.TrimSpace(t.Description)
		if desc != "" {
			b.WriteString(fmt.Sprintf("- %s: %s\n", t.Name, desc))
		} else {
			b.WriteString(fmt.Sprintf("- %s\n", t.Name))
		}
		b.WriteString(fmt.Sprintf("  parameters: %s\n", schema))
	}

	b.WriteString("\nTo call one of these tools, reply with ONLY the following block and\n")
	b.WriteString("nothing else, then stop generating immediately:\n\n")
	b.WriteString(`<tool_call>{"name":"TOOL_NAME","arguments":{}}</tool_call>`)
	b.WriteString("\n\nRules:\n")
	b.WriteString("- `name` must be exactly one of the tool names listed above.\n")
	b.WriteString("- `arguments` must be a JSON object matching that tool's parameters.\n")
	b.WriteString("- Emit at most one <tool_call> block per reply.\n")
	b.WriteString("- Do not wrap the block in a markdown code fence.\n")
	b.WriteString("- Do not invent tool names.\n")
	b.WriteString("- After emitting the block, STOP. The client runs the tool and returns\n")
	b.WriteString("  the result to you; you then continue the task.\n")
	if required {
		b.WriteString("\nYou MUST call one of the tools above on this turn.\n")
	}
	b.WriteString("[END CLIENT TOOL PROTOCOL]")

	return b.String()
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
