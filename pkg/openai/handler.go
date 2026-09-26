package openai

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vibe-coding-labs/AtomCode2API/pkg/atmc"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/store"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/toolbridge"
)

// Server implements the OpenAI-compatible HTTP API.
type Server struct {
	Client  *atmc.Client
	store   *store.Store
	sessions *SessionTracker
}

// NewServer creates a new OpenAI-compatible proxy server.
func NewServer(client *atmc.Client, s *store.Store) *Server {
	return &Server{
		Client:   client,
		store:    s,
		sessions: NewSessionTracker(30 * time.Minute),
	}
}

// RegisterRoutes registers all OpenAI-compatible endpoints on the mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/web-search", s.handleWebSearch)
	mux.HandleFunc("/v1/rerank", s.handleRerank)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/health", s.handleHealth)
}

// ─── Helper ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	data := NewErrorResponse(code, msg)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	w.Write(data)
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.WriteHeader(200)
		return false
	}
	if r.Method != method {
		writeError(w, 405, "method not allowed")
		return false
	}
	return true
}

// ─── Endpoints ───────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(200)
		return
	}

	h, err := s.Client.Health()
	daemonOK := err == nil && h.Status == "ok"

	auth, authErr := s.Client.AuthStatus()
	loggedIn := authErr == nil && auth.LoggedIn

	status := "ok"
	if !daemonOK {
		status = "degraded"
	}

	daemonVersion := "?"
	if h != nil && h.Version != "" {
		daemonVersion = h.Version
	}
	resp := map[string]any{
		"status":   status,
		"service":  "atomcode-2api",
		"daemon": map[string]any{
			"connected": daemonOK,
			"version":   daemonVersion,
		},
		"logged_in": loggedIn,
		"uptime":    time.Now().Unix(),
	}
	if daemonOK && loggedIn && auth.User != nil {
		resp["user"] = auth.User
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	models, err := s.Client.ListModels()
	if err != nil {
		log.Printf("openai: list models error: %v", err)
		writeError(w, 502, fmt.Sprintf("daemon error: %v", err))
		return
	}

	writeJSON(w, 200, TranslateModels(models))
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Parse messages
	var messages []map[string]any
	if err := json.Unmarshal(req.Messages, &messages); err != nil {
		writeError(w, 400, "invalid messages format")
		return
	}

	// Extract system prompt + filter
	systemPrompt := ""
	var remaining []map[string]any
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == "system" {
			if content, ok := m["content"].(string); ok {
				systemPrompt = content
			}
		} else {
			remaining = append(remaining, m)
		}
	}

	if len(remaining) == 0 {
		writeError(w, 400, "no messages to process")
		return
	}

	// Resolve provider from model name.
	//
	// An unknown model must be rejected rather than passed through: the daemon
	// silently falls back to its default model when it receives no provider, so
	// a typo (or a stale hardcoded name) would otherwise "succeed" while quietly
	// running a completely different model.
	provider := ""
	if req.Model != "" {
		providers, err := s.Client.ListProviders()
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

	// Client-side tools. The daemon ignores a `tools` field, so the definitions
	// are injected into the prompt as a text protocol and the model's reply is
	// converted back into standard `tool_calls` (see pkg/toolbridge).
	tools := toolbridge.ParseOpenAITools(req.Tools)
	required := toolChoiceRequired(req.ToolChoice)
	if toolbridge.ToolChoiceNone(req.ToolChoice) {
		tools = nil
	}

	// Build daemon message
	daemonMessage := atmc.FormatMessages(remaining, systemPrompt)
	if proto := toolbridge.ProtocolPrompt(tools, required); proto != "" {
		daemonMessage = proto + "\n\n" + daemonMessage
	}

	// Session tracking. Include the tool set in the key so that a conversation
	// that gains or loses tools does not reuse a stale daemon session.
	convKey := atmc.ConversationKey(remaining, systemPrompt+toolKeySuffix(tools))
	daemonSessionID := s.sessions.Get(convKey)

	log.Printf("openai chat: model=%s stream=%t provider=%s messages=%d system=%t tools=%d sid=%s",
		req.Model, req.Stream, strOr(provider, "(auto)"), len(remaining),
		systemPrompt != "", len(tools), strOr(daemonSessionID, "(new)"))

	if req.Stream {
		s.handleStreamChat(w, r, &req, daemonMessage, provider, systemPrompt, daemonSessionID, convKey, tools)
	} else {
		s.handleNonStreamChat(w, r, &req, daemonMessage, provider, systemPrompt, daemonSessionID, convKey, tools)
	}
}

// toolKeySuffix folds the declared tool names into the conversation key.
func toolKeySuffix(tools []toolbridge.Tool) string {
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

// toolChoiceRequired reports whether the client demands a tool call this turn.
func toolChoiceRequired(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "required" || s == "any"
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Type == "any"
	}
	return false
}

