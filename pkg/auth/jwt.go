package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	storeDir = ".atomcode-2api"
	keyFile  = ".auth_key"
)

var (
	defaultStoreDir string
)

func init() {
	home, _ := os.UserHomeDir()
	defaultStoreDir = filepath.Join(home, storeDir)
}

// ─── AES-GCM Encryption ─────────────────────────────────────────────────────

type Encryptor struct {
	aead cipher.AEAD
}

func NewEncryptor() (*Encryptor, error) {
	if err := os.MkdirAll(defaultStoreDir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(defaultStoreDir, keyFile)
	data, err := os.ReadFile(keyPath)
	if err != nil {
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0600); err != nil {
			return nil, err
		}
		data = []byte(hex.EncodeToString(key))
	}
	key, err := hex.DecodeString(string(data))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Encryptor{aead: aead}, nil
}

func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := e.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

func (e *Encryptor) Decrypt(ciphertext string) (string, error) {
	data, err := hex.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	nonceSize := e.aead.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := data[:nonceSize], data[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// ─── JWT ─────────────────────────────────────────────────────────────────────

type JWTManager struct {
	secret   []byte
	issuer   string
	duration time.Duration
}

func NewJWTManager(secret string) *JWTManager {
	if secret == "" {
		b := make([]byte, 32)
		io.ReadFull(rand.Reader, b)
		secret = hex.EncodeToString(b)
	}
	return &JWTManager{
		secret:   []byte(secret),
		issuer:   "atomcode-2api",
		duration: 24 * time.Hour,
	}
}

type Claims struct {
	jwt.RegisteredClaims
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

func (m *JWTManager) GenerateToken(userID, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.duration)),
		},
		UserID: userID,
		Role:   role,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

func (m *JWTManager) ValidateToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		return m.secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}

// ValidateLegacyToken verifies the pre-existing two-segment HMAC token produced
// by GenerateToken (pkg/auth/jdlogin.go):
//
//	base64url(payload) "." base64url(HMAC-SHA256(payload, secret))
//
// It is retained so tokens issued before the JWT unification keep working.
func ValidateLegacyToken(tokenStr, secret string) (*Claims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("not a legacy token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	expected := hmacSHA256(string(payload), secret)
	if !hmac.Equal(sig, expected) {
		return nil, fmt.Errorf("signature mismatch")
	}
	var c struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	if c.Exp > 0 && time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("token expired")
	}
	return &Claims{UserID: c.Sub}, nil
}

// ValidateAnyToken accepts either a standard JWT or a legacy HMAC token.
// This is the entry point used by the dashboard middleware so that both
// formats validate against the same stored secret.
func ValidateAnyToken(tokenStr, secret string) (*Claims, error) {
	if tokenStr == "" || secret == "" {
		return nil, fmt.Errorf("missing token or secret")
	}
	if c, err := NewJWTManager(secret).ValidateToken(tokenStr); err == nil {
		return c, nil
	}
	return ValidateLegacyToken(tokenStr, secret)
}