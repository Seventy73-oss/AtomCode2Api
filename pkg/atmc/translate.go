package atmc

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// ConversationKey generates a deterministic hash for a conversation prefix.
func ConversationKey(messages []map[string]any, system string) string {
	payload := system
	for i, m := range messages {
		if i == len(messages)-1 {
			break
		}
		role, _ := m["role"].(string)
		content := contentString(m["content"])
		payload += fmt.Sprintf("|%s:%s", role, content)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(payload)))[:16]
}

// FormatMessages formats the messages list into a single daemon message string.
//
// The daemon v5.1.0 `/chat` body has NO `system` field, so the system prompt is
// folded into the message body. Without this the prompt is silently dropped by
// the daemon (see the note in crates/atomcode-daemon/src/lib.rs `struct ChatRequest`).
//
// Assistant messages carrying `tool_calls` and `tool` role messages carrying
// execution results are rendered as explicit text blocks, so the model can see
// the tool handshake that happened on the client side.
func FormatMessages(messages []map[string]any, systemPrompt string) string {
	var parts []string

	// Inline the system prompt as a leading instruction block.
	if sp := strings.TrimSpace(systemPrompt); sp != "" {
		parts = append(parts, "System: "+sp)
	}

	for _, m := range messages {
		role, _ := m["role"].(string)

		// Assistant turn that requested tool calls.
		if role == "assistant" {
			text := contentString(m["content"])
			calls := renderToolCalls(m["tool_calls"])
			switch {
			case text != "" && calls != "":
				parts = append(parts, fmt.Sprintf("Assistant: %s\n%s", text, calls))
			case calls != "":
				parts = append(parts, "Assistant: "+calls)
			case text != "":
				parts = append(parts, "Assistant: "+text)
			}
			continue
		}

		// Tool execution result being fed back to the model.
		if role == "tool" {
			content := contentString(m["content"])
			name, _ := m["name"].(string)
			id, _ := m["tool_call_id"].(string)
			if content == "" && name == "" {
				continue
			}
			var b strings.Builder
			b.WriteString("User: [TOOL RESULT]")
			if name != "" {
				b.WriteString("\ntool: " + name)
			}
			if id != "" {
				b.WriteString("\ncall_id: " + id)
			}
			b.WriteString("\noutput:\n" + content + "\n[END TOOL RESULT]")
			parts = append(parts, b.String())
			continue
		}

		content := contentString(m["content"])
		if content == "" {
			continue
		}

		label := "User"
		switch role {
		case "user":
			label = "User"
		case "system":
			// Already hoisted into the leading System block by the caller;
			// a mid-conversation system message is still surfaced here.
			if sp := strings.TrimSpace(systemPrompt); sp != "" {
				continue
			}
			label = "System"
		default:
			label = titleCase(role)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", label, content))
	}
	return strings.Join(parts, "\n\n")
}

// renderToolCalls turns an OpenAI-style `tool_calls` array into a readable
// text block for the daemon.
func renderToolCalls(raw any) string {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range list {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := ""
		args := ""
		if fn, ok := call["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		if name == "" {
			name, _ = call["name"].(string)
		}
		if args == "" {
			args, _ = call["arguments"].(string)
		}
		if name == "" {
			continue
		}
		if args == "" {
			args = "{}"
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("[ASSISTANT CALLED TOOL] %s(%s)", name, args))
	}
	return b.String()
}

// titleCase upper-cases the first rune, replacing the deprecated strings.Title.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 32
	}
	return string(r)
}

