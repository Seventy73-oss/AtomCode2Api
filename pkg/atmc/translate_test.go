package atmc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConversationKey(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "hello"},
	}
	key1 := ConversationKey(msgs, "")
	key2 := ConversationKey(msgs, "")
	if key1 != key2 {
		t.Errorf("same input should produce same key: %s vs %s", key1, key2)
	}
	if len(key1) != 16 {
		t.Errorf("key should be 16 chars, got %d: %s", len(key1), key1)
	}
}

func TestFormatMessages(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "you are helpful"},
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi there"},
	}
	got := FormatMessages(msgs, "system prompt")
	if !strings.Contains(got, "User: hello") {
		t.Errorf("expected User: hello, got: %s", got)
	}
	if !strings.Contains(got, "Assistant: hi there") {
		t.Errorf("expected Assistant: hi there, got: %s", got)
	}
	// The daemon /chat body has no `system` field, so the system prompt MUST be
	// carried inside the message text or it is silently dropped upstream.
	if !strings.Contains(got, "system prompt") {
		t.Errorf("system prompt must be folded into the message, got: %s", got)
	}
}

func TestFormatMessagesWithoutSystemPrompt(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "hello"},
	}
	got := FormatMessages(msgs, "")
	if got != "User: hello" {
		t.Errorf("expected 'User: hello', got: %q", got)
	}
}

func TestFindProviderForModelMatchesNameAndModel(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "deepseek-v4-flash", Model: "deepseek-v4-flash-20260101"},
	}
	// Match on the selectable id (`name`).
	if got := FindProviderForModel(providers, "deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Errorf("expected name match, got %q", got)
	}
	// Match on the concrete upstream model name.
	if got := FindProviderForModel(providers, "deepseek-v4-flash-20260101"); got != "deepseek-v4-flash" {
		t.Errorf("expected model fallback match, got %q", got)
	}
	if got := FindProviderForModel(providers, "nonexistent"); got != "" {
		t.Errorf("expected empty for unknown model, got %q", got)
	}
}

func TestContentString(t *testing.T) {
	// String content
	if s := contentString("hello"); s != "hello" {
		t.Errorf("expected 'hello', got '%s'", s)
	}
	// Nil content
	if s := contentString(nil); s != "" {
		t.Errorf("expected '', got '%s'", s)
	}
	// Multi-part content
	multi := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "text", "text": "world"},
	}
	if s := contentString(multi); s != "hello\nworld" {
		t.Errorf("expected 'hello\\nworld', got '%s'", s)
	}
	// Image (non-text) content
	withImage := []any{
		map[string]any{"type": "text", "text": "desc"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,..."}},
	}
	if s := contentString(withImage); s != "desc" {
		t.Errorf("expected 'desc', got '%s'", s)
	}
}

func TestFindProviderForModel(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "deepseek", Model: "deepseek-chat"},
		{Name: "openai", Model: "gpt-4"},
	}
	if p := FindProviderForModel(providers, "deepseek-chat"); p != "deepseek" {
		t.Errorf("expected 'deepseek', got '%s'", p)
	}
	if p := FindProviderForModel(providers, "UNKNOWN"); p != "" {
		t.Errorf("expected '', got '%s'", p)
	}
	if p := FindProviderForModel(providers, "DEEPSEEK-CHAT"); p != "deepseek" {
		t.Errorf("case insensitive match failed: got '%s'", p)
	}
}

