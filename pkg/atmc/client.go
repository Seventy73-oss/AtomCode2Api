package atmc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibe-coding-labs/AtomCode2API/pkg/auth"
)

// Client is the HTTP client for the AtomCode daemon REST API.
type Client struct {
	BaseURL    string
	httpClient *http.Client

	mu    sync.RWMutex
	token string // daemon auth token (v5.1.0+); empty means no auth
}

// NewClient creates a new daemon client.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 120 * time.Second},
		token:      resolveDaemonToken(baseURL),
	}
}

// SetTimeout overrides the default HTTP client timeout.
func (c *Client) SetTimeout(d time.Duration) {
	c.httpClient.Timeout = d
}

// SetToken sets the daemon auth token explicitly.
func (c *Client) SetToken(t string) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

// Token returns the currently configured daemon auth token.
func (c *Client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// RefreshTokenFromDisk re-reads the daemon token file. Daemon mints a new token
// on every start, so a long-running proxy must pick up the new value.
func (c *Client) RefreshTokenFromDisk() {
	if t := resolveDaemonToken(c.BaseURL); t != "" {
		c.SetToken(t)
	}
}

// ─── Token discovery ──────────────────────────────────────────────────────────

// daemonPort extracts the port from the daemon base URL (default 13456).
func daemonPort(baseURL string) int {
	u, err := url.Parse(baseURL)
	if err != nil || u.Port() == "" {
		return 13456
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		return 13456
	}
	return p
}

// atomcodeHome resolves the AtomCode config directory, honouring ATOMCODE_HOME
// like the daemon does. Kept as a thin alias over auth.AtomcodeHome so every
// component resolves the same directory.
func atomcodeHome() (string, error) {
	return auth.AtomcodeHome()
}

// resolveDaemonToken looks up the daemon auth token introduced in AtomCode v5.1.0.
//
// Resolution order (mirrors crates/atomcode-daemon/src/main.rs):
//  1. ATOMCODE_DAEMON_TOKEN env var (non-empty wins outright)
//  2. ~/.atomcode/daemon-<port>.json written by the daemon (0600)
//
// Returns "" when neither exists, which means the daemon runs with
// `--no-auth` (or is an older release) and needs no Authorization header.
func resolveDaemonToken(baseURL string) string {
	if t := strings.TrimSpace(os.Getenv("ATOMCODE_DAEMON_TOKEN")); t != "" {
		return t
	}

	home, err := atomcodeHome()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, fmt.Sprintf("daemon-%d.json", daemonPort(baseURL)))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return strings.TrimSpace(f.Token)
}

// ─── Request plumbing ─────────────────────────────────────────────────────────

// applyHeaders sets the common headers, including daemon auth when configured.
func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if t := c.Token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}

// newRequest builds a request against the daemon with auth applied.
func (c *Client) newRequest(method, path string, body []byte) (*http.Request, error) {
	u, err := url.JoinPath(c.BaseURL, path)
	if err != nil {
		return nil, fmt.Errorf("join path: %w", err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)
	return req, nil
}

func (c *Client) syncRequest(method, path string, body any) (int, []byte) {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}

	req, err := c.newRequest(method, path, reqBody)
	if err != nil {
		return 500, []byte(fmt.Sprintf(`{"error":"build request: %s"}`, err))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 503, []byte(fmt.Sprintf(`{"error":"daemon unreachable: %s"}`, err))
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	// A 401 means the daemon started with a fresh token; retry once after
	// re-reading the token file so a restart doesn't wedge the proxy.
	if resp.StatusCode == http.StatusUnauthorized {
		c.RefreshTokenFromDisk()
		if t := c.Token(); t != "" {
			if req2, err := c.newRequest(method, path, reqBody); err == nil {
				if resp2, err := c.httpClient.Do(req2); err == nil {
					defer resp2.Body.Close()
					data2, _ := io.ReadAll(resp2.Body)
					return resp2.StatusCode, data2
				}
			}
		}
	}

	return resp.StatusCode, data
}

func (c *Client) syncGet(path string) (int, []byte) {
	return c.syncRequest("GET", path, nil)
}

func (c *Client) syncPost(path string, body any) (int, []byte) {
	return c.syncRequest("POST", path, body)
}

func (c *Client) syncDelete(path string) (int, []byte) {
	return c.syncRequest("DELETE", path, nil)
}

// ─── Auth ──────────────────────────────────────────────────────────────────────

func (c *Client) Health() (*HealthResponse, error) {
	code, data := c.syncGet("/health")
	if code != 200 {
		return nil, fmt.Errorf("health check failed (HTTP %d): %s", code, string(data))
	}
	var h HealthResponse
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parse health response: %w", err)
	}
	return &h, nil
}