// contentString extracts a string from message content.
func contentString(content any) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var texts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "text" {
					if t, ok := m["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

// FindProviderForModel searches the provider list for a matching model name.
// v5.1.0 exposes the selectable id as `name`, with `model` as the upstream name.
func FindProviderForModel(providers []ProviderConfig, model string) string {
	modelLower := strings.ToLower(model)
	for _, p := range providers {
		if strings.ToLower(p.Name) == modelLower {
			return p.Name
		}
	}
	// Fall back to matching the concrete upstream model name.
	for _, p := range providers {
		if strings.ToLower(p.Model) == modelLower {
			return p.Name
		}
	}
	return ""
}

// ─── OpenAI SSE Translation ──────────────────────────────────────────────────

// TranslateToOpenAIChunk converts a daemon SSEEvent to an OpenAI SSE data line.
// Returns the SSE data string, or empty string to skip, or "__DONE__" to signal completion.
//
// `clientTools` is the set of tool names the client declared. The daemon runs
// its OWN built-in tools (web_search, read_file, ...) during a turn and emits
// tool_start/tool_result for them; forwarding those as client tool_calls would
// ask the client to execute a tool it never declared. Pass nil to forward all.
func TranslateToOpenAIChunk(ev *SSEEvent, model string, toolIdx *int, clientTools map[string]bool) string {
	switch ev.Type {
	case "text":
		return fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"index":0}]}`, jsonString(ev.Content))
	case "reasoning":
		return fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%s},"index":0}]}`, jsonString(ev.Content))
	case "tool_start":
		// Only surface calls the client can actually execute.
		if clientTools != nil && !clientTools[ev.Name] {
			return ""
		}
		*toolIdx++
		id := ev.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", *toolIdx)
		}
		return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}]},"index":0}]}`,
			jsonString(id), jsonString(ev.Name), jsonString(ev.Arguments))
	case "tool_batch":
		// The batch announces the whole tool set for this assistant turn,
		// including daemon-internal tools. Individual tool_start events follow
		// and are filtered above, so nothing to emit here.
		return ""
	case "tool_output", "tool_result", "tool_progress":
		return ""
	case "tokens":
		// The usage chunk has empty `choices`. The closing brace for the
		// `usage` object was missing here, which produced malformed JSON once
		// BuildOpenAIFullChunk spliced it in (it strips the leading '{' and
		// re-wraps), so strict clients failed with "Extra data: line 1 column N".
		return fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			ev.Prompt, ev.Completion, ev.Total)
	case "warning", "persistence_warning":
		// Non-fatal advisories: surface as a reasoning-style notice so clients
		// see something instead of silently dropping the signal.
		return fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%s},"index":0}]}`, jsonString(ev.Message))
	case "rate_limited":
		return fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%s},"index":0}]}`,
			jsonString(fmt.Sprintf("[rate limited] reset %s", strOr(ev.ResetLabel, ev.ResetAtDisplay))))
	case "done", "stopped":
		return "__DONE__"
	case "error":
		return fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":"error","index":0}]}`)
	default:
		return ""
	}
}

// BuildOpenAIFullChunk wraps a delta into a full OpenAI SSE chunk JSON string.
//
// `deltaJSON` is a complete JSON object such as
// `{"choices":[{"delta":{"content":"hi"},"index":0}]}`. Its leading '{' is
// stripped and the remaining fields are spliced into the chunk envelope, so the
// envelope must NOT add a trailing '}' of its own — doing so produced malformed
// JSON on every text and tool_call frame (strict clients abort the stream with
// "Extra data: line 1 column N").
//
// The result is validated before being returned; a malformed frame is dropped
// and logged rather than emitted.
func BuildOpenAIFullChunk(deltaJSON string, model string) string {
	if deltaJSON == "__DONE__" || deltaJSON == "" {
		return ""
	}
	if !json.Valid([]byte(deltaJSON)) {
		log.Printf("atmc: dropping malformed delta (invalid JSON): %s", deltaJSON)
		return ""
	}
	// deltaJSON[1:] drops the leading '{', leaving `"choices":[...]}` which
	// closes the envelope itself.
	full := fmt.Sprintf(`{"id":"chatcmpl-atomcode","object":"chat.completion.chunk","created":%d,"model":%s,%s`,
		time.Now().Unix(), jsonString(model), deltaJSON[1:])
	if !json.Valid([]byte(full)) {
		log.Printf("atmc: dropping malformed chunk after splice: %s", full)
		return ""
	}
	return full
}

// ─── Anthropic SSE Translation ───────────────────────────────────────────────

// AnthropicState tracks Anthropic SSE translation progress.
type AnthropicState struct {
	MessageID    string
	ContentIndex int
	HasSentStart bool
	CurrentBlock string // "text", "thinking", "tool_use"
}

func NewAnthropicState() *AnthropicState {
	return &AnthropicState{
		MessageID: fmt.Sprintf("msg_%x", time.Now().UnixNano()),
	}
}