func TestTranslateToOpenAIChunk(t *testing.T) {
	toolIdx := 0
	cases := []struct {
		name     string
		ev       SSEEvent
		expectFn func(string) bool
	}{
		{
			"text",
			SSEEvent{Type: "text", Content: "hello"},
			func(s string) bool { return strings.Contains(s, `"content":"hello"`) },
		},
		{
			"reasoning",
			SSEEvent{Type: "reasoning", Content: "thinking..."},
			func(s string) bool { return strings.Contains(s, `"reasoning_content"`) },
		},
		{
			"tool_start",
			SSEEvent{Type: "tool_start", ID: "call_1", Name: "read_file", Arguments: `{"path":"a.txt"}`},
			func(s string) bool { return strings.Contains(s, `"name":"read_file"`) },
		},
		{
			"tokens",
			SSEEvent{Type: "tokens", Prompt: 10, Completion: 20, Total: 30},
			func(s string) bool { return strings.Contains(s, `"prompt_tokens":10`) },
		},
		{
			"skip_tool_output",
			SSEEvent{Type: "tool_output"},
			func(s string) bool { return s == "" },
		},
		{
			"done",
			SSEEvent{Type: "done"},
			func(s string) bool { return s == "__DONE__" },
		},
		{
			"stopped",
			SSEEvent{Type: "stopped"},
			func(s string) bool { return s == "__DONE__" },
		},
		{
			"error",
			SSEEvent{Type: "error", Message: "oops"},
			func(s string) bool { return strings.Contains(s, `"finish_reason":"error"`) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TranslateToOpenAIChunk(&c.ev, "test-model", &toolIdx, nil)
			if !c.expectFn(got) {
				t.Errorf("unexpected result: %s", got)
			}
		})
	}
}

func TestTranslateToAnthropicSSE(t *testing.T) {
	state := NewAnthropicState()
	// First event should produce message_start + content_block_start + content_block_delta
	ev := SSEEvent{Type: "text", Content: "hello"}
	lines := TranslateToAnthropicSSE(&ev, "test-model", state, nil)
	if len(lines) != 3 {
		t.Errorf("expected 3 lines for initial text, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"message_start"`) {
		t.Errorf("first line should be message_start: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"type":"content_block_start"`) {
		t.Errorf("second line should be content_block_start: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"type":"content_block_delta"`) {
		t.Errorf("third line should be content_block_delta: %s", lines[2])
	}

	// Subsequent text event should only produce content_block_delta
	state2 := NewAnthropicState()
	state2.HasSentStart = true
	ev2 := SSEEvent{Type: "text", Content: "more"}
	lines2 := TranslateToAnthropicSSE(&ev2, "test-model", state2, nil)
	if len(lines2) != 1 {
		t.Errorf("expected 1 line for subsequent text, got %d", len(lines2))
	}
	if !strings.Contains(lines2[0], `"delta":{"type":"text_delta"`) {
		t.Errorf("expected text_delta: %s", lines2[0])
	}
}

func TestBuildOpenAIFullChunk(t *testing.T) {
	delta := `{"choices":[{"delta":{"content":"hi"},"index":0}]}`
	full := BuildOpenAIFullChunk(delta, "test-model")
	if !strings.Contains(full, `"model":"test-model"`) {
		t.Errorf("expected model in chunk: %s", full)
	}
	if !strings.Contains(full, `"object":"chat.completion.chunk"`) {
		t.Errorf("expected chat.completion.chunk: %s", full)
	}
	if !strings.Contains(full, `"content":"hi"`) {
		t.Errorf("expected content in chunk: %s", full)
	}

	// DONE should return empty
	if s := BuildOpenAIFullChunk("__DONE__", "m"); s != "" {
		t.Errorf("expected empty for DONE, got %s", s)
	}
}

// Every streamed frame must be valid JSON. The envelope previously appended an
// extra '}' after splicing the delta, so strict clients aborted the stream with
// "Extra data: line 1 column N".
func TestStreamedChunksAreValidJSON(t *testing.T) {
	idx := 0
	events := []SSEEvent{
		{Type: "text", Content: "hello"},
		{Type: "text", Content: "world"},
		{Type: "reasoning", Content: "thinking"},
		{Type: "tool_start", ID: "call_1", Name: "get_weather", Arguments: `{"city":"Beijing"}`},
		{Type: "tokens", Prompt: 10, Completion: 5, Total: 15},
		{Type: "warning", Message: "compacted"},
		{Type: "rate_limited", ResetLabel: "21:00"},
		{Type: "error", Message: "boom"},
	}

	for _, ev := range events {
		e := ev
		delta := TranslateToOpenAIChunk(&e, "test-model", &idx, nil)
		if delta == "" || delta == "__DONE__" {
			continue
		}
		if !json.Valid([]byte(delta)) {
			t.Errorf("event %q produced invalid delta JSON: %s", ev.Type, delta)
			continue
		}
		full := BuildOpenAIFullChunk(delta, "test-model")
		if full == "" {
			t.Errorf("event %q produced an empty chunk", ev.Type)
			continue
		}
		if !json.Valid([]byte(full)) {
			t.Errorf("event %q produced invalid chunk JSON: %s", ev.Type, full)
		}
	}
}

// A malformed delta must be dropped, never emitted.
func TestBuildOpenAIFullChunkRejectsMalformed(t *testing.T) {
	if got := BuildOpenAIFullChunk(`{"choices":[`, "m"); got != "" {
		t.Errorf("expected malformed delta to be dropped, got %s", got)
	}
}

func TestJsonString(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"hello", `"hello"`},
		{`he"llo`, `"he\"llo"`},
		{"", `""`},
	}
	for _, c := range cases {
		got := jsonString(c.input)
		if got != c.expected {
			t.Errorf("jsonString(%q) = %s, want %s", c.input, got, c.expected)
		}
	}
}

func TestFormatMessagesRendersToolCalls(t *testing.T) {
	// An assistant turn that requested a tool, followed by the client's result.
	msgs := []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "get_weather",
					"arguments": `{"city":"Beijing"}`,
				},
			},
		}},
		{"role": "tool", "tool_call_id": "call_1", "name": "get_weather", "content": "22C"},
	}
	got := FormatMessages(msgs, "")

	// The tool handshake must be visible to the model, otherwise it loses the
	// thread of what it already asked for.
	if !strings.Contains(got, "get_weather") {
		t.Errorf("tool call not rendered: %s", got)
	}
	if !strings.Contains(got, "ASSISTANT CALLED TOOL") {
		t.Errorf("assistant tool call marker missing: %s", got)
	}
	if !strings.Contains(got, "TOOL RESULT") {
		t.Errorf("tool result marker missing: %s", got)
	}
	if !strings.Contains(got, "22C") {
		t.Errorf("tool output missing: %s", got)
	}
}