func (s *Server) handleNonStreamChat(w http.ResponseWriter, r *http.Request, req *ChatRequest,
	daemonMsg, provider, system, sessionID, convKey string, tools []toolbridge.Tool) {

	// The daemon v5.1.0 /chat body has no `system` field — `system` is already
	// folded into daemonMsg by atmc.FormatMessages.
	daemonReq := &atmc.ChatRequest{
		Message:   daemonMsg,
		Stream:    true,
		Provider:  provider,
		SessionID: sessionID,
	}

	var events []atmc.SSEEvent
	var lastSessionID string
	var stopReason, stopMessage string
	ch, err := s.Client.ChatStream(daemonReq)
	if err != nil {
		writeError(w, 502, fmt.Sprintf("daemon chat error: %v", err))
		return
	}

	for ev := range ch {
		if ev.Type == "done" {
			lastSessionID = ev.SessionID
			stopReason = ev.StopReason
			stopMessage = ev.Message
			break
		}
		events = append(events, ev)
	}

	// Persist session for multi-turn context
	if lastSessionID != "" && lastSessionID != sessionID {
		s.sessions.Set(convKey, lastSessionID)
	}

	// The daemon signals a failed turn through done.stop_reason rather than an
	// `error` event. Without this check the client received HTTP 200 with empty
	// content and finish_reason "stop", i.e. a silent failure.
	if atmc.IsFailureStopReason(stopReason) {
		writeError(w, atmc.StopReasonHTTPStatus(stopReason), fmt.Sprintf(
			"daemon turn failed (%s): %s", stopReason, strOr(stopMessage, "no detail")))
		return
	}

	resp := TranslateToOpenAIResponse(events, req.Model, toolbridge.Names(tools))

	// Convert the text protocol back into real tool_calls.
	if len(tools) > 0 {
		text := resp.Choices[0].Message.Content
		clean, calls := toolbridge.Parse(text, toolbridge.Names(tools))
		resp.Choices[0].Message.Content = clean
		if len(calls) > 0 {
			resp.Choices[0].Message.ToolCalls = toOpenAIToolCalls(calls)
			resp.Choices[0].FinishReason = strPtr("tool_calls")
		}
	}

	writeJSON(w, 200, resp)
}

// toOpenAIToolCalls converts parsed bridge calls into the OpenAI wire shape.
func toOpenAIToolCalls(calls []toolbridge.ToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for i, c := range calls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", c.Name, i)
		}
		idx := i
		out = append(out, ToolCall{
			Index: &idx,
			ID:    id,
			Type:  "function",
			Function: ToolCallFunction{
				Name:      c.Name,
				Arguments: c.Arguments,
			},
		})
	}
	return out
}

