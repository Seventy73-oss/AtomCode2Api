package anthropic

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vibe-coding-labs/AtomCode2API/pkg/atmc"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/store"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/toolbridge"
)

// Handler implements the Anthropic Messages API.
type Handler struct {
	Client   *atmc.Client
	store    *store.Store
	sessions *sessionTracker
}

// NewHandler creates a new Anthropic Messages API handler.
func NewHandler(client *atmc.Client, s *store.Store) *Handler {
	return &Handler{
		Client:   client,
		store:    s,
		sessions: newSessionTracker(30 * time.Minute),
	}
}

// RequestIDKey is the context key for request ID.
type RequestIDKey struct{}

// WithRequestID stores the request ID in the request.
func WithRequestID(r *http.Request, id uint64) *http.Request {
	r.Header.Set("X-Request-ID", strconv.FormatUint(id, 10))
	return r
}

// RegisterRoutes registers Anthropic Messages API endpoints.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/messages", h.handleMessages)
}

func (h *Handler) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key")
		w.WriteHeader(200)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}

	var req MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Extract system prompt from system field or first message
	systemPrompt := ""
	if len(req.System) > 0 {
		var blocks []ContentBlock
		if err := json.Unmarshal(req.System, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					systemPrompt += b.Text
				}
			}
		} else {
			// Try as plain string
			var s string
			if json.Unmarshal(req.System, &s) == nil {
				systemPrompt = s
			}
		}
	}

	// Format messages for daemon
	formatted, sp := FormatAnthropicMessages(req.Messages)
	if systemPrompt == "" {
		systemPrompt = sp
	}

	if formatted == "" {
		writeError(w, 400, "no messages to process")
		return
	}

	// Resolve provider from model.
	//
	// Reject unknown models: the daemon silently falls back to its default model
	// when no provider is supplied, so a typo would otherwise appear to work
	// while running a different model entirely.
	provider := ""
	if req.Model != "" {
		providers, err := h.Client.ListProviders()
		if err == nil && len(providers) > 0 {
			provider = atmc.FindProviderForModel(providers, req.Model)
			if provider == "" {
				writeError(w, 404, fmt.Sprintf(
					"model %q not found. Available: %s (the daemon would silently "+
						"fall back to its default model, so this request was rejected)",
					req.Model, strings.Join(atmc.ModelNames(providers), ", ")))
				return
			}
		}
	}

	// Client-side tools. The daemon ignores a `tools` field, so definitions are
	// injected into the prompt as a text protocol and the reply is converted
	// back into standard tool_use blocks (see pkg/toolbridge).
	tools := toolbridge.ParseAnthropicTools(json.RawMessage(req.Tools))
	required := anthropicToolChoiceRequired(req.ToolChoice)
	if anthropicToolChoiceNone(req.ToolChoice) {
		tools = nil
	}
	if proto := toolbridge.ProtocolPrompt(tools, required); proto != "" {
		formatted = proto + "\n\n" + formatted
	}

	// Build conversation key & session tracking. The declared tool set is part
	// of the key so a conversation that gains tools does not reuse a stale
	// daemon session.
	msgs := messagesToMap(req.Messages)
	convKey := atmc.ConversationKey(msgs, systemPrompt+anthropicToolKeySuffix(tools))
	daemonSessionID := h.sessions.get(convKey)

	log.Printf("anthropic messages: model=%s stream=%t provider=%s messages=%d system=%t tools=%d sid=%s",
		req.Model, req.Stream, strOr(provider, "(auto)"), len(req.Messages),
		systemPrompt != "", len(tools), strOr(daemonSessionID, "(new)"))

	if req.Stream {
		h.handleStreamChat(w, r, &req, formatted, provider, systemPrompt, daemonSessionID, convKey, tools)
	} else {
		h.handleNonStreamChat(w, r, &req, formatted, provider, systemPrompt, daemonSessionID, convKey, tools)
	}
}

// anthropicToolKeySuffix folds the declared tool names into the conversation key.
func anthropicToolKeySuffix(tools []toolbridge.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return "|tools:" + strings.Join(names, ",")
}

