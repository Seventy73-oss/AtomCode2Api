package auth

import (
	"log"
	"net/http"
	"strings"
)

// publicPrefixes are request paths that must never require a JWT.
//
// The SPA shell and its hashed bundles are served from the root and /assets/,
// so they have to load before the client can even present a token. The
// bootstrap auth endpoints (/api/auth/status|setup|login) are how a client
// obtains a token in the first place. The OpenAI/Anthropic proxy surface under
// /v1/ is guarded by its own API-key model and must stay reachable for SDKs.
var publicPrefixes = []string{
	"/assets/",    // Vite build output
	"/static/",    // legacy asset path, kept for compatibility
	"/favicon.",   // favicon.ico / favicon.svg
	"/health",     // daemon-facing health probe
	"/v1/",        // OpenAI / Anthropic compatible proxy
	"/api/auth/",  // status / setup / login / change-password (self-guarding)
}

// publicExact are exact paths served without authentication.
var publicExact = map[string]bool{
	"/":                    true, // SPA shell (index.html)
	"/index.html":          true,
	"/login":               true,
	"/setup":               true,
	"/forgot-password":     true,
	"/oauth-error":         true,
	"/dashboard":           true,
	"/accounts":            true,
	"/settings":            true,
	"/api/health":          true, // liveness probe used by Docker healthchecks
	"/api/github-stars":    true,
}

// isPublicPath reports whether the request may bypass JWT validation.
func isPublicPath(path string) bool {
	if publicExact[path] {
		return true
	}
	for _, p := range publicPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	// SPA client-side routes such as /accounts/<user_id> must render the shell;
	// the page itself only shows data fetched from the (protected) /api/* layer.
	if strings.HasPrefix(path, "/accounts/") {
		return true
	}
	return false
}

// SecretProvider returns the current JWT signing secret, or "" when the
// dashboard has not been initialised yet. It is consulted per request so that
// setting the root password after startup immediately arms authentication.
type SecretProvider func() string

// JWTMiddleware wraps a handler with JWT validation for dashboard routes.
//
// Every /api/* endpoint requires a valid JWT (via `Authorization: Bearer` or
// the `token` cookie). Static assets, the SPA shell, the auth bootstrap
// endpoints and the /v1/ proxy surface pass through.
func JWTMiddleware(jwtManager *JWTManager, next http.Handler) http.Handler {
	static := jwtManager
	return JWTMiddlewareDynamic(func() string {
		if static == nil {
			return ""
		}
		return string(static.secret)
	}, next)
}

// JWTMiddlewareDynamic is JWTMiddleware with a per-request secret lookup.
//
// The secret lives in the SQLite store and is created by /api/auth/setup, which
// normally runs *after* the server has started. Resolving it lazily means a
// freshly configured instance is protected without a restart.
func JWTMiddlewareDynamic(secretFn SecretProvider, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if isPublicPath(path) {
			next.ServeHTTP(w, r)
			return
		}

		// Protect the remaining /api/* surface.
		if strings.HasPrefix(path, "/api/") {
			secret := ""
			if secretFn != nil {
				secret = secretFn()
			}
			if secret == "" {
				// Not initialised: refuse rather than silently serving an open
				// dashboard. The client is told to run the setup flow.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"detail":"dashboard not initialized: complete /setup first"}`))
				return
			}

			token := ""
			if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
				token = strings.TrimSpace(a[7:])
			}
			if token == "" {
				if c, err := r.Cookie("token"); err == nil {
					token = c.Value
				}
			}
			if token == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			claims, err := ValidateAnyToken(token, secret)
			if err != nil {
				log.Printf("auth: invalid JWT token: %v", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r.Header.Set("X-User-ID", claims.UserID)
		}

		next.ServeHTTP(w, r)
	})
}
