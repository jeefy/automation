package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	ghlib "github.com/cncf/automation/utilities/pr-cooldown/internal/github"
	gh "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

type contextKey string

const tokenContextKey contextKey = "github_token"

// TokenFromContext retrieves the GitHub token from the request context.
func TokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(tokenContextKey).(string)
	return token
}

// tokenCacheEntry holds a cached token validation result.
type tokenCacheEntry struct {
	login     string
	expiresAt time.Time
}

// GitHubClientFactory creates a GitHub client from a token.
// This allows testing with mock clients.
type GitHubClientFactory func(ctx context.Context, token string) ghlib.Client

// DefaultGitHubClientFactory creates a real GitHub client using oauth2.
func DefaultGitHubClientFactory(ctx context.Context, token string) ghlib.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, ts)
	return ghlib.NewClient(gh.NewClient(httpClient))
}

// TokenValidator validates GitHub tokens and caches results.
type TokenValidator struct {
	cacheTTL      time.Duration
	mu            sync.RWMutex
	cache         map[string]tokenCacheEntry
	clientFactory GitHubClientFactory
}

// NewTokenValidator creates a new token validator with the given cache TTL.
func NewTokenValidator(cacheTTL time.Duration) *TokenValidator {
	return &TokenValidator{
		cacheTTL:      cacheTTL,
		cache:         make(map[string]tokenCacheEntry),
		clientFactory: DefaultGitHubClientFactory,
	}
}

// NewTokenValidatorWithFactory creates a new token validator with a custom client factory.
func NewTokenValidatorWithFactory(cacheTTL time.Duration, factory GitHubClientFactory) *TokenValidator {
	return &TokenValidator{
		cacheTTL:      cacheTTL,
		cache:         make(map[string]tokenCacheEntry),
		clientFactory: factory,
	}
}

// Middleware returns HTTP middleware that validates the GitHub token
// from the Authorization header.
func (v *TokenValidator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r)
		if token == "" {
			http.Error(w, `{"error":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
			return
		}

		// Check cache first
		v.mu.RLock()
		entry, ok := v.cache[token]
		v.mu.RUnlock()

		if ok && time.Now().Before(entry.expiresAt) {
			// Token is cached and valid
			ctx := context.WithValue(r.Context(), tokenContextKey, token)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Validate against GitHub
		ghClient := v.clientFactory(r.Context(), token)

		login, err := ghClient.ValidateToken(r.Context())
		if err != nil {
			http.Error(w, `{"error":"invalid GitHub token"}`, http.StatusUnauthorized)
			return
		}

		// Cache the result
		v.mu.Lock()
		v.cache[token] = tokenCacheEntry{
			login:     login,
			expiresAt: time.Now().Add(v.cacheTTL),
		}
		v.mu.Unlock()

		ctx := context.WithValue(r.Context(), tokenContextKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
