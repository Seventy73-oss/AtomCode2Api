package dashboard

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vibe-coding-labs/AtomCode2API/pkg/auth"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/atmc"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/keepalive"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/proxy"
	"github.com/vibe-coding-labs/AtomCode2API/pkg/store"
)

//go:embed static
var staticFiles embed.FS

type Handler struct {
	store    *store.Store
	staticFS fs.FS
	keeper   *keepalive.Keeper
	daemon   *atmc.Client
	Version  string
}

func NewHandler(s *store.Store, staticFS fs.FS, k *keepalive.Keeper, daemonClient *atmc.Client) *Handler {
	if staticFS == nil {
		sub, _ := fs.Sub(staticFiles, "static")
		staticFS = sub
	}
	return &Handler{store: s, staticFS: staticFS, keeper: k, daemon: daemonClient}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// api guards every /api/* handler against a nil store. Previously the store
	// could fail to open (e.g. CGO-less build of mattn/go-sqlite3) and the very
	// first request dereferenced nil, panicking the server.
	api := func(pattern string, fn http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if h.store == nil {
				writeError(w, http.StatusServiceUnavailable,
					"store unavailable: database failed to open")
				return
			}
			fn(w, r)
		})
	}

	api("/api/auth/status", h.handleAuthStatus)
	api("/api/auth/setup", h.handleAuthSetup)
	api("/api/auth/login", h.handleAuthLogin)
	api("/api/auth/change-password", h.handleChangePassword)

	api("/api/accounts", h.handleAccounts)
	api("/api/accounts/", h.handleAccountAction)
	api("/api/accounts-export", h.handleExportAccounts)
	api("/api/accounts-import", h.handleImportAccounts)
	api("/api/accounts/batch-import", h.handleBatchImport)
	api("/api/accounts-auto-login", h.handleAutoLogin)
	api("/api/accounts-clear-all", h.handleClearAllAccounts)
	api("/api/stats", h.handleStats)
	api("/api/settings", h.handleSettings)
	api("/api/errors", h.handleErrors)
	api("/api/models", h.handleModels)
	api("/api/models/catalog", h.handleModelsCatalog)
	api("/api/codingplan/status", h.handleCodingPlanStatus)
	api("/api/codingplan/daily", h.handleCodingPlanDaily)
	mux.HandleFunc("/api/health", h.handleHealth)
	api("/api/github-stars", h.handleGitHubStars)

	api("/api/browser-login", h.handleBrowserLogin)
	api("/api/oauth-callback", h.handleOAuthCallback)
	api("/api/oauth-submit", h.handleOAuthSubmit)
	api("/api/qr-login/init", h.handleQRLoginInit)
	api("/api/qr-login/status", h.handleQRLoginStatus)
}

const jwtSecretKey = "auth_jwt_secret"
const defaultJWTExpiry = 24 * time.Hour

// ─── Auth ────────────────────────────────────────────────────────────────────

func (h *Handler) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	hash := h.store.GetSetting("auth_password_hash")
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": hash != "",
	})
}

func (h *Handler) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	if h.store.GetSetting("auth_password_hash") != "" {
		writeError(w, 409, "root password already initialized"); return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !readJSONBody(w, r, &body) { return }
	if len(body.Password) < 6 { writeError(w, 400, "密码长度不能少于 6 位"); return }

	hash, err := auth.HashPassword(body.Password)
	if err != nil { writeError(w, 500, "密码加密失败"); return }
	if err := h.store.SetSetting("auth_password_hash", hash); err != nil { writeError(w, 500, "保存密码失败"); return }

	if h.store.GetSetting(jwtSecretKey) == "" {
		h.store.SetSetting(jwtSecretKey, generateRandomHex(32))
	}
	token, err := h.issueJWT()
	if err != nil { writeError(w, 500, "生成 token 失败"); return }

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token})
}

