package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghlib "github.com/cncf/automation/utilities/pr-cooldown/internal/github"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
)

// --- Mock Store ---

type mockStore struct {
	users      map[string]*models.UserCache
	activities map[string][]models.PRActivity
	cooldowns  map[string]*models.Cooldown
	failOn     string // method name to fail on
}

func newMockStore() *mockStore {
	return &mockStore{
		users:      make(map[string]*models.UserCache),
		activities: make(map[string][]models.PRActivity),
		cooldowns:  make(map[string]*models.Cooldown),
	}
}

func (m *mockStore) GetUserCache(login string) (*models.UserCache, error) {
	if m.failOn == "GetUserCache" {
		return nil, fmt.Errorf("store error")
	}
	return m.users[login], nil
}

func (m *mockStore) SetUserCache(cache *models.UserCache) error {
	m.users[cache.GitHubLogin] = cache
	return nil
}

func (m *mockStore) GetPRActivity(login string, since time.Time) ([]models.PRActivity, error) {
	var result []models.PRActivity
	for _, a := range m.activities[login] {
		if a.ClosedAt != nil && !a.ClosedAt.Before(since) {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockStore) SetPRActivity(activities []models.PRActivity) error {
	if len(activities) == 0 {
		return nil
	}
	login := activities[0].GitHubLogin
	m.activities[login] = activities
	return nil
}

func (m *mockStore) GetCooldown(login string) (*models.Cooldown, error) {
	if m.failOn == "GetCooldown" {
		return nil, fmt.Errorf("store error")
	}
	return m.cooldowns[login], nil
}

func (m *mockStore) SetCooldown(cooldown *models.Cooldown) error {
	m.cooldowns[cooldown.GitHubLogin] = cooldown
	return nil
}

func (m *mockStore) Close() error { return nil }

// --- Mock GitHub Client for middleware ---

type mockGHClient struct {
	login string
	err   error
}

func (m *mockGHClient) GetUser(ctx context.Context, login string) (*models.UserCache, error) {
	return nil, nil
}

func (m *mockGHClient) GetClosedPRs(ctx context.Context, login string, since time.Time) ([]models.PRActivity, error) {
	return nil, nil
}

func (m *mockGHClient) CheckKeywordsInComments(ctx context.Context, repoFullName string, prNumber int, prAuthor string, keywords []string) (bool, error) {
	return false, nil
}

func (m *mockGHClient) ValidateToken(ctx context.Context) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.login, nil
}

// --- Tests ---

func TestHealthCheck(t *testing.T) {
	h := NewHandler(newMockStore(), 24*time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	h.HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestCheck_MethodNotAllowed(t *testing.T) {
	h := NewHandler(newMockStore(), 24*time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/check", nil)
	w := httptest.NewRecorder()

	h.Check(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestCheck_InvalidBody(t *testing.T) {
	h := NewHandler(newMockStore(), 24*time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()

	h.Check(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCheck_MissingPRAuthor(t *testing.T) {
	h := NewHandler(newMockStore(), 24*time.Hour)

	body := models.CheckRequest{
		Repo:     "cncf/automation",
		PRNumber: 1,
		PRAuthor: "",
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewBuffer(bodyJSON))
	w := httptest.NewRecorder()

	h.Check(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCheck_MissingRepo(t *testing.T) {
	h := NewHandler(newMockStore(), 24*time.Hour)

	body := models.CheckRequest{
		Repo:     "",
		PRNumber: 1,
		PRAuthor: "testuser",
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewBuffer(bodyJSON))
	w := httptest.NewRecorder()

	h.Check(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCheck_MissingPRNumber(t *testing.T) {
	h := NewHandler(newMockStore(), 24*time.Hour)

	body := models.CheckRequest{
		Repo:     "cncf/automation",
		PRNumber: 0,
		PRAuthor: "testuser",
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewBuffer(bodyJSON))
	w := httptest.NewRecorder()

	h.Check(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCheck_ActiveCooldownReturnsVerdict(t *testing.T) {
	s := newMockStore()
	now := time.Now().UTC()
	until := now.Add(3 * 24 * time.Hour)
	s.cooldowns["spammer"] = &models.Cooldown{
		GitHubLogin:     "spammer",
		CurrentLevel:    0,
		CooldownUntil:   &until,
		LastTriggeredAt: &now,
		History:         []models.CooldownHistory{{TriggeredAt: now, Reason: "test", Level: 0}},
	}

	h := NewHandler(s, 24*time.Hour)

	body := models.CheckRequest{
		Repo:            "cncf/automation",
		PRNumber:        42,
		PRAuthor:        "spammer",
		LookbackDays:    30,
		EscalationTiers: []int{3, 7, 21},
		Thresholds: map[models.AccountAgeTier]models.Thresholds{
			models.TierNew: {KeywordFlagged: 1, PlainClosed: 2},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewBuffer(bodyJSON))
	ctx := context.WithValue(req.Context(), tokenContextKey, "fake-token")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	h.Check(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp models.CheckResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
}

func TestCheck_EvaluatorError(t *testing.T) {
	s := newMockStore()
	s.failOn = "GetCooldown" // Force evaluator to fail at step 1

	h := NewHandler(s, 24*time.Hour)

	body := models.CheckRequest{
		Repo:            "cncf/automation",
		PRNumber:        42,
		PRAuthor:        "testuser",
		LookbackDays:    30,
		EscalationTiers: []int{3, 7, 21},
		Thresholds: map[models.AccountAgeTier]models.Thresholds{
			models.TierNew: {KeywordFlagged: 1, PlainClosed: 2},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewBuffer(bodyJSON))
	ctx := context.WithValue(req.Context(), tokenContextKey, "fake-token")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	h.Check(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestCheck_DefaultsApplied(t *testing.T) {
	req := &models.CheckRequest{
		Repo:     "cncf/automation",
		PRNumber: 1,
		PRAuthor: "testuser",
	}

	err := validateRequest(req)
	if err != nil {
		t.Fatalf("validateRequest: %v", err)
	}

	if req.LookbackDays != 30 {
		t.Errorf("lookback_days = %d, want 30", req.LookbackDays)
	}
	if len(req.EscalationTiers) != 3 {
		t.Errorf("escalation_tiers length = %d, want 3", len(req.EscalationTiers))
	}
	if req.Thresholds == nil {
		t.Error("thresholds should not be nil after defaults applied")
	}
}

func TestCheck_DefaultsNotOverwritten(t *testing.T) {
	req := &models.CheckRequest{
		Repo:            "cncf/automation",
		PRNumber:        1,
		PRAuthor:        "testuser",
		LookbackDays:    60,
		EscalationTiers: []int{1, 2, 3},
		Thresholds: map[models.AccountAgeTier]models.Thresholds{
			models.TierNew: {KeywordFlagged: 5, PlainClosed: 10},
		},
	}

	err := validateRequest(req)
	if err != nil {
		t.Fatalf("validateRequest: %v", err)
	}

	if req.LookbackDays != 60 {
		t.Errorf("lookback_days = %d, want 60 (should not be overwritten)", req.LookbackDays)
	}
	if len(req.EscalationTiers) != 3 || req.EscalationTiers[0] != 1 {
		t.Errorf("escalation_tiers should not be overwritten")
	}
	if req.Thresholds[models.TierNew].KeywordFlagged != 5 {
		t.Errorf("thresholds should not be overwritten")
	}
}

func TestTokenMiddleware_MissingHeader(t *testing.T) {
	validator := NewTokenValidator(5 * time.Minute)

	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestTokenMiddleware_InvalidFormat(t *testing.T) {
	validator := NewTokenValidator(5 * time.Minute)

	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic not-a-bearer-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestTokenMiddleware_ValidToken(t *testing.T) {
	mock := &mockGHClient{login: "testuser"}
	factory := func(ctx context.Context, token string) ghlib.Client {
		return mock
	}
	validator := NewTokenValidatorWithFactory(5*time.Minute, factory)

	var capturedToken string
	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedToken = TokenFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ghp_valid123")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if capturedToken != "ghp_valid123" {
		t.Errorf("token in context = %q, want %q", capturedToken, "ghp_valid123")
	}
}

func TestTokenMiddleware_InvalidToken(t *testing.T) {
	mock := &mockGHClient{err: fmt.Errorf("bad credentials")}
	factory := func(ctx context.Context, token string) ghlib.Client {
		return mock
	}
	validator := NewTokenValidatorWithFactory(5*time.Minute, factory)

	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ghp_invalid")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestTokenMiddleware_CacheHit(t *testing.T) {
	callCount := 0
	mock := &mockGHClient{login: "testuser"}
	factory := func(ctx context.Context, token string) ghlib.Client {
		callCount++
		return mock
	}
	validator := NewTokenValidatorWithFactory(5*time.Minute, factory)

	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request — validates against GitHub
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer ghp_cached")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d", w1.Code, http.StatusOK)
	}
	if callCount != 1 {
		t.Fatalf("factory call count = %d, want 1", callCount)
	}

	// Second request — should use cache, not call factory again
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer ghp_cached")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want %d", w2.Code, http.StatusOK)
	}
	if callCount != 1 {
		t.Errorf("factory call count = %d, want 1 (should use cache)", callCount)
	}
}

func TestTokenMiddleware_CacheExpiry(t *testing.T) {
	callCount := 0
	mock := &mockGHClient{login: "testuser"}
	factory := func(ctx context.Context, token string) ghlib.Client {
		callCount++
		return mock
	}
	// Very short TTL so it expires between requests
	validator := NewTokenValidatorWithFactory(1*time.Millisecond, factory)

	handler := validator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer ghp_expiring")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if callCount != 1 {
		t.Fatalf("first call count = %d, want 1", callCount)
	}

	// Wait for cache to expire
	time.Sleep(5 * time.Millisecond)

	// Second request — cache expired, should validate again
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer ghp_expiring")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if callCount != 2 {
		t.Errorf("second call count = %d, want 2 (cache expired)", callCount)
	}
	_ = w1
	_ = w2
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer ghp_abc123", "ghp_abc123"},
		{"bearer ghp_abc123", "ghp_abc123"},
		{"BEARER ghp_abc123", "ghp_abc123"},
		{"Basic dXNlcjpwYXNz", ""},
		{"", ""},
		{"Bearer ", ""},
		{"OnlyOneWord", ""},
	}

	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			r.Header.Set("Authorization", tt.header)
		}
		got := extractBearerToken(r)
		if got != tt.want {
			t.Errorf("extractBearerToken(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestTokenFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	got := TokenFromContext(ctx)
	if got != "" {
		t.Errorf("TokenFromContext(empty) = %q, want empty", got)
	}
}

func TestValidationError_Error(t *testing.T) {
	err := errMissing("test_field")
	if err.Error() != "missing required field: test_field" {
		t.Errorf("Error() = %q, want %q", err.Error(), "missing required field: test_field")
	}
}
