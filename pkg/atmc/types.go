package atmc

// --- Auth ---

type LoginStartRequest struct {
	OpenBrowser bool `json:"open_browser"`
}

type LoginStartResponse struct {
	LoginID          string `json:"login_id"`
	URL              string `json:"url"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

type LoginPollResponse struct {
	Status string    `json:"status,omitempty"`
	User   *UserInfo `json:"user,omitempty"`
	Error  string    `json:"error,omitempty"`
}

type AuthStatusResponse struct {
	LoggedIn bool       `json:"logged_in"`
	User     *UserInfo  `json:"user,omitempty"`
	Token    *TokenInfo `json:"token,omitempty"`
}

type UserInfo struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

type TokenInfo struct {
	AccessToken     string `json:"access_token,omitempty"`
	TokenType       string `json:"token_type,omitempty"`
	ExpiresIn       int    `json:"expires_in"`
	HasRefreshToken bool   `json:"has_refresh_token"`
}

// --- Chat ---

// ChatRequest mirrors the daemon v5.1.0 `/chat` body
// (crates/atomcode-daemon/src/lib.rs, `struct ChatRequest`).
//
// Note: the daemon has NO `system` field. A system prompt must be folded into
// `Message` by the caller (see FormatMessages / FormatAnthropicMessages).
// `Stream` is accepted for source compatibility but is not part of the wire
// schema — `/chat` always answers with an SSE stream.
type ChatRequest struct {
	Message      string       `json:"message"`
	Stream       bool         `json:"-"`
	WorkingDir   string       `json:"working_dir,omitempty"`
	Provider     string       `json:"provider,omitempty"`
	SessionID    string       `json:"session_id,omitempty"`
	RequestID    string       `json:"request_id,omitempty"`
	Images       []ImageInput `json:"images,omitempty"`
	ApprovalMode string       `json:"approval_mode,omitempty"`
}

// ImageInput is one attached image for a vision-capable model.
// `Data` is base64 WITHOUT the data-URL prefix.
type ImageInput struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ChatStopRequest is the body of POST /chat/stop.
type ChatStopRequest struct {
	SessionID string `json:"session_id"`
}

// ChatStopResponse is returned by POST /chat/stop.
type ChatStopResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// PermissionDecisionRequest is the body of POST /chat/permission.
// Decision is one of: allow | deny | always_allow | allow_persist.
type PermissionDecisionRequest struct {
	SessionID string `json:"session_id"`
	Decision  string `json:"decision"`
	ToolName  string `json:"tool_name,omitempty"`
}

// PermissionDecisionResponse is returned by POST /chat/permission.
type PermissionDecisionResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// SSEEvent is a single `data:` payload from the daemon `/chat` SSE stream.
//
// It is intentionally a superset of every `ChatEvent` variant in
// crates/atomcode-daemon/src/lib.rs (tag = "type"), because the daemon adds
// new variants over time and unknown ones must not abort the stream.
type SSEEvent struct {
	Type string `json:"type"`

	// text / reasoning / tool_output / command_output
	Content string `json:"content,omitempty"`

	// tool_start / tool_result / tool_progress
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Chunk      string `json:"chunk,omitempty"`
	Output     string `json:"output,omitempty"`
	Progress   string `json:"progress,omitempty"`
	Success    *bool  `json:"success,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`

	// tokens / done
	Prompt     int    `json:"prompt,omitempty"`
	Completion int    `json:"completion,omitempty"`
	Total      int    `json:"total,omitempty"`
	ToolCalls  int    `json:"tool_calls,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`

	// runtime_info
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// error / warning / persistence_warning / rate_limited
	Message          string `json:"message,omitempty"`
	ResetAtDisplay   string `json:"reset_at_display,omitempty"`
	ResetLabel       string `json:"reset_label,omitempty"`
	SecsUntilReset   *int64 `json:"secs_until_reset,omitempty"`
	AutoResuming     *bool  `json:"auto_resuming,omitempty"`

	// permission_request / user_input_request
	ToolName    string `json:"tool_name,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CallID      string `json:"call_id,omitempty"`
	RequestID   uint64 `json:"request_id,omitempty"`

	// artifact_start / artifact_content / artifact_end
	ArtifactType string `json:"artifact_type,omitempty"`
	Language     string `json:"language,omitempty"`
	Title        string `json:"title,omitempty"`

	// tool_batch
	Calls []ToolBatchCall `json:"calls,omitempty"`
}

// ToolBatchCall is one entry of the daemon `tool_batch` SSE event.
type ToolBatchCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// --- Daemon health ---

type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Service string `json:"service,omitempty"`
}

// --- Models ---

// ModelInfo mirrors the daemon v5.1.0 `ModelInfo` (GET /models).
type ModelInfo struct {
	Provider        string   `json:"provider,omitempty"`
	Model           string   `json:"model"`
	ProviderType    string   `json:"provider_type,omitempty"`
	IsDefault       bool     `json:"is_default,omitempty"`
	EffortApplicable bool    `json:"effort_applicable,omitempty"`
	ReasoningEffort *string  `json:"reasoning_effort,omitempty"`
	EffortLevels    []string `json:"effort_levels,omitempty"`

	// Legacy / display fields kept for the dashboard.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ProviderConfig mirrors the daemon v5.1.0 `ProviderInfo`
// (GET /providers -> {"default_provider": "...", "providers": [...]}).
type ProviderConfig struct {
	Name          string `json:"name"`
	Type          string `json:"type,omitempty"`
	Model         string `json:"model"`
	BaseURL       string `json:"base_url,omitempty"`
	ProviderType  string `json:"provider_type,omitempty"`
	IsDefault     bool   `json:"is_default,omitempty"`
	HasAPIKey     bool   `json:"has_api_key,omitempty"`
	RequiresLogin bool   `json:"requires_login,omitempty"`
	SupportsVision bool  `json:"supports_vision,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxTokens     *int   `json:"max_tokens,omitempty"`
}