func (h *Handler) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	hash := h.store.GetSetting("auth_password_hash")
	if hash == "" { writeError(w, 409, "root password not initialized"); return }

	var body struct{ Password string `json:"password"` }
	if !readJSONBody(w, r, &body) { return }
	if !auth.CheckPassword(body.Password, hash) { writeError(w, 401, "密码错误"); return }

	token, err := h.issueJWT()
	if err != nil { writeError(w, 500, "生成 token 失败"); return }
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token})
}

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	hash := h.store.GetSetting("auth_password_hash")
	if hash == "" { writeError(w, 409, "root password not initialized"); return }

	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !readJSONBody(w, r, &body) { return }
	if !auth.CheckPassword(body.OldPassword, hash) { writeError(w, 401, "原密码错误"); return }
	if len(body.NewPassword) < 6 { writeError(w, 400, "新密码长度不能少于 6 位"); return }

	newHash, err := auth.HashPassword(body.NewPassword)
	if err != nil { writeError(w, 500, "密码加密失败"); return }
	if err := h.store.SetSetting("auth_password_hash", newHash); err != nil { writeError(w, 500, "保存密码失败"); return }
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) issueJWT() (string, error) {
	secret := h.store.GetSetting(jwtSecretKey)
	if secret == "" { return "", fmt.Errorf("JWT secret not configured") }
	// Issue a standard HS256 JWT so signing and verification share one
	// implementation. Tokens minted by the legacy HMAC scheme still validate.
	return auth.NewJWTManager(secret).GenerateToken("root", "admin")
}

// resolveDefaultModel returns the daemon's current default model name, falling
// back to the first available one. Replaces hardcoded names such as
// "deepseek-v4-flash", which no longer exist in the CodingPlan catalogue and
// would silently resolve to whatever the daemon defaults to.
func (h *Handler) resolveDefaultModel() string {
	if h.daemon != nil {
		if providers, err := h.daemon.ListProviders(); err == nil {
			if m := atmc.DefaultModel(providers); m != "" {
				return m
			}
		}
	}
	return "AtomGit-qwen3.8-27b"
}

func generateRandomHex(n int) string {
	b := make([]byte, n)
	io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}

// ─── Accounts ────────────────────────────────────────────────────────────────

func (h *Handler) handleAccounts(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	switch r.Method {
	case http.MethodGet: h.listAccounts(w, r)
	case http.MethodPost: h.addAccount(w, r)
	default: writeError(w, 405, "method not allowed")
	}
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.store.ListAccounts()
	if err != nil { writeError(w, 500, err.Error()); return }
	if accounts == nil { accounts = []store.AccountInfo{} }
	for i := range accounts {
			accounts[i].ActiveSessions = proxy.GetActiveSessions(accounts[i].UserID)
		}
		h.store.FillAccountStats(accounts)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (h *Handler) addAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nickname     string `json:"nickname"`
		PtKey        string `json:"pt_key"`
		UserID       string `json:"user_id"`
		IsDefault    *bool  `json:"is_default"`
		DefaultModel string `json:"default_model"`
	}
	if !readJSONBody(w, r, &body) { return }
	if body.UserID == "" || body.PtKey == "" { writeError(w, 400, "user_id and pt_key are required"); return }

	isDefault := false
	if body.IsDefault != nil { isDefault = *body.IsDefault }

	if err := h.store.AddAccount(body.UserID, body.PtKey, body.Nickname, isDefault, body.DefaultModel); err != nil {
		writeError(w, 500, err.Error()); return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user_id": body.UserID})
}