func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, req *ChatRequest,
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

	// `system` is already folded into daemonMsg (see atmc.FormatMessages).
	daemonReq := &atmc.ChatRequest{
		Message:   daemonMsg,
		Stream:    true,
		Provider:  provider,
		SessionID: sessionID,
	}

	ch, err := s.Client.ChatStream(daemonReq)
	if err != nil {
		errJSON := fmt.Sprintf(`data: {"error":"%s"}\n\ndata: [DONE]\n\n`, err.Error())
		fmt.Fprint(w, errJSON)
		flusher.Flush()
		return
	}

	toolIdx := 0
	hasToolUse := false
	var lastSessionID string

	// Bridge state: text is buffered so a `<tool_call>` protocol block can be
	// withheld from the client and converted into real `tool_calls` instead.
	var bridgeBuf strings.Builder
	bridging := len(tools) > 0
	validNames := toolbridge.Names(tools)
	var emittedToolCalls bool

	emitText := func(text string) {
		if text == "" {
			return
		}
		delta := fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"index":0}]}`, jsonString(text))
		if full := atmc.BuildOpenAIFullChunk(delta, req.Model); full != "" {
			fmt.Fprintf(w, "data: %s\n\n", full)
			flusher.Flush()
		}
	}

	// flushBridge parses the accumulated buffer and emits any tool calls.
	// Returns true when at least one call was emitted.
	flushBridge := func(final bool) bool {
		if !bridging || bridgeBuf.Len() == 0 {
			return false
		}
		buf := bridgeBuf.String()
		emit, held := toolbridge.SplitStreamBuffer(buf)
		if emit != "" {
			emitText(emit)
			// Keep only the withheld portion for the next round.
			bridgeBuf.Reset()
			if held {
				bridgeBuf.WriteString(buf[len(emit):])
			}
			buf = bridgeBuf.String()
		}
		if !final && !toolbridge.ContainsCompleteToolCall(buf) {
			return false
		}
		_, calls := toolbridge.Parse(buf, validNames)
		if len(calls) == 0 {
			return false
		}
		bridgeBuf.Reset()
		for i, c := range calls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", c.Name, i)
			}
			delta := fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}]},"index":0}]}`,
				i, jsonString(id), jsonString(c.Name), jsonString(c.Arguments))
			if full := atmc.BuildOpenAIFullChunk(delta, req.Model); full != "" {
				fmt.Fprintf(w, "data: %s\n\n", full)
				flusher.Flush()
			}
		}
		return true
	}

	for ev := range ch {
		if ev.Type == "done" {
			lastSessionID = ev.SessionID

			// A failed turn arrives as done.stop_reason, not an `error` event.
			// Report it as an SSE error frame with finish_reason "error" instead
			// of a misleading empty success.
			if atmc.IsFailureStopReason(ev.StopReason) {
				msg := fmt.Sprintf("daemon turn failed (%s): %s", ev.StopReason, strOr(ev.Message, "no detail"))
				log.Printf("openai stream: %s", msg)
				fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"error":{"message":%s,"type":"upstream_error","code":%d}}`,
					jsonString(msg), atmc.StopReasonHTTPStatus(ev.StopReason)))
				flusher.Flush()
				fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
					`{"id":"chatcmpl-atomcode","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"delta":{},"finish_reason":"error","index":0}]}`,
					time.Now().Unix(), req.Model))
				flusher.Flush()
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				break
			}

			if bridging {
				// Final flush: whatever is buffered is the model's last word.
				if flushBridge(true) {
					emittedToolCalls = true
				} else if tail := bridgeBuf.String(); tail != "" {
					_, calls := toolbridge.Parse(tail, validNames)
					if len(calls) > 0 {
						emittedToolCalls = true
					} else {
						emitText(tail)
					}
					bridgeBuf.Reset()
				}
			}
			// Send appropriate finish_reason chunk before [DONE]
			finishReason := "stop"
			if hasToolUse || emittedToolCalls {
				finishReason = "tool_calls"
			}
			finishChunk := fmt.Sprintf(`{"id":"chatcmpl-atomcode","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"delta":{},"finish_reason":"%s","index":0}]}`, time.Now().Unix(), req.Model, finishReason)
			fmt.Fprintf(w, "data: %s\n\n", finishChunk)
			flusher.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		if ev.Type == "tool_start" {
			hasToolUse = true
		}

		// Buffer assistant text while bridging so protocol blocks are hidden.
		if bridging && ev.Type == "text" {
			bridgeBuf.WriteString(ev.Content)
			if toolbridge.ContainsCompleteToolCall(bridgeBuf.String()) {
				if flushBridge(false) {
					emittedToolCalls = true
				}
			} else {
				// Emit the safe prefix, retaining any partial marker.
				if emit, _ := toolbridge.SplitStreamBuffer(bridgeBuf.String()); emit != "" {
					emitText(emit)
					rest := bridgeBuf.String()[len(emit):]
					bridgeBuf.Reset()
					bridgeBuf.WriteString(rest)
				}
			}
			continue
		}
		if bridging && (ev.Type == "reasoning") {
			// Reasoning stays visible; it never contains the protocol block.
			delta := atmc.TranslateToOpenAIChunk(&ev, req.Model, &toolIdx, validNames)
			if delta != "" && delta != "__DONE__" {
				if full := atmc.BuildOpenAIFullChunk(delta, req.Model); full != "" {
					fmt.Fprintf(w, "data: %s\n\n", full)
					flusher.Flush()
				}
			}
			continue
		}

		delta := atmc.TranslateToOpenAIChunk(&ev, req.Model, &toolIdx, validNames)
		if delta == "" {
			continue
		}
		if delta == "__DONE__" {
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		full := atmc.BuildOpenAIFullChunk(delta, req.Model)
		if full != "" {
			fmt.Fprintf(w, "data: %s\n\n", full)
			flusher.Flush()
		}
	}

	// Persist session for multi-turn context
	if lastSessionID != "" && lastSessionID != sessionID {
		s.sessions.Set(convKey, lastSessionID)
	}
}

// ─── Session Tracker ─────────────────────────────────────────────────────────

type SessionTracker struct {
	sessions map[string]sessionEntry
	ttl      time.Duration
}

type sessionEntry struct {
	sessionID string
	updatedAt time.Time
}

func NewSessionTracker(ttl time.Duration) *SessionTracker {
	st := &SessionTracker{
		sessions: make(map[string]sessionEntry),
		ttl:      ttl,
	}
	// Start cleanup goroutine
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			st.cleanup()
		}
	}()
	return st
}

func (st *SessionTracker) Get(key string) string {
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

func (st *SessionTracker) Set(key, sessionID string) {
	st.sessions[key] = sessionEntry{
		sessionID: sessionID,
		updatedAt: time.Now(),
	}
}

func (st *SessionTracker) cleanup() {
	now := time.Now()
	for k, v := range st.sessions {
		if now.Sub(v.updatedAt) > st.ttl {
			delete(st.sessions, k)
		}
	}
}

// ─── Small helpers ───────────────────────────────────────────────────────────

// jsonString marshals a string into a JSON literal for hand-built SSE payloads.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func strOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// mapValue is not used; inline instead.