func (c *Client) AuthStatus() (*AuthStatusResponse, error) {
	code, data := c.syncGet("/auth/status")
	if code != 200 {
		return nil, fmt.Errorf("auth status failed (HTTP %d): %s", code, string(data))
	}
	var a AuthStatusResponse
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse auth status: %w", err)
	}
	return &a, nil
}

func (c *Client) LoginStart() (*LoginStartResponse, error) {
	_, data := c.syncPost("/auth/login/start", LoginStartRequest{OpenBrowser: false})
	var l LoginStartResponse
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parse login start: %w", err)
	}
	return &l, nil
}

func (c *Client) LoginPoll(loginID string) (*LoginPollResponse, error) {
	_, data := c.syncPost(fmt.Sprintf("/auth/login/%s/poll", loginID), map[string]any{})
	var p LoginPollResponse
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse login poll: %w", err)
	}
	return &p, nil
}

// Logout ends the daemon's AtomGit session (daemon v5.1.0: POST /auth/logout).
func (c *Client) Logout() error {
	code, data := c.syncPost("/auth/logout", map[string]any{})
	if code != 200 {
		return fmt.Errorf("logout failed (HTTP %d): %s", code, string(data))
	}
	return nil
}

// ─── CodingPlan ───────────────────────────────────────────────────────────────

func (c *Client) CodingPlanSetup() (*CodingPlanSetupResponse, error) {
	_, data := c.syncPost("/codingplan/setup", map[string]any{})
	var s CodingPlanSetupResponse
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse codingplan setup: %w", err)
	}
	return &s, nil
}

// CodingPlanUsageSummary retrieves structured entitlement + quota data.
//
// Replaces the removed `/codingplan/status` endpoint. Unlike `/codingplan/setup`
// this call has no side effects — it never re-claims the plan or rewrites the
// provider config.
func (c *Client) CodingPlanUsageSummary() (*CodingPlanUsageSummaryResponse, error) {
	code, data := c.syncGet("/codingplan/usage/summary")
	if code != 200 {
		return nil, fmt.Errorf("codingplan usage summary failed (HTTP %d): %s", code, string(data))
	}
	var s CodingPlanUsageSummaryResponse
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse codingplan usage summary: %w", err)
	}
	return &s, nil
}

// CodingPlanUsageDaily retrieves the 60-day account-wide usage series.
func (c *Client) CodingPlanUsageDaily() (*CodingPlanDailyUsageResponse, error) {
	code, data := c.syncGet("/codingplan/usage/daily")
	if code != 200 {
		return nil, fmt.Errorf("codingplan usage daily failed (HTTP %d): %s", code, string(data))
	}
	var s CodingPlanDailyUsageResponse
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse codingplan usage daily: %w", err)
	}
	return &s, nil
}

// RefreshToken attempts to refresh the daemon's auth token.
// Returns true if the token was refreshed or is still valid.
func (c *Client) RefreshToken() (bool, error) {
	auth, err := c.AuthStatus()
	if err != nil {
		return false, fmt.Errorf("auth status check failed: %w", err)
	}
	if auth.LoggedIn && auth.Token != nil && auth.Token.ExpiresIn > 60 {
		return true, nil
	}
	return false, fmt.Errorf("token expired or not logged in")
}