func (h *Handler) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	if len(parts) == 0 || parts[0] == "" { writeError(w, 400, "missing user_id"); return }
	userID := parts[0]
	action := ""
	if len(parts) > 1 { action = parts[1] }

	switch {
	case action == "" && r.Method == http.MethodDelete:
		h.store.RemoveAccount(userID); writeJSON(w, 200, map[string]any{"ok": true})
	case action == "default" && r.Method == http.MethodPut:
		if err := h.store.SetDefault(userID); err != nil { writeError(w, 500, err.Error()); return }
		writeJSON(w, 200, map[string]any{"ok": true})
	case action == "model" && r.Method == http.MethodPut:
		var b struct{ DefaultModel string `json:"default_model"` }
		if !readJSONBody(w, r, &b) { return }
		if err := h.store.UpdateAccountModel(userID, b.DefaultModel); err != nil { writeError(w, 500, err.Error()); return }
		writeJSON(w, 200, map[string]any{"ok": true})
	case action == "stats" && r.Method == http.MethodGet:
		stats, err := h.store.GetAccountStats(userID)
		if err != nil { writeError(w, 500, err.Error()); return }
		if stats.ByModel == nil { stats.ByModel = []store.ModelCount{} }
		if stats.ByEndpoint == nil { stats.ByEndpoint = []store.EndpointCount{} }
		if stats.Hourly == nil { stats.Hourly = []store.HourlyData{} }
		writeJSON(w, 200, stats)
	case action == "logs" && r.Method == http.MethodGet:
		limit := 200
		if l := r.URL.Query().Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
			if limit > 1000 { limit = 1000 }
		}
		logs, err := h.store.GetAccountLogs(userID, limit)
		if err != nil { writeError(w, 500, err.Error()); return }
		if logs == nil { logs = []store.RequestLog{} }
		writeJSON(w, 200, map[string]any{"logs": logs, "total": len(logs)})
	case action == "renew-token" && r.Method == http.MethodPost:
		token, err := h.store.RenewToken(userID)
		if err != nil { writeError(w, 500, err.Error()); return }
		writeJSON(w, 200, map[string]any{"ok": true, "api_token": token})
	case action == "remark" && r.Method == http.MethodPut:
		var b struct{ Remark string `json:"remark"` }
		if !readJSONBody(w, r, &b) { return }
		if err := h.store.UpdateRemark(userID, b.Remark); err != nil { writeError(w, 500, err.Error()); return }
		writeJSON(w, 200, map[string]any{"ok": true})
	case action == "validate" && r.Method == http.MethodPost:
		account, err := h.store.GetAccount(userID)
		if err != nil { writeError(w, 500, err.Error()); return }
		if account == nil { writeError(w, 404, "account not found"); return }
		valid := account.PtKey != ""
		if valid {
			h.store.SetCredentialValid(userID, true)
		}
		writeJSON(w, 200, map[string]any{"api_key": userID, "valid": valid})
	case action == "models" && r.Method == http.MethodGet:
		// Serve the models the daemon actually exposes. The previous hardcoded
		// list (deepseek-v4-flash, deepseek-chat, glm-5.2, ...) no longer exists
		// upstream and misled clients into requesting models that would silently
		// resolve to the daemon's default.
		if h.daemon == nil {
			writeError(w, 503, "daemon client not available")
			return
		}
		providers, err := h.daemon.ListProviders()
		if err != nil {
			writeError(w, 502, "无法读取模型列表: "+err.Error())
			return
		}
		models := make([]map[string]any, 0, len(providers))
		for _, p := range providers {
			models = append(models, map[string]any{
				"id":              p.Name,
				"name":            p.Name,
				"model":           p.Model,
				"is_default":      p.IsDefault,
				"context_window":  p.ContextWindow,
				"supports_vision": p.SupportsVision,
				"free":            "true",
			})
		}
		writeJSON(w, 200, map[string]any{"models": models})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (h *Handler) handleAutoLogin(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	creds, err := auth.LoadFromSystem()
	if err != nil {
		writeError(w, 400, "无法从本机获取 AtomCode 凭据: "+err.Error())
		return
	}
	if creds.UserID == "" || creds.Token == "" {
		writeError(w, 400, "AtomCode 凭证不完整（UserID 或 Token 为空）")
		return
	}

	// If we previously imported a stale "local" account, remove it now
	accounts, _ := h.store.ListAccounts()
	for _, a := range accounts {
		if a.UserID == "local" && creds.UserID != "local" {
			h.store.RemoveAccount("local")
			log.Printf("auto-login: removed stale local account")
		}
	}

	// Re-fetch account list after cleanup
	accounts, _ = h.store.ListAccounts()
	isDefault := true
	for _, a := range accounts {
		if a.IsDefault { isDefault = false; break }
	}

	// Check if this user already exists
	for _, a := range accounts {
		if a.UserID == creds.UserID {
			// Account already exists, just ensure the token is up-to-date
			h.store.UpdatePtKey(creds.UserID, creds.Token)
			writeJSON(w, 200, map[string]any{"ok": true, "user_id": creds.UserID, "is_default": a.IsDefault})
			return
		}
	}

	if err := h.store.AddAccount(creds.UserID, creds.Token, creds.UserID, isDefault, h.resolveDefaultModel()); err != nil {
		writeError(w, 500, "保存账号失败: "+err.Error()); return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user_id": creds.UserID, "is_default": isDefault})
}

func (h *Handler) handleClearAllAccounts(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	n, err := h.store.ClearAllAccounts()
	if err != nil { writeError(w, 500, err.Error()); return }
	writeJSON(w, 200, map[string]any{"ok": true, "count": n})
}

func (h *Handler) handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	items, err := h.store.ExportAccounts()
	if err != nil { writeError(w, 500, err.Error()); return }
	writeJSON(w, 200, map[string]any{"ok": true, "accounts": items, "count": len(items)})
}