// ProvidersResponse is the full GET /providers envelope.
type ProvidersResponse struct {
	DefaultProvider string           `json:"default_provider,omitempty"`
	Providers       []ProviderConfig `json:"providers"`
}

// --- CodingPlan ---

type CodingPlanSetupResponse struct {
	Success         bool             `json:"success"`
	ReportText      string           `json:"report_text,omitempty"`
	DefaultProvider string           `json:"default_provider,omitempty"`
	Providers       []ProviderConfig `json:"providers,omitempty"`
	Steps           map[string]any   `json:"steps,omitempty"`
}

// CodingPlanUsageSummaryResponse mirrors daemon v5.1.0
// GET /codingplan/usage/summary (crates/atomcode-daemon/src/api_codingplan.rs).
//
// This replaces the removed `/codingplan/status` endpoint and the previous
// `report_text` string-scraping approach.
type CodingPlanUsageSummaryResponse struct {
	SchemaVersion int                            `json:"schema_version"`
	Available     bool                           `json:"available"`
	Plan          *CodingPlanPlan                `json:"plan,omitempty"`
	PrimaryWindow *CodingPlanQuotaWindow         `json:"primary_window,omitempty"`
	Windows       []CodingPlanQuotaWindow        `json:"windows"`
	QuotaHint     string                         `json:"quota_hint,omitempty"`
}

// CodingPlanPlan is the entitlement half of the usage summary.
type CodingPlanPlan struct {
	Name          string `json:"name"`
	Status        int    `json:"status"`
	ClaimedAt     string `json:"claimed_at,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	TotalDays     int    `json:"total_days"`
	RemainingDays int    `json:"remaining_days"`
}

// CodingPlanQuotaWindow is one rolling quota window.
// Metric is either "calls" (new server schema) or "tokens" (legacy window).
type CodingPlanQuotaWindow struct {
	Metric             string  `json:"metric"`
	WindowHours        int     `json:"window_hours"`
	WindowSizeSeconds  int64   `json:"window_size_seconds"`
	Limit              *int64  `json:"limit,omitempty"`
	Used               *int64  `json:"used,omitempty"`
	Remaining          *int64  `json:"remaining,omitempty"`
	UsagePercent       float64 `json:"usage_percent"`
	RemainingPercent   float64 `json:"remaining_percent"`
	QuotaExhausted     bool    `json:"quota_exhausted"`
	NextResetAt        string  `json:"next_reset_at,omitempty"`
	NextResetDisplay   string  `json:"next_reset_display,omitempty"`
	SecondsUntilReset  int64   `json:"seconds_until_reset"`
	ResetLabel         string  `json:"reset_label,omitempty"`
	UsageDescription   string  `json:"usage_description,omitempty"`
}

// CodingPlanDailyUsageResponse mirrors daemon v5.1.0
// GET /codingplan/usage/daily.
type CodingPlanDailyUsageResponse struct {
	SchemaVersion int                       `json:"schema_version"`
	Available     bool                      `json:"available"`
	Days          int                       `json:"days"`
	StartDate     string                    `json:"start_date"`
	EndDate       string                    `json:"end_date"`
	Models        []string                  `json:"models"`
	Rows          []CodingPlanDailyUsageRow `json:"rows"`
	ModelTokens   map[string]int64          `json:"model_tokens"`
	ModelRequests map[string]int64          `json:"model_requests"`
	TotalTokens   int64                     `json:"total_tokens"`
	TotalRequests int64                     `json:"total_requests"`
}

// CodingPlanDailyUsageRow is one day of the daily usage series.
type CodingPlanDailyUsageRow struct {
	Date          string           `json:"date"`
	ModelTokens   map[string]int64 `json:"model_tokens"`
	ModelRequests map[string]int64 `json:"model_requests"`
	TotalTokens   int64            `json:"total_tokens"`
	TotalRequests int64            `json:"total_requests"`
}