func TestFormatMessagesPlainToolMessage(t *testing.T) {
	// A tool message without tool_calls history still renders its output.
	msgs := []map[string]any{
		{"role": "tool", "name": "read_file", "content": "file contents"},
	}
	got := FormatMessages(msgs, "")
	if !strings.Contains(got, "TOOL RESULT") || !strings.Contains(got, "file contents") {
		t.Errorf("unexpected render: %s", got)
	}
}

// The daemon runs its own built-in tools (web_search, read_file, ...) during a
// turn and emits tool_start for them. Those must NOT be surfaced to a client
// that never declared them, or the client is asked to execute an unknown tool.
func TestDaemonInternalToolsAreFiltered(t *testing.T) {
	clientTools := map[string]bool{"get_weather": true}

	idx := 0
	internal := SSEEvent{Type: "tool_start", ID: "call_1", Name: "web_search",
		Arguments: `{"query":"tokyo weather"}`}
	if got := TranslateToOpenAIChunk(&internal, "m", &idx, clientTools); got != "" {
		t.Errorf("daemon-internal tool must be filtered, got: %s", got)
	}

	declared := SSEEvent{Type: "tool_start", ID: "call_2", Name: "get_weather",
		Arguments: `{"city":"Tokyo"}`}
	if got := TranslateToOpenAIChunk(&declared, "m", &idx, clientTools); got == "" {
		t.Error("client-declared tool must be forwarded")
	}

	// tool_batch announces every tool for the turn, including internal ones.
	batch := SSEEvent{Type: "tool_batch", Calls: []ToolBatchCall{
		{ID: "call_1", Name: "web_search"},
		{ID: "call_2", Name: "get_weather"},
	}}
	if got := TranslateToOpenAIChunk(&batch, "m", &idx, clientTools); got != "" {
		t.Errorf("tool_batch must not be forwarded directly, got: %s", got)
	}

	// nil means "no filtering" for callers that have no tool context.
	if got := TranslateToOpenAIChunk(&internal, "m", &idx, nil); got == "" {
		t.Error("nil clientTools should forward everything")
	}
}