func (h *Handler) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	var body struct{ Accounts []store.ExportAccountItem `json:"accounts"` }
	if !readJSONBody(w, r, &body) { return }
	if len(body.Accounts) == 0 { writeError(w, 400, "accounts array is empty"); return }

	added, updated, err := h.store.ImportAccounts(body.Accounts)
	if err != nil { writeError(w, 500, err.Error()); return }
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "updated": updated})
}

// handleBatchImport is a simplified account import endpoint designed for AI agents.
// Accepts a flat JSON array of accounts: [{"user_id":"...","pt_key":"...",...}]
// or the standard nested format: {"accounts": [...]}
// Returns the same response format as handleImportAccounts.
func (h *Handler) handleBatchImport(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	var accounts []store.ExportAccountItem

	// Try array format first: [{"user_id":"...", ...}]
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "cannot read body"); return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))

	if err := json.Unmarshal(raw, &accounts); err == nil {
		// Direct array format - use it directly
	} else {
		// Try nested format: {"accounts": [...]}
		var body struct{ Accounts []store.ExportAccountItem `json:"accounts"` }
		if json.Unmarshal(raw, &body) == nil {
			accounts = body.Accounts
		} else {
			writeError(w, 400, "expected JSON array of accounts or {accounts: [...]}")
			return
		}
	}

	if len(accounts) == 0 { writeError(w, 400, "accounts list is empty"); return }
	if len(accounts) > 100 {
		writeError(w, 400, "maximum 100 accounts per batch"); return
	}

	added, updated, err := h.store.ImportAccounts(accounts)
	if err != nil { writeError(w, 500, err.Error()); return }
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "updated": updated, "total": added + updated})
}

// ─── Stats, Settings, Errors, Models, Health ─────────────────────────────────

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	stats, err := h.store.GetStats()
	if err != nil { writeError(w, 500, err.Error()); return }
	if stats.ByModel == nil { stats.ByModel = []store.ModelCount{} }
	if stats.ByAccount == nil { stats.ByAccount = []store.AccountCount{} }

	totals, _ := h.store.GetAllTimeTotals()
	hourly, _ := h.store.GetHourlyStats()
	if hourly == nil { hourly = []store.HourlyData{} }

	writeJSON(w, 200, map[string]any{
		"total_requests": stats.TotalRequests, "total_input_tokens": stats.TotalInputTk,
		"total_output_tokens": stats.TotalOutputTk, "accounts_count": stats.AccountsCount,
		"avg_latency_ms": stats.AvgLatencyMs, "error_count": stats.ErrorCount,
		"stream_count": stats.StreamCount, "success_count": stats.SuccessCount,
		"by_model": stats.ByModel, "by_account": stats.ByAccount,
		"all_time": totals, "hourly": hourly,
		"quota": map[string]any{
			"daily_limit":        h.store.GetDailyQuota(),
			"account_daily_limit": h.store.GetAccountDailyQuota(),
		},
	})
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }

	switch r.Method {
	case http.MethodGet:
		settings, err := h.store.GetSettings()
		if err != nil { writeError(w, 500, err.Error()); return }
		if settings == nil { settings = map[string]string{} }
		// Never expose secrets over the API, even to an authenticated caller.
		// Previously GET returned auth_jwt_secret + auth_password_hash to any
		// unauthenticated client, which allowed forging a valid JWT.
		for k := range sensitiveSettings {
			delete(settings, k)
		}
		writeJSON(w, 200, map[string]any{"settings": settings})
	case http.MethodPut:
		if !h.isAuthenticated(r) {
			writeError(w, 401, "unauthorized")
			return
		}
		var raw map[string]json.RawMessage
		if !readJSONBody(w, r, &raw) { return }
		for k := range raw {
			if sensitiveSettings[k] {
				delete(raw, k)
			}
		}
		settings := make(map[string]string, len(raw))
		for k, v := range raw {
			trimmed := strings.TrimSpace(string(v))
			if len(trimmed) > 0 && trimmed[0] == '"' {
				var s string
				json.Unmarshal(v, &s)
				settings[k] = s
			} else {
				settings[k] = trimmed
			}
		}
		if err := h.store.SetSettings(settings); err != nil { writeError(w, 500, err.Error()); return }
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (h *Handler) handleErrors(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
		if limit > 200 { limit = 200 }
	}
	logs, err := h.store.GetRecentErrors(limit)
	if err != nil { writeError(w, 500, err.Error()); return }
	if logs == nil { logs = []store.RequestLog{} }
	writeJSON(w, 200, map[string]any{"errors": logs, "total": len(logs)})
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	// Report the daemon's real models. The previous hardcoded list named models
	// that no longer exist upstream (deepseek-v4-flash, Qwen-QwQ-32B).
	if h.daemon != nil {
		if providers, err := h.daemon.ListProviders(); err == nil {
			models := make([]map[string]string, 0, len(providers))
			for _, p := range providers {
				models = append(models, map[string]string{"id": p.Name, "name": p.Name})
			}
			writeJSON(w, 200, map[string]any{"models": models})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"models": []map[string]string{}})
}