// anthropicToolChoiceNone reports whether the client disabled tool use.
func anthropicToolChoiceNone(raw jsonField) bool {
	if len(raw) == 0 {
		return false
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Type == "none"
	}
	return false
}

// anthropicToolChoiceRequired reports tool_choice {type:"any"} / {type:"tool"}.
func anthropicToolChoiceRequired(raw jsonField) bool {
	if len(raw) == 0 {
		return false
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Type == "any" || obj.Type == "tool"
	}
	return false
}

func (h *Handler) handleNonStreamChat(w http.ResponseWriter, r *http.Request, req *MessageRequest,
	daemonMsg, provider, system, sessionID, convKey string, tools []toolbridge.Tool) {

	// The daemon v5.1.0 /chat body has no `system` field — the system prompt is
	// already folded into daemonMsg by atmc.FormatAnthropicMessages.
	daemonReq := &atmc.ChatRequest{
		Message:   daemonMsg,
		Stream:    true,
		Provider:  provider,
		SessionID: sessionID,
	}

	var events []atmc.SSEEvent
	var lastSessionID string
	ch, err := h.Client.ChatStream(daemonReq)
	if err != nil {
		writeError(w, 502, fmt.Sprintf("daemon error: %v", err))
		return
	}
	for ev := range ch {
		if ev.Type == "done" {
			lastSessionID = ev.SessionID
			break
		}
		events = append(events, ev)
	}

	if lastSessionID != "" && lastSessionID != sessionID {
		h.sessions.set(convKey, lastSessionID)
	}

	resp := translateToAnthropicResponse(events, req.Model)

	// Convert the text protocol back into real tool_use blocks.
	if len(tools) > 0 {
		clean, calls := toolbridge.Parse(extractResponseText(resp), toolbridge.Names(tools))
		if len(calls) > 0 {
			setResponseText(resp, clean)
			appendToolUseBlocks(resp, calls)
		} else {
			setResponseText(resp, clean)
		}
	}

	writeJSON(w, 200, resp)
}