// ─── Models / Providers ──────────────────────────────────────────────────────

func (c *Client) ListModels() ([]ModelInfo, error) {
	_, data := c.syncGet("/models")
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	models := make([]ModelInfo, 0, len(raw))
	for _, r := range raw {
		var m ModelInfo
		if err := json.Unmarshal(r, &m); err != nil {
			continue
		}
		// v5.1.0 /models returns `provider` as the selectable model id and
		// `model` as the concrete upstream model name.
		if m.ID == "" {
			if m.Provider != "" {
				m.ID = m.Provider
			} else {
				m.ID = m.Model
			}
		}
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.ID == "" {
			continue
		}
		models = append(models, m)
	}
	return models, nil
}

func (c *Client) ListProviders() ([]ProviderConfig, error) {
	code, data := c.syncGet("/providers")
	if code != 200 {
		return nil, fmt.Errorf("list providers failed (HTTP %d): %s", code, string(data))
	}

	// Canonical v5.1.0 envelope: {"default_provider": "...", "providers": [...]}
	var env ProvidersResponse
	if err := json.Unmarshal(data, &env); err == nil && env.Providers != nil {
		return env.Providers, nil
	}

	// Tolerate a bare array (older daemons / alternate builds).
	var providers []ProviderConfig
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("parse providers: %w", err)
	}
	return providers, nil
}

// ─── Chat (SSE stream) ───────────────────────────────────────────────────────

// ChatStream sends a chat request and returns a channel of SSE events.
// The caller must read from the channel until it closes.
func (c *Client) ChatStream(req *ChatRequest) (<-chan SSEEvent, error) {
	body, _ := json.Marshal(req)

	httpReq, err := c.newRequest("POST", "/chat", body)
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat request failed: %w", err)
	}

	// Retry once with a freshly-read token if the daemon restarted.
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		c.RefreshTokenFromDisk()
		if httpReq2, err := c.newRequest("POST", "/chat", body); err == nil {
			httpReq2.Header.Set("Accept", "text/event-stream")
			if resp2, err := c.httpClient.Do(httpReq2); err == nil {
				resp = resp2
			}
		}
	}

	if resp.StatusCode != 200 {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("chat returned HTTP 401: daemon requires auth. " +
				"Set ATOMCODE_DAEMON_TOKEN or start the daemon with --no-auth")
		}
		return nil, fmt.Errorf("chat returned HTTP %d", resp.StatusCode)
	}

	ch := make(chan SSEEvent, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		// Daemon turns can emit large tool outputs; allow up to 4 MiB per line.
		scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			dataStr := line[6:]
			if dataStr == "[DONE]" {
				ch <- SSEEvent{Type: "done"}
				return
			}
			var ev SSEEvent
			if err := json.Unmarshal([]byte(dataStr), &ev); err != nil {
				log.Printf("atmc: failed to parse SSE event: %v (line: %s)", err, line)
				continue
			}
			ch <- ev
		}
		if err := scanner.Err(); err != nil {
			log.Printf("atmc: SSE scanner error: %v", err)
		}
	}()

	return ch, nil
}

// StopChat cancels a running turn. `sessionID` accepts either a real session id
// or the first-turn `request_id` the daemon registered as an alias.
func (c *Client) StopChat(sessionID string) (*ChatStopResponse, error) {
	_, data := c.syncPost("/chat/stop", ChatStopRequest{SessionID: sessionID})
	var r ChatStopResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse chat stop: %w", err)
	}
	return &r, nil
}

// ChatPermission delivers a tool-approval decision for a blocked turn.
func (c *Client) ChatPermission(sessionID, decision, toolName string) (*PermissionDecisionResponse, error) {
	_, data := c.syncPost("/chat/permission", PermissionDecisionRequest{
		SessionID: sessionID,
		Decision:  decision,
		ToolName:  toolName,
	})
	var r PermissionDecisionResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse permission decision: %w", err)
	}
	return &r, nil
}