// ModelCatalogItem describes a model available through the CodingPlan proxy.
type ModelCatalogItem struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Provider         string `json:"provider"`
	Type             string `json:"type"` // chat, reasoning, vision
	ContextWindow    int    `json:"context_window"`
	Free             bool   `json:"free"` // free with CodingPlan
	Default          bool   `json:"default"`
	EffortApplicable bool   `json:"effort_applicable"`
	InputPrice       string `json:"input_price"`
	OutputPrice      string `json:"output_price"`
	PricingNote      string `json:"pricing_note,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
}

func (h *Handler) handleModelsCatalog(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	// Static catalog of models available through CodingPlan.
	// Pricing info sourced from AtomCodeReverseEngineer project analysis.
	catalog := []ModelCatalogItem{
		{
			ID:               "deepseek-v4-flash",
			Name:             "DeepSeek V4 Flash",
			Provider:         "AtomGit",
			Type:             "chat",
			ContextWindow:    1000000,
			Free:             true,
			Default:          true,
			EffortApplicable: true,
			InputPrice:       "¥0",
			OutputPrice:      "¥0",
			PricingNote:      "CodingPlan 免费额度覆盖",
			MaxOutputTokens:  8192,
		},
		{
			ID:               "Qwen/Qwen3-VL-8B-Instruct",
			Name:             "Qwen3-VL 8B Instruct",
			Provider:         "AtomGit",
			Type:             "vision",
			ContextWindow:    64000,
			Free:             true,
			Default:          false,
			EffortApplicable: false,
			InputPrice:       "¥0",
			OutputPrice:      "¥0",
			PricingNote:      "CodingPlan 免费额度覆盖",
			MaxOutputTokens:  4096,
		},
		{
			ID:               "deepseek-chat",
			Name:             "DeepSeek Chat",
			Provider:         "DeepSeek",
			Type:             "chat",
			ContextWindow:    128000,
			Free:             false,
			Default:          false,
			EffortApplicable: false,
			InputPrice:       "¥0.5/百万tokens",
			OutputPrice:      "¥2/百万tokens",
			PricingNote:      "CodingPlan 需升级 Pro 以使用",
			MaxOutputTokens:  8192,
		},
		{
			ID:               "deepseek-reasoner",
			Name:             "DeepSeek Reasoner",
			Provider:         "DeepSeek",
			Type:             "reasoning",
			ContextWindow:    128000,
			Free:             false,
			Default:          false,
			EffortApplicable: false,
			InputPrice:       "¥1/百万tokens",
			OutputPrice:      "¥4/百万tokens",
			PricingNote:      "CodingPlan 需升级 Pro 以使用",
			MaxOutputTokens:  8192,
		},
		{
			ID:               "glm-5.2",
			Name:             "GLM 5.2",
			Provider:         "Zhipu AI",
			Type:             "chat",
			ContextWindow:    128000,
			Free:             false,
			Default:          false,
			EffortApplicable: false,
			InputPrice:       "¥0.5/百万tokens",
			OutputPrice:      "¥2/百万tokens",
			PricingNote:      "CodingPlan 需升级 Pro 以使用",
			MaxOutputTokens:  4096,
		},
	}

	writeJSON(w, 200, map[string]any{"catalog": catalog})
}

func (h *Handler) handleCodingPlanStatus(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	// Model catalogue split by CodingPlan tier. Free models are covered by
	// CodingPlan Lite; the rest need Pro.
	// Model lists come from the daemon; the old hardcoded names are gone
	// upstream.
	var freeModels, paidModels []string
	if providers, err := h.daemon.ListProviders(); err == nil {
		for _, p := range providers {
			freeModels = append(freeModels, p.Name)
		}
	}

	// v5.1.0: read structured entitlement + quota from
	// GET /codingplan/usage/summary. That endpoint is side-effect free, unlike
	// /codingplan/setup which re-claims the plan and rewrites provider config.
	if h.daemon == nil {
		writeError(w, 503, "daemon client not available")
		return
	}

	summary, err := h.daemon.CodingPlanUsageSummary()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"available":    false,
			"error":        err.Error(),
			"free_models":  freeModels,
			"paid_models":  paidModels,
			"pro_required": true,
			"note":         "无法读取 CodingPlan 状态，请确认 daemon 已登录且版本 >= 5.1.0。",
		})
		return
	}

	// Plan / entitlement block.
	plan := map[string]any{
		"name":           "CodingPlan Lite",
		"expires_at":     "",
		"remaining_days": 0,
		"total_days":     0,
		"status":         0,
		"claimed_at":     "",
	}
	if summary.Plan != nil {
		plan = map[string]any{
			"name":           summary.Plan.Name,
			"expires_at":     summary.Plan.ExpiresAt,
			"remaining_days": summary.Plan.RemainingDays,
			"total_days":     summary.Plan.TotalDays,
			"status":         summary.Plan.Status,
			"claimed_at":     summary.Plan.ClaimedAt,
		}
	}

	// Quota block: prefer the primary window, fall back to the first available.
	win := summary.PrimaryWindow
	if win == nil && len(summary.Windows) > 0 {
		win = &summary.Windows[0]
	}

	usage := map[string]any{
		"metric":                 "",
		"window_hours":           0,
		"limit":                  nil,
		"used":                   nil,
		"remaining":              nil,
		"current_window_percent": 0,
		"remaining_percent":      100,
		"quota_exhausted":        false,
		"resets_at":              "",
		"reset_label":            "每日重置",
		"seconds_until_reset":    0,
		"description":            "",
	}
	if win != nil {
		usage = map[string]any{
			"metric":                 win.Metric,
			"window_hours":           win.WindowHours,
			"limit":                  win.Limit,
			"used":                   win.Used,
			"remaining":              win.Remaining,
			"current_window_percent": int(win.UsagePercent),
			"remaining_percent":      int(win.RemainingPercent),
			"quota_exhausted":        win.QuotaExhausted,
			"resets_at":              strOr(win.NextResetDisplay, win.NextResetAt),
			"reset_label":            strOr(win.ResetLabel, "每日重置"),
			"seconds_until_reset":    win.SecondsUntilReset,
			"description":            win.UsageDescription,
		}
	}

	// All rolling windows, for clients that want the full picture.
	windows := make([]map[string]any, 0, len(summary.Windows))
	for _, w := range summary.Windows {
		windows = append(windows, map[string]any{
			"metric":              w.Metric,
			"window_hours":        w.WindowHours,
			"limit":               w.Limit,
			"used":                w.Used,
			"remaining":           w.Remaining,
			"usage_percent":       w.UsagePercent,
			"remaining_percent":   w.RemainingPercent,
			"quota_exhausted":     w.QuotaExhausted,
			"resets_at":           strOr(w.NextResetDisplay, w.NextResetAt),
			"reset_label":         w.ResetLabel,
			"seconds_until_reset": w.SecondsUntilReset,
		})
	}

	note := "免费模型由 CodingPlan Lite 额度覆盖。付费模型需升级至 Pro 套餐。"
	if summary.QuotaHint != "" {
		note = summary.QuotaHint
	}

	writeJSON(w, 200, map[string]any{
		"available":    summary.Available,
		"plan":         plan,
		"usage":        usage,
		"windows":      windows,
		"quota_hint":   summary.QuotaHint,
		"free_models":  freeModels,
		"paid_models":  paidModels,
		"pro_required": true,
		"note":         note,
	})
}

// handleCodingPlanDaily proxies the 60-day account-wide usage series.
func (h *Handler) handleCodingPlanDaily(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	if h.daemon == nil {
		writeError(w, 503, "daemon client not available")
		return
	}
	daily, err := h.daemon.CodingPlanUsageDaily()
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, daily)
}

// strOr returns def when s is empty.
func strOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }

	accounts, _ := h.store.ListAccounts()
	count := 0
	if accounts != nil {
		count = len(accounts)
	}
	version := h.Version
	if version == "" {
		version = "dev"
	}
	writeJSON(w, 200, map[string]any{
		"status":   "ok",
		"service":  "atomcode-2api",
		"version":  version,
		"accounts": count,
		"endpoints": []string{
			"POST /v1/chat/completions",
			"POST /v1/messages",
			"GET /v1/models",
			"GET /health",
			"GET /",
		},
	})
}

// ─── OAuth / QR Login ────────────────────────────────────────────────────────

func (h *Handler) handleBrowserLogin(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	if h.daemon == nil {
		writeError(w, 503, "daemon client not available")
		return
	}

	loginData, err := h.daemon.LoginStart()
	if err != nil {
		writeError(w, 502, "无法启动登录流程: "+err.Error())
		return
	}
	if loginData.URL == "" {
		writeError(w, 502, "daemon 未返回登录 URL")
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":                true,
		"url":               loginData.URL,
		"login_id":          loginData.LoginID,
		"expires_in_seconds": loginData.ExpiresInSeconds,
	})
}

func (h *Handler) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }

	if h.daemon == nil {
		writeError(w, 503, "daemon client not available")
		return
	}

	var body struct {
		LoginID string `json:"login_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON")
		return
	}
	if body.LoginID == "" {
		writeError(w, 400, "login_id is required")
		return
	}

	result, err := h.daemon.LoginPoll(body.LoginID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": "error", "message": err.Error()})
		return
	}

	if result.Status == "authorized" && result.User != nil {
		// Login successful — daemon has saved the token internally.
		// Import the account into our store.
		nickname := result.User.Name
		if nickname == "" {
			nickname = result.User.ID
		}
		isDefault := true
		accounts, _ := h.store.ListAccounts()
		for _, a := range accounts {
			if a.IsDefault {
				isDefault = false
				break
			}
		}
		// The daemon stores the token in auth.toml after successful login.
		// Use auto-import to load it.
		creds, err := auth.LoadFromSystem()
		if err == nil && creds.UserID != "" && creds.Token != "" {
			h.store.AddAccount(creds.UserID, creds.Token, nickname, isDefault, h.resolveDefaultModel())
			h.store.SetCredentialValid(creds.UserID, true)
			writeJSON(w, 200, map[string]any{"status": "confirmed", "ok": true, "user_id": creds.UserID, "nickname": nickname})
			return
		}
		writeJSON(w, 200, map[string]any{"status": "confirmed", "ok": true, "user_id": result.User.ID, "nickname": nickname})
		return
	}

	if result.Error != "" {
		writeJSON(w, 200, map[string]any{"status": "error", "message": result.Error})
		return
	}

	writeJSON(w, 200, map[string]any{"status": "pending"})
}