// extractResponseText concatenates the text blocks of a non-streaming response.
func extractResponseText(resp map[string]any) string {
	blocks, _ := resp["content"].([]any)
	var b strings.Builder
	for _, raw := range blocks {
		if blk, ok := raw.(map[string]any); ok && blk["type"] == "text" {
			if t, ok := blk["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// setResponseText replaces the text content of a non-streaming response while
// preserving any non-text blocks (thinking, tool_use).
func setResponseText(resp map[string]any, text string) {
	blocks, _ := resp["content"].([]any)
	out := make([]any, 0, len(blocks)+1)
	if strings.TrimSpace(text) != "" {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	for _, raw := range blocks {
		if blk, ok := raw.(map[string]any); ok && blk["type"] == "text" {
			continue
		}
		out = append(out, raw)
	}
	resp["content"] = out
}

// appendToolUseBlocks adds tool_use content blocks and sets stop_reason.
func appendToolUseBlocks(resp map[string]any, calls []toolbridge.ToolCall) {
	blocks, _ := resp["content"].([]any)
	for i, c := range calls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("toolu_%s_%d", c.Name, i)
		}
		var input any = map[string]any{}
		if err := json.Unmarshal([]byte(c.Arguments), &input); err != nil {
			input = map[string]any{}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  c.Name,
			"input": input,
		})
	}
	resp["content"] = blocks
	resp["stop_reason"] = "tool_use"
}

func (h *Handler) handleStreamChat(w http.ResponseWriter, r *http.Request, req *MessageRequest,
	daemonMsg, provider, system, sessionID, convKey string, tools []toolbridge.Tool) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(200)

	// The daemon v5.1.0 /chat body has no `system` field — the system prompt is
	// already folded into daemonMsg by atmc.FormatAnthropicMessages.
	daemonReq := &atmc.ChatRequest{
		Message:   daemonMsg,
		Stream:    true,
		Provider:  provider,
		SessionID: sessionID,
	}

	ch, err := h.Client.ChatStream(daemonReq)
	if err != nil {
		fmt.Fprintf(w, "data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"%s\"}}\n\n", err.Error())
		flusher.Flush()
		return
	}

	state := atmc.NewAnthropicState()
	var lastSessionID string
	hasSentStop := false

	// Bridge state: assistant text is buffered so a `<tool_call>` protocol block
	// can be withheld and emitted as proper tool_use content blocks instead.
	bridging := len(tools) > 0
	validNames := toolbridge.Names(tools)
	var bridgeBuf strings.Builder
	emittedToolUse := false

	send := func(line string) {
		if strings.Contains(line, `"type":"message_stop"`) {
			hasSentStop = true
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}

	// emitToolUse opens a tool_use block and streams the parsed calls into it.
	emitToolUse := func(calls []toolbridge.ToolCall) {
		if len(calls) == 0 {
			return
		}
		if !state.HasSentStart {
			state.HasSentStart = true
			send(fmt.Sprintf(`{"type":"message_start","message":{"id":%s,"type":"message","role":"assistant","content":[],"model":%s,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}},"model":%s}`,
				jsonStr(state.MessageID), jsonStr(req.Model), jsonStr(req.Model)))
		}
		for _, c := range calls {
			if state.CurrentBlock != "" {
				send(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex))
				state.ContentIndex++
			}
			state.CurrentBlock = "tool_use"
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("toolu_%s_%d", c.Name, state.ContentIndex)
			}
			send(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
				state.ContentIndex, jsonStr(id), jsonStr(c.Name)))
			send(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
				state.ContentIndex, jsonStr(c.Arguments)))
			emittedToolUse = true
		}
	}

	for ev := range ch {
		if ev.Type == "done" {
			lastSessionID = ev.SessionID
			if bridging && bridgeBuf.Len() > 0 {
				buf := bridgeBuf.String()
				emit, _ := toolbridge.SplitStreamBuffer(buf)
				if emit != "" {
					for _, line := range atmc.TranslateToAnthropicSSE(&atmc.SSEEvent{Type: "text", Content: emit}, req.Model, state, validNames) {
						send(line)
					}
				}
				_, calls := toolbridge.Parse(buf, validNames)
				emitToolUse(calls)
				bridgeBuf.Reset()
			}
			if emittedToolUse {
				// Anthropic signals a tool call with stop_reason "tool_use".
				if state.CurrentBlock != "" {
					send(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ContentIndex))
					state.CurrentBlock = ""
				}
				send(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{}}`)
				send(`{"type":"message_stop"}`)
				hasSentStop = true
			} else if !hasSentStop {
				lines := atmc.TranslateToAnthropicSSE(&ev, req.Model, state, validNames)
				for _, line := range lines {
					send(line)
				}
				hasSentStop = true
			}
			flusher.Flush()
			break
		}

		// Buffer assistant text while bridging.
		if bridging && ev.Type == "text" {
			bridgeBuf.WriteString(ev.Content)
			buf := bridgeBuf.String()
			if toolbridge.ContainsCompleteToolCall(buf) {
				emit, _ := toolbridge.SplitStreamBuffer(buf)
				if emit != "" {
					for _, line := range atmc.TranslateToAnthropicSSE(&atmc.SSEEvent{Type: "text", Content: emit}, req.Model, state, validNames) {
						send(line)
					}
				}
				_, calls := toolbridge.Parse(buf, validNames)
				emitToolUse(calls)
				bridgeBuf.Reset()
			} else if emit, _ := toolbridge.SplitStreamBuffer(buf); emit != "" {
				for _, line := range atmc.TranslateToAnthropicSSE(&atmc.SSEEvent{Type: "text", Content: emit}, req.Model, state, validNames) {
					send(line)
				}
				rest := buf[len(emit):]
				bridgeBuf.Reset()
				bridgeBuf.WriteString(rest)
			}
			continue
		}

		// While bridging, the daemon's `tokens` event would emit a stop_reason
		// of "end_turn" via the generic translator. That contradicts the
		// "tool_use" we emit at the end, and Anthropic clients honour the last
		// message_delta — so suppress the intermediate stop_reason and forward
		// only the usage numbers.
		if bridging && ev.Type == "tokens" {
			send(fmt.Sprintf(`{"type":"message_delta","delta":{},"usage":{"output_tokens":%d,"input_tokens":%d}}`,
				ev.Completion, ev.Prompt))
			continue
		}

		lines := atmc.TranslateToAnthropicSSE(&ev, req.Model, state, validNames)
		for _, line := range lines {
			send(line)
		}
	}

	if !hasSentStop {
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}

	if lastSessionID != "" && lastSessionID != sessionID {
		h.sessions.set(convKey, lastSessionID)
	}
}

// jsonStr marshals a string into a JSON literal for hand-built SSE payloads.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ─── Translation helpers ────────────────────────────────────────────────────

func translateToAnthropicResponse(events []atmc.SSEEvent, model string) map[string]any {
	resp := map[string]any{
		"id":         fmt.Sprintf("msg_%x", time.Now().UnixNano()),
		"type":       "message",
		"role":       "assistant",
		"model":      model,
		"content":    []any{},
		"stop_reason":    "end_turn",
		"stop_sequence":  nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}

	var contentBlocks []any
	textContent := ""
	hasToolUse := false

	for _, ev := range events {
		switch ev.Type {
		case "text":
			textContent += ev.Content
		case "reasoning":
			// Add thinking block
			contentBlocks = append(contentBlocks, map[string]any{
				"type":     "thinking",
				"thinking": ev.Content,
				"signature": "",
			})
		case "tool_start":
			hasToolUse = true
			var input any = ev.Arguments
			var parsed any
			if json.Unmarshal([]byte(ev.Arguments), &parsed) == nil {
				input = parsed
			}
			contentBlocks = append(contentBlocks, map[string]any{
				"type":  "tool_use",
				"id":    ev.ID,
				"name":  ev.Name,
				"input": input,
			})
		case "tokens":
			resp["usage"] = map[string]any{
				"input_tokens":  ev.Prompt,
				"output_tokens": ev.Completion,
			}
		case "error":
			resp["stop_reason"] = "error"
			resp["error"] = map[string]any{
				"type":    "api_error",
				"message": ev.Message,
			}
		}
	}

	if textContent != "" && !hasToolUse {
		// Single text block
		if len(contentBlocks) == 0 {
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": textContent,
			})
		} else {
			contentBlocks = append([]any{map[string]any{
				"type": "text",
				"text": textContent,
			}}, contentBlocks...)
		}
	}

	resp["content"] = contentBlocks
	return resp
}

// ─── Session Tracker ─────────────────────────────────────────────────────────

type sessionEntry struct {
	sessionID string
	updatedAt time.Time
}

type sessionTracker struct {
	sessions map[string]sessionEntry
	ttl      time.Duration
}

func newSessionTracker(ttl time.Duration) *sessionTracker {
	st := &sessionTracker{
		sessions: make(map[string]sessionEntry),
		ttl:      ttl,
	}
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			st.cleanup()
		}
	}()
	return st
}

func (st *sessionTracker) get(key string) string {
	entry, ok := st.sessions[key]
	if !ok {
		return ""
	}
	if time.Since(entry.updatedAt) > st.ttl {
		delete(st.sessions, key)
		return ""
	}
	return entry.sessionID
}

func (st *sessionTracker) set(key, sessionID string) {
	st.sessions[key] = sessionEntry{
		sessionID: sessionID,
		updatedAt: time.Now(),
	}
}

func (st *sessionTracker) cleanup() {
	now := time.Now()
	for k, v := range st.sessions {
		if now.Sub(v.updatedAt) > st.ttl {
			delete(st.sessions, k)
		}
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	data, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	w.Write(data)
}

func strOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func messagesToMap(msgs []Message) []map[string]any {
	result := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		item := map[string]any{
			"role":    m.Role,
			"content": contentBlocksToText(m.Content),
		}
		result = append(result, item)
	}
	return result
}

func contentBlocksToText(blocks []ContentBlock) string {
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "thinking":
			texts = append(texts, b.Thinking)
		case "tool_use":
			texts = append(texts, fmt.Sprintf("[tool_use: %s]", b.Name))
		case "tool_result":
			if b.Content != nil {
				continue
			}
		}
	}
	return strings.Join(texts, "\n")
}