// TranslateToAnthropicSSE converts a daemon SSEEvent to Anthropic SSE data lines.
//
// `clientTools` is the set of tool names the client declared; the daemon's own
// built-in tools (web_search, read_file, ...) are filtered out so the client is
// never asked to execute a tool it did not declare. Pass nil to forward all.
func TranslateToAnthropicSSE(ev *SSEEvent, model string, state *AnthropicState, clientTools map[string]bool) []string {
	switch ev.Type {
	case "text":
		return translateAnthropicText(ev, model, state)
	case "reasoning":
		return translateAnthropicReasoning(ev, model, state)
	case "tool_start":
		if clientTools != nil && !clientTools[ev.Name] {
			return nil
		}
		return translateAnthropicToolStart(ev, model, state)
	case "tool_batch":
		// Announces every tool for the turn, including daemon-internal ones.
		// The individual tool_start events are filtered above.
		return nil
	case "tool_output", "tool_result", "tool_progress":
		return nil
	case "tokens":
		return []string{
			fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d,"input_tokens":%d},"message":%s}`,
				ev.Completion, ev.Prompt, jsonString(state.MessageID)),
		}
	case "warning", "persistence_warning":
		return []string{
			fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
				state.ContentIndex, jsonString("\n[notice] "+ev.Message+"\n")),
		}
	case "rate_limited":
		label := strOr(ev.ResetLabel, ev.ResetAtDisplay)
		return []string{
			fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
				state.ContentIndex, jsonString("\n[rate limited] resets "+label+"\n")),
		}
	case "done", "stopped":
		var lines []string
		if state.CurrentBlock != "" {
			lines = append(lines, fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex))
			state.CurrentBlock = ""
		}
		lines = append(lines,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{}}`,
			`{"type":"message_stop"}`)
		return lines
	case "error":
		if state.CurrentBlock != "" {
			state.CurrentBlock = ""
		}
		return []string{
			fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":%s}}`, jsonString(ev.Message)),
		}
	default:
		return nil
	}
}

func translateAnthropicText(ev *SSEEvent, model string, state *AnthropicState) []string {
	// If we are in a non-text block, close it first
	if state.CurrentBlock != "" && state.CurrentBlock != "text" {
		closeBlock := []string{
			fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex),
		}
		state.ContentIndex++
		state.CurrentBlock = "text"
		openBlock := []string{
			fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, state.ContentIndex),
			fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, state.ContentIndex, jsonString(ev.Content)),
		}
		return append(closeBlock, openBlock...)
	}

	if !state.HasSentStart {
		state.HasSentStart = true
		state.CurrentBlock = "text"
		return []string{
			fmt.Sprintf(`{"type":"message_start","message":{"id":%s,"type":"message","role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}},"model":%s}`,
				jsonString(state.MessageID), jsonString(model), jsonString(model)),
			fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, state.ContentIndex),
			fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, state.ContentIndex, jsonString(ev.Content)),
		}
	}
	state.CurrentBlock = "text"
	return []string{
		fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, state.ContentIndex, jsonString(ev.Content)),
	}
}

func translateAnthropicReasoning(ev *SSEEvent, model string, state *AnthropicState) []string {
	// If we are in a non-thinking block, close it first
	if state.CurrentBlock != "" && state.CurrentBlock != "thinking" {
		closeBlock := []string{
			fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex),
		}
		state.ContentIndex++
		state.CurrentBlock = "thinking"
		openBlock := []string{
			fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, state.ContentIndex),
			fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, state.ContentIndex, jsonString(ev.Content)),
		}
		return append(closeBlock, openBlock...)
	}

	if !state.HasSentStart {
		state.HasSentStart = true
		state.CurrentBlock = "thinking"
		return []string{
			fmt.Sprintf(`{"type":"message_start","message":{"id":%s,"type":"message","role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}},"model":%s}`,
				jsonString(state.MessageID), jsonString(model), jsonString(model)),
			fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, state.ContentIndex),
			fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, state.ContentIndex, jsonString(ev.Content)),
		}
	}
	state.CurrentBlock = "thinking"
	return []string{
		fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, state.ContentIndex, jsonString(ev.Content)),
	}
}

func translateAnthropicToolStart(ev *SSEEvent, model string, state *AnthropicState) []string {
	var lines []string

	// Close current block if any
	if state.CurrentBlock != "" && state.CurrentBlock != "tool_use" {
		lines = append(lines, fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex))
		state.ContentIndex++
		state.HasSentStart = true
	} else if !state.HasSentStart {
		state.HasSentStart = true
		state.ContentIndex = 0
		lines = append(lines,
			fmt.Sprintf(`{"type":"message_start","message":{"id":%s,"type":"message","role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}},"model":%s}`,
				jsonString(state.MessageID), jsonString(model), jsonString(model)))
	} else if state.CurrentBlock == "tool_use" {
		// Already in a tool_use, close previous
		lines = append(lines, fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex))
		state.ContentIndex++
	}

	state.CurrentBlock = "tool_use"
	lines = append(lines,
		fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, state.ContentIndex, jsonString(ev.ID), jsonString(ev.Name)),
		fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, state.ContentIndex, jsonString(ev.Arguments)),
	)
	return lines
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// strOr returns def when s is empty.
func strOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}