func (h *Handler) handleOAuthSubmit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }
	var body struct{ PtKey string `json:"pt_key"` }
	if !readJSONBody(w, r, &body) { return }
	if body.PtKey == "" { writeError(w, 400, "pt_key is required"); return }
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Handler) handleQRLoginInit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodPost { writeError(w, 405, "method not allowed"); return }
	sessionID, qrImage, err := auth.QRInit()
	if err != nil { writeError(w, 500, "生成二维码失败: "+err.Error()); return }
	writeJSON(w, 200, map[string]any{"ok": true, "session_id": sessionID, "qr_image": "data:image/png;base64," + qrImage})
}

func (h *Handler) handleQRLoginStatus(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	sessionID := r.URL.Query().Get("session")
	if sessionID == "" { writeError(w, 400, "missing session parameter"); return }

	status, result, err := auth.QRPollStatus(sessionID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	if status != "confirmed" {
		writeJSON(w, 200, map[string]any{"status": status})
		return
	}

	nickname := result.RealName
	if nickname == "" { nickname = result.UserID }
	isDefault := true
	accounts, _ := h.store.ListAccounts()
	for _, a := range accounts { if a.IsDefault { isDefault = false; break } }

	if err := h.store.AddAccount(result.UserID, result.PtKey, nickname, isDefault, h.resolveDefaultModel()); err != nil {
		writeJSON(w, 200, map[string]any{"status": "confirmed", "ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "confirmed", "ok": true, "user_id": result.UserID, "nickname": nickname})
}

// ─── GitHub Stars ────────────────────────────────────────────────────────────

var ghStarsCache int
var ghStarsCacheTime time.Time

func (h *Handler) handleGitHubStars(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
	if r.Method != http.MethodGet { writeError(w, 405, "method not allowed"); return }

	stars := ghStarsCache
	writeJSON(w, 200, map[string]any{"stars": stars})
}

// knownAPISet lists OpenAI/Anthropic-style endpoint paths that users
// commonly hit without the /v1/ prefix. When these paths arrive at the
// SPA catch-all we return a JSON 404 with a helpful hint instead of HTML.
var knownAPISet = map[string]bool{
	"/chat/completions":     true,
	"/completions":          true,
	"/messages":             true,
	"/models":               true,
	"/embeddings":           true,
	"/web-search":           true,
	"/rerank":               true,
	"/images/generations":   true,
	"/audio/transcriptions": true,
	"/audio/translations":   true,
}

// ─── Static Files / SPA ───────────────────────────────────────────────────

func (h *Handler) ServeStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Intercept known API paths that are missing the /v1/ prefix.
	// Return a structured JSON 404 so SDKs get a clear error instead of HTML.
	if knownAPISet[path] {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("%s %s not found. AtomCode2API serves the API under /v1/. Set base_url to http://<host>:<port>/v1", r.Method, path),
			},
		})
		return
	}

	if path == "/" {
		path = "/index.html"
	}

	if h.staticFS != nil {
		// Try exact file first
		f, err := h.staticFS.Open(strings.TrimPrefix(path, "/"))
		if err == nil {
			defer f.Close()
			stat, _ := f.Stat()
			if stat != nil && !stat.IsDir() {
				ct := "text/html"
				if strings.HasSuffix(path, ".js") { ct = "application/javascript" }
				if strings.HasSuffix(path, ".css") { ct = "text/css" }
				if strings.HasSuffix(path, ".json") { ct = "application/json" }
				if strings.HasSuffix(path, ".ico") { ct = "image/x-icon" }
				if strings.HasSuffix(path, ".svg") { ct = "image/svg+xml" }
				w.Header().Set("Content-Type", ct)
				http.ServeContent(w, r, path, stat.ModTime(), readFileSeeker{f})
				return
			}
		}

		// SPA fallback: serve index.html for all non-file, non-API routes
		if f, err := h.staticFS.Open("index.html"); err == nil {
			defer f.Close()
			stat, _ := f.Stat()
			w.Header().Set("Content-Type", "text/html")
			http.ServeContent(w, r, "index.html", stat.ModTime(), readFileSeeker{f})
			return
		}
	}

	http.NotFound(w, r)
}

type readFileSeeker struct {
	fs.File
}

func (r readFileSeeker) Seek(offset int64, whence int) (int64, error) {
	if seeker, ok := r.File.(io.Seeker); ok {
		return seeker.Seek(offset, whence)
	}
	return 0, fmt.Errorf("file not seekable")
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func setCors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	setCors(w)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"detail": msg})
}

func readJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (h *Handler) isAuthenticated(r *http.Request) bool {
	if h.store == nil {
		return false
	}
	token := bearerToken(r)
	if token == "" {
		if c, err := r.Cookie("token"); err == nil {
			token = c.Value
		}
	}
	if token == "" {
		return false
	}
	// Verify the JWT signature and expiry against the stored secret.
	// Previously this only checked that both strings were non-empty, so any
	// fabricated token was accepted.
	secret := h.store.GetSetting("auth_jwt_secret")
	if secret == "" {
		return false
	}
	_, err := auth.ValidateAnyToken(token, secret)
	return err == nil
}

// bearerToken extracts a token from the Authorization: Bearer header.
func bearerToken(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

// sensitiveSettings lists setting keys that must never cross the API boundary.
var sensitiveSettings = map[string]bool{
	"auth_jwt_secret":    true,
	"auth_password_hash": true,
}