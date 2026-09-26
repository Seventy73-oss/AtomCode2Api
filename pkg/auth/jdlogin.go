package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// QRSession holds the state for a QR login flow.
type QRSession struct {
	SessionID string
	PtKey     string
	UserID    string
	RealName  string
	ExpiresAt time.Time
}

type QRVerifyNeededError struct {
	VerifyURL string
	RiskCode  string
}

func (e *QRVerifyNeededError) Error() string {
	return fmt.Sprintf("verification required: %s (risk=%s)", e.VerifyURL, e.RiskCode)
}

var qrSessions = make(map[string]*QRSession)

const (
	jdhBaseURL = "https://jdh-sdk.jd.com"
	appID      = "jdh_joycode"
)

// QRInit creates a QR login session and returns the session ID and QR image base64.
func QRInit() (string, string, error) {
	// JD QR API has been deprecated by JD. Return a clear error message
	// directing users to use the daemon-based auto-login instead.
	return "", "", fmt.Errorf("京东扫码登录接口已失效，请使用「一键登录」从本机 AtomCode 自动导入账号。如需手动登录，请运行: atomcode setup")
}

// QRPollStatus polls the QR login status.
func QRPollStatus(sessionID string) (string, *QRSession, error) {
	sess, ok := qrSessions[sessionID]
	if !ok {
		return "", nil, fmt.Errorf("session not found")
	}
	if time.Now().After(sess.ExpiresAt) {
		return "expired", nil, nil
	}

	resp, err := http.Post(jdhBaseURL+"/api/qr/QRCodeCheckStatus",
		"application/json",
		strings.NewReader(fmt.Sprintf(`{"sessionId":"%s","appId":"%s"}`, sessionID, appID)),
	)
	if err != nil {
		return "", nil, fmt.Errorf("qr check: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status     string `json:"status"`
			VerifyURL  string `json:"verifyUrl"`
			RiskCode   string `json:"riskCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", nil, fmt.Errorf("parse: %w", err)
	}

	switch result.Data.Status {
	case "SCANNED":
		return "scanned", nil, nil
	case "CONFIRMED":
		return "confirmed", sess, nil
	case "VERIFICATION_REQUIRED":
		return "", nil, &QRVerifyNeededError{
			VerifyURL: result.Data.VerifyURL,
			RiskCode:  result.Data.RiskCode,
		}
	default:
		return result.Data.Status, nil, nil
	}
}

// AtomcodeHome resolves the AtomCode config directory.
//
// The daemon honours the ATOMCODE_HOME environment variable and falls back to
// ~/.atomcode. This helper mirrors that resolution so every component agrees on
// where auth.toml, config.toml and daemon-<port>.json live. Previously this file
// hard-coded ~/.atomcode while pkg/atmc honoured ATOMCODE_HOME, so a daemon
// started with a custom home produced a "logged in" daemon but an empty account
// list in the proxy.
func AtomcodeHome() (string, error) {
	if h := strings.TrimSpace(os.Getenv("ATOMCODE_HOME")); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".atomcode"), nil
}

// LoadFromSystem loads AtomCode daemon credentials from the local system.
// Reads auth.toml (OAuth access token + user info) written by AtomCode's login
// flow, falling back to config.toml for the legacy api_key format.
func LoadFromSystem() (*Credentials, error) {
	dir, err := AtomcodeHome()
	if err != nil {
		return nil, err
	}

	// Primary: auth.toml (TOML format with user info)
	authFile := filepath.Join(dir, "auth.toml")
	if data, err := os.ReadFile(authFile); err == nil {
		if creds := parseAuthToml(string(data)); creds != nil {
			return creds, nil
		}
	}

	// Fallback: config.toml (legacy format with api_key)
	configFile := filepath.Join(dir, "config.toml")
	paths := []string{configFile}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "api_key") || strings.HasPrefix(line, "token") {
				// Skip token_type lines
				if strings.HasPrefix(line, "token_type") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					val := strings.TrimSpace(parts[1])
					val = strings.Trim(val, `"`)
					if val != "" {
						return &Credentials{Token: val, UserID: "local"}, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("no credentials found in %s", strings.Join(append([]string{authFile}, paths...), ", "))
}

// parseAuthToml parses the AtomCode auth.toml file to extract access_token and user ID.
// TOML format:
//
//	access_token = "..."
//	[user]
//	id = "..."
func parseAuthToml(content string) *Credentials {
	var accessToken, userID string

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		// Skip section headers and empty lines
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"`)

		switch key {
		case "access_token":
			accessToken = val
		case "id":
			userID = val
		}
	}

	if accessToken == "" {
		return nil
	}
	if userID == "" {
		userID = "local"
	}
	return &Credentials{Token: accessToken, UserID: userID}
}

// GenerateToken creates a JWT-like signed token using the secret.
func GenerateToken(userID, secret string, expiry time.Duration) (string, error) {
	payload := fmt.Sprintf(`{"sub":"%s","exp":%d}`, userID, time.Now().Add(expiry).Unix())
	mac := hmacSHA256(payload, secret)
	token := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac)
	return token, nil
}

func hmacSHA256(data, key string) []byte {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(data))
	return m.Sum(nil)
}

func init() {
	_ = url.QueryEscape
}