func TestDaemonInternalToolsFilteredAnthropic(t *testing.T) {
	clientTools := map[string]bool{"get_weather": true}
	state := NewAnthropicState()

	internal := SSEEvent{Type: "tool_start", ID: "call_1", Name: "read_file",
		Arguments: `{"file_path":"/etc/hosts"}`}
	if lines := TranslateToAnthropicSSE(&internal, "m", state, clientTools); len(lines) != 0 {
		t.Errorf("daemon-internal tool must be filtered, got: %v", lines)
	}

	declared := SSEEvent{Type: "tool_start", ID: "call_2", Name: "get_weather",
		Arguments: `{"city":"Tokyo"}`}
	if lines := TranslateToAnthropicSSE(&declared, "m", state, clientTools); len(lines) == 0 {
		t.Error("client-declared tool must be forwarded")
	}

	batch := SSEEvent{Type: "tool_batch", Calls: []ToolBatchCall{{Name: "web_search"}}}
	if lines := TranslateToAnthropicSSE(&batch, "m", state, clientTools); len(lines) != 0 {
		t.Errorf("tool_batch must not be forwarded, got: %v", lines)
	}
}

// The CodingPlan catalogue changes over time, so the default model must be read
// from the daemon rather than hardcoded (deepseek-v4-flash no longer exists).
func TestDefaultModel(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "AtomGit-glm5.3-flash"},
		{Name: "AtomGit-qwen3.8-27b", IsDefault: true},
	}
	if got := DefaultModel(providers); got != "AtomGit-qwen3.8-27b" {
		t.Errorf("expected the is_default entry, got %q", got)
	}
	// With no default flag, fall back to the first entry rather than a literal.
	noDefault := []ProviderConfig{{Name: "only-one"}}
	if got := DefaultModel(noDefault); got != "only-one" {
		t.Errorf("expected first entry, got %q", got)
	}
	if got := DefaultModel(nil); got != "" {
		t.Errorf("expected empty for no providers, got %q", got)
	}
}

func TestModelNames(t *testing.T) {
	got := ModelNames([]ProviderConfig{{Name: "b"}, {Name: "a"}, {Name: ""}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("expected sorted non-empty names [a b], got %v", got)
	}
}

// The daemon reports a failed turn via done.stop_reason, NOT an `error` event.
// Treating those as success produced HTTP 200 with empty content, so a 403 from
// upstream looked like a successful empty answer.
func TestFailureStopReasons(t *testing.T) {
	failures := []string{
		"provider_error", "internal_error", "timeout",
		"prompt_rejected", "policy_denied", "rate_limited",
	}
	for _, r := range failures {
		if !IsFailureStopReason(r) {
			t.Errorf("%q should be treated as a failure", r)
		}
	}

	// Loop limits and user interruption are normal terminals, not failures:
	// the agent produced output and merely hit a bound.
	nonFailures := []string{
		"", "stopped", "cancelled", "max_rounds",
		"repeat_loop", "tool_loop_detected", "max_continuations",
	}
	for _, r := range nonFailures {
		if IsFailureStopReason(r) {
			t.Errorf("%q must not be treated as a failure", r)
		}
	}
}

func TestStopReasonHTTPStatus(t *testing.T) {
	cases := map[string]int{
		"provider_error":  502,
		"internal_error":  502,
		"timeout":         504,
		"prompt_rejected": 400,
		"policy_denied":   400,
		"rate_limited":    429,
	}
	for reason, want := range cases {
		if got := StopReasonHTTPStatus(reason); got != want {
			t.Errorf("%s: expected %d, got %d", reason, want, got)
		}
	}
}

// Advisory warnings must not be injected into the response body: the daemon
// sends the same text as a warning AND as done.message for fatal errors, and
// surfacing it as reasoning_content corrupted the stream.
func TestWarningsNotInjectedIntoContent(t *testing.T) {
	idx := 0
	ev := SSEEvent{Type: "warning", Message: "HTTP 403: model is not enabled"}
	if got := TranslateToOpenAIChunk(&ev, "m", &idx, nil); got != "" {
		t.Errorf("warning must not be emitted as content, got: %s", got)
	}

	state := NewAnthropicState()
	if lines := TranslateToAnthropicSSE(&ev, "m", state, nil); len(lines) != 0 {
		t.Errorf("warning must not be emitted as content, got: %v", lines)
	}

	persist := SSEEvent{Type: "persistence_warning", Message: "db write failed"}
	if got := TranslateToOpenAIChunk(&persist, "m", &idx, nil); got != "" {
		t.Errorf("persistence_warning must not be emitted as content, got: %s", got)
	}
}
