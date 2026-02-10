package evaluator

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	if m.failOn == "SetUserCache" {
		return fmt.Errorf("store error")
	}
	m.users[cache.GitHubLogin] = cache
	return nil
}

func (m *mockStore) GetPRActivity(login string, since time.Time) ([]models.PRActivity, error) {
	if m.failOn == "GetPRActivity" {
		return nil, fmt.Errorf("store error")
	}
	var result []models.PRActivity
	for _, a := range m.activities[login] {
		if a.ClosedAt != nil && !a.ClosedAt.Before(since) {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockStore) SetPRActivity(activities []models.PRActivity) error {
	if m.failOn == "SetPRActivity" {
		return fmt.Errorf("store error")
	}
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
	if m.failOn == "SetCooldown" {
		return fmt.Errorf("store error")
	}
	m.cooldowns[cooldown.GitHubLogin] = cooldown
	return nil
}

func (m *mockStore) Close() error { return nil }

// --- Mock GitHub Client ---

type mockGitHubClient struct {
	user       *models.UserCache
	closedPRs  []models.PRActivity
	keywordHit map[string]bool // key: "repo#number"
	failOn     string          // method name to fail on
}

func (m *mockGitHubClient) GetUser(ctx context.Context, login string) (*models.UserCache, error) {
	if m.failOn == "GetUser" {
		return nil, fmt.Errorf("github error")
	}
	return m.user, nil
}

func (m *mockGitHubClient) GetClosedPRs(ctx context.Context, login string, since time.Time) ([]models.PRActivity, error) {
	if m.failOn == "GetClosedPRs" {
		return nil, fmt.Errorf("github error")
	}
	return m.closedPRs, nil
}

func (m *mockGitHubClient) CheckKeywordsInComments(ctx context.Context, repoFullName string, prNumber int, prAuthor string, keywords []string) (bool, error) {
	if m.failOn == "CheckKeywordsInComments" {
		return false, fmt.Errorf("github error")
	}
	key := repoFullName + "#" + string(rune(prNumber+'0'))
	return m.keywordHit[key], nil
}

func (m *mockGitHubClient) ValidateToken(ctx context.Context) (string, error) {
	return "test", nil
}

// --- Helper ---

func defaultRequest() models.CheckRequest {
	return models.CheckRequest{
		Repo:            "cncf/automation",
		PRNumber:        99,
		PRAuthor:        "testuser",
		LookbackDays:    30,
		EscalationTiers: []int{3, 7, 21, 0},
		Keywords:        []string{"spam", "slop"},
		Thresholds: map[models.AccountAgeTier]models.Thresholds{
			models.TierNew:         {KeywordFlagged: 1, PlainClosed: 2},
			models.TierEstablished: {KeywordFlagged: 2, PlainClosed: 3},
			models.TierVeteran:     {KeywordFlagged: 2, PlainClosed: 4},
		},
	}
}

// --- Tests ---

func TestCheck_AllowNoHistory(t *testing.T) {
	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "testuser",
			AccountCreatedAt: time.Now().AddDate(-1, 0, 0),
			CachedAt:         time.Now(),
		},
		closedPRs: nil,
	}

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictAllow)
	}
	if resp.AccountAgeTier != models.TierEstablished {
		t.Errorf("tier = %q, want %q", resp.AccountAgeTier, models.TierEstablished)
	}
}

func TestCheck_AllowFirstPRNewAccount(t *testing.T) {
	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "newuser",
			AccountCreatedAt: time.Now().AddDate(0, 0, -10),
			CachedAt:         time.Now(),
		},
		closedPRs: nil,
	}

	req := defaultRequest()
	req.PRAuthor = "newuser"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictAllow)
	}
	if resp.AccountAgeTier != models.TierNew {
		t.Errorf("tier = %q, want %q", resp.AccountAgeTier, models.TierNew)
	}
}

func TestCheck_CooldownKeywordThreshold_NewAccount(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "spammer",
			AccountCreatedAt: now.AddDate(0, 0, -10),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{
				GitHubLogin:  "spammer",
				PRNumber:     1,
				RepoFullName: "org/repo1",
				State:        "closed",
				ClosedAt:     &closedAt,
				CachedAt:     now,
			},
		},
		keywordHit: map[string]bool{"org/repo1#1": true},
	}

	req := defaultRequest()
	req.PRAuthor = "spammer"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.CooldownLevel == nil || *resp.CooldownLevel != 0 {
		t.Errorf("cooldown_level = %v, want 0", resp.CooldownLevel)
	}
	if resp.CooldownUntil == nil {
		t.Fatal("cooldown_until should not be nil for 3-day cooldown")
	}
	if resp.KeywordFlaggedCount != 1 {
		t.Errorf("keyword_flagged_count = %d, want 1", resp.KeywordFlaggedCount)
	}
}

func TestCheck_CooldownPlainThreshold_NewAccount(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "spammer",
			AccountCreatedAt: now.AddDate(0, 0, -10),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "spammer", PRNumber: 1, RepoFullName: "org/repo1", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "spammer", PRNumber: 2, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	req.PRAuthor = "spammer"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.PlainClosedCount != 2 {
		t.Errorf("plain_closed_count = %d, want 2", resp.PlainClosedCount)
	}
}

func TestCheck_CooldownVeteranHigherThreshold(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "veteran",
			AccountCreatedAt: now.AddDate(-3, 0, 0),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "veteran", PRNumber: 1, RepoFullName: "org/repo1", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "veteran", PRNumber: 2, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "veteran", PRNumber: 3, RepoFullName: "org/repo3", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	req.PRAuthor = "veteran"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q (veteran has higher threshold)", resp.Verdict, models.VerdictAllow)
	}
}

func TestCheck_CooldownVeteranExceedsThreshold(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "veteran",
			AccountCreatedAt: now.AddDate(-3, 0, 0),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "veteran", PRNumber: 1, RepoFullName: "org/repo1", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "veteran", PRNumber: 2, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "veteran", PRNumber: 3, RepoFullName: "org/repo3", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "veteran", PRNumber: 4, RepoFullName: "org/repo4", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	req.PRAuthor = "veteran"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
}

func TestCheck_ActiveCooldownShortCircuit(t *testing.T) {
	now := time.Now().UTC()
	until := now.Add(3 * 24 * time.Hour)

	s := newMockStore()
	s.cooldowns["spammer"] = &models.Cooldown{
		GitHubLogin:     "spammer",
		CurrentLevel:    0,
		CooldownUntil:   &until,
		LastTriggeredAt: &now,
		History:         []models.CooldownHistory{{TriggeredAt: now, Reason: "test", Level: 0}},
	}

	ghClient := &mockGitHubClient{}

	req := defaultRequest()
	req.PRAuthor = "spammer"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.CooldownLevel == nil || *resp.CooldownLevel != 0 {
		t.Errorf("cooldown_level = %v, want 0", resp.CooldownLevel)
	}
}

func TestCheck_ExpiredCooldownAllows(t *testing.T) {
	now := time.Now().UTC()
	expired := now.Add(-1 * time.Hour)

	s := newMockStore()
	s.cooldowns["reformed"] = &models.Cooldown{
		GitHubLogin:     "reformed",
		CurrentLevel:    0,
		CooldownUntil:   &expired,
		LastTriggeredAt: &now,
		History:         []models.CooldownHistory{{TriggeredAt: now, Reason: "test", Level: 0}},
	}

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "reformed",
			AccountCreatedAt: now.AddDate(-1, 0, 0),
			CachedAt:         now,
		},
		closedPRs: nil,
	}

	req := defaultRequest()
	req.PRAuthor = "reformed"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q (cooldown expired)", resp.Verdict, models.VerdictAllow)
	}
}

func TestCheck_EscalationLevels(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)
	expired := now.Add(-1 * time.Hour)

	s := newMockStore()
	s.cooldowns["repeat"] = &models.Cooldown{
		GitHubLogin:     "repeat",
		CurrentLevel:    0,
		CooldownUntil:   &expired,
		LastTriggeredAt: &now,
		History:         []models.CooldownHistory{{TriggeredAt: now, Reason: "first", Level: 0}},
	}

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "repeat",
			AccountCreatedAt: now.AddDate(0, 0, -10),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "repeat", PRNumber: 10, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "repeat", PRNumber: 11, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	req.PRAuthor = "repeat"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Fatalf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.CooldownLevel == nil || *resp.CooldownLevel != 1 {
		t.Errorf("cooldown_level = %v, want 1", resp.CooldownLevel)
	}
}

func TestCheck_PermanentBan(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)
	expired := now.Add(-1 * time.Hour)

	s := newMockStore()
	s.cooldowns["permaban"] = &models.Cooldown{
		GitHubLogin:   "permaban",
		CurrentLevel:  2,
		CooldownUntil: &expired,
		History: []models.CooldownHistory{
			{TriggeredAt: now, Reason: "first", Level: 0},
			{TriggeredAt: now, Reason: "second", Level: 1},
			{TriggeredAt: now, Reason: "third", Level: 2},
		},
	}

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "permaban",
			AccountCreatedAt: now.AddDate(0, 0, -10),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "permaban", PRNumber: 20, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "permaban", PRNumber: 21, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	req.PRAuthor = "permaban"
	req.EscalationTiers = []int{3, 7, 21, 0}

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Fatalf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.CooldownLevel == nil || *resp.CooldownLevel != 3 {
		t.Errorf("cooldown_level = %v, want 3", resp.CooldownLevel)
	}
	if resp.CooldownUntil != nil {
		t.Errorf("cooldown_until should be nil for permanent ban, got %v", resp.CooldownUntil)
	}
}

func TestCheck_PermanentBanShortCircuit(t *testing.T) {
	s := newMockStore()
	s.cooldowns["permabanned"] = &models.Cooldown{
		GitHubLogin:   "permabanned",
		CurrentLevel:  3,
		CooldownUntil: nil,
		History:       []models.CooldownHistory{},
	}

	ghClient := &mockGitHubClient{}

	req := defaultRequest()
	req.PRAuthor = "permabanned"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.Reason != "Permanently banned" {
		t.Errorf("reason = %q, want %q", resp.Reason, "Permanently banned")
	}
}

func TestCheck_CapsAtLastTierWithoutZero(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)
	expired := now.Add(-1 * time.Hour)

	s := newMockStore()
	s.cooldowns["capped"] = &models.Cooldown{
		GitHubLogin:   "capped",
		CurrentLevel:  2,
		CooldownUntil: &expired,
		History: []models.CooldownHistory{
			{TriggeredAt: now, Reason: "first", Level: 0},
			{TriggeredAt: now, Reason: "second", Level: 1},
			{TriggeredAt: now, Reason: "third", Level: 2},
		},
	}

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "capped",
			AccountCreatedAt: now.AddDate(0, 0, -10),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "capped", PRNumber: 30, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "capped", PRNumber: 31, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	req.PRAuthor = "capped"
	req.EscalationTiers = []int{3, 7, 21}

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictCooldown {
		t.Fatalf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
	if resp.CooldownLevel == nil || *resp.CooldownLevel != 2 {
		t.Errorf("cooldown_level = %v, want 2 (capped)", resp.CooldownLevel)
	}
	if resp.CooldownUntil == nil {
		t.Error("cooldown_until should not be nil (should be 21 days)")
	}
}

// --- Cache hit tests ---

func TestCheck_UsesCachedUser(t *testing.T) {
	now := time.Now().UTC()

	s := newMockStore()
	// Pre-populate the user cache with fresh data
	s.users["cached-user"] = &models.UserCache{
		GitHubLogin:      "cached-user",
		AccountCreatedAt: now.AddDate(-1, 0, 0),
		CachedAt:         now, // fresh cache
	}

	// GitHub client should NOT be called for user if cache is fresh
	ghClient := &mockGitHubClient{
		failOn: "GetUser", // fail if called
	}

	req := defaultRequest()
	req.PRAuthor = "cached-user"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictAllow)
	}
}

func TestCheck_UsesCachedPRActivity(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	s.users["cached-activity"] = &models.UserCache{
		GitHubLogin:      "cached-activity",
		AccountCreatedAt: now.AddDate(0, 0, -10),
		CachedAt:         now,
	}
	// Pre-populate PR activity cache with fresh data
	s.activities["cached-activity"] = []models.PRActivity{
		{GitHubLogin: "cached-activity", PRNumber: 1, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		{GitHubLogin: "cached-activity", PRNumber: 2, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
	}

	// GitHub client should NOT be called for PRs if cache is fresh
	ghClient := &mockGitHubClient{
		failOn: "GetClosedPRs",
	}

	req := defaultRequest()
	req.PRAuthor = "cached-activity"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// New account with 2 plain closed PRs -> cooldown
	if resp.Verdict != models.VerdictCooldown {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictCooldown)
	}
}

func TestCheck_StaleUserCacheRefreshes(t *testing.T) {
	now := time.Now().UTC()
	staleTime := now.Add(-48 * time.Hour) // older than 24h TTL

	s := newMockStore()
	s.users["stale-user"] = &models.UserCache{
		GitHubLogin:      "stale-user",
		AccountCreatedAt: now.AddDate(-1, 0, 0),
		CachedAt:         staleTime, // stale
	}

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "stale-user",
			AccountCreatedAt: now.AddDate(-1, 0, 0),
			CachedAt:         now,
		},
	}

	req := defaultRequest()
	req.PRAuthor = "stale-user"

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictAllow)
	}
}

// --- Error handling tests ---

func TestCheck_ErrorGetCooldown(t *testing.T) {
	s := newMockStore()
	s.failOn = "GetCooldown"

	ghClient := &mockGitHubClient{}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from GetCooldown")
	}
}

func TestCheck_ErrorGetUserCache(t *testing.T) {
	s := newMockStore()
	s.failOn = "GetUserCache"

	ghClient := &mockGitHubClient{}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from GetUserCache")
	}
}

func TestCheck_ErrorGetUser(t *testing.T) {
	s := newMockStore()

	ghClient := &mockGitHubClient{
		failOn: "GetUser",
	}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from GetUser")
	}
}

func TestCheck_ErrorSetUserCache(t *testing.T) {
	s := newMockStore()
	s.failOn = "SetUserCache"

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "testuser",
			AccountCreatedAt: time.Now().AddDate(-1, 0, 0),
			CachedAt:         time.Now(),
		},
	}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from SetUserCache")
	}
}

func TestCheck_ErrorGetPRActivity(t *testing.T) {
	now := time.Now().UTC()
	s := newMockStore()
	s.users["testuser"] = &models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: now.AddDate(-1, 0, 0),
		CachedAt:         now,
	}
	s.failOn = "GetPRActivity"

	ghClient := &mockGitHubClient{}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from GetPRActivity")
	}
}

func TestCheck_ErrorGetClosedPRs(t *testing.T) {
	now := time.Now().UTC()
	s := newMockStore()
	s.users["testuser"] = &models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: now.AddDate(-1, 0, 0),
		CachedAt:         now,
	}

	ghClient := &mockGitHubClient{
		failOn: "GetClosedPRs",
	}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from GetClosedPRs")
	}
}

func TestCheck_ErrorCheckKeywordsInComments(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)
	s := newMockStore()
	s.users["testuser"] = &models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: now.AddDate(-1, 0, 0),
		CachedAt:         now,
	}

	ghClient := &mockGitHubClient{
		closedPRs: []models.PRActivity{
			{GitHubLogin: "testuser", PRNumber: 1, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		failOn: "CheckKeywordsInComments",
	}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from CheckKeywordsInComments")
	}
}

func TestCheck_ErrorSetPRActivity(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)
	s := newMockStore()
	s.users["testuser"] = &models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: now.AddDate(-1, 0, 0),
		CachedAt:         now,
	}
	s.failOn = "SetPRActivity"

	ghClient := &mockGitHubClient{
		closedPRs: []models.PRActivity{
			{GitHubLogin: "testuser", PRNumber: 1, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from SetPRActivity")
	}
}

func TestCheck_ErrorSetCooldown(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	s.failOn = "SetCooldown"

	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "testuser",
			AccountCreatedAt: now.AddDate(0, 0, -10), // new
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "testuser", PRNumber: 1, RepoFullName: "org/repo1", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "testuser", PRNumber: 2, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	eval := New(s, ghClient, 24*time.Hour)
	_, err := eval.Check(context.Background(), defaultRequest())
	if err == nil {
		t.Fatal("expected error from SetCooldown")
	}
}

// --- Threshold edge cases ---

func TestCheck_NoKeywordsSkipsKeywordCheck(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "testuser",
			AccountCreatedAt: now.AddDate(-1, 0, 0),
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "testuser", PRNumber: 1, RepoFullName: "org/repo", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		failOn: "CheckKeywordsInComments", // would fail if called
	}

	req := defaultRequest()
	req.Keywords = nil // no keywords

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v (should not call CheckKeywordsInComments with no keywords)", err)
	}
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q", resp.Verdict, models.VerdictAllow)
	}
}

func TestCheck_MissingTierThresholdUsesDefault(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-2 * time.Hour)

	s := newMockStore()
	ghClient := &mockGitHubClient{
		user: &models.UserCache{
			GitHubLogin:      "testuser",
			AccountCreatedAt: now.AddDate(-1, 0, 0), // established
			CachedAt:         now,
		},
		closedPRs: []models.PRActivity{
			{GitHubLogin: "testuser", PRNumber: 1, RepoFullName: "org/repo1", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "testuser", PRNumber: 2, RepoFullName: "org/repo2", State: "closed", ClosedAt: &closedAt, CachedAt: now},
			{GitHubLogin: "testuser", PRNumber: 3, RepoFullName: "org/repo3", State: "closed", ClosedAt: &closedAt, CachedAt: now},
		},
		keywordHit: map[string]bool{},
	}

	req := defaultRequest()
	// Only provide threshold for "new" tier, not "established"
	req.Thresholds = map[models.AccountAgeTier]models.Thresholds{
		models.TierNew: {KeywordFlagged: 1, PlainClosed: 2},
	}

	eval := New(s, ghClient, 24*time.Hour)
	resp, err := eval.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Default for missing tier is {KeywordFlagged: 2, PlainClosed: 4}
	// 3 plain closed < 4 -> allow
	if resp.Verdict != models.VerdictAllow {
		t.Errorf("verdict = %q, want %q (should use default lenient threshold)", resp.Verdict, models.VerdictAllow)
	}
}

func TestAccountAgeTierFromAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		createdAt time.Time
		want      models.AccountAgeTier
	}{
		{"brand new", now.AddDate(0, 0, -1), models.TierNew},
		{"89 days", now.AddDate(0, 0, -89), models.TierNew},
		{"91 days", now.AddDate(0, 0, -91), models.TierEstablished},
		{"1 year", now.AddDate(-1, 0, 0), models.TierEstablished},
		{"2 years", now.AddDate(-2, 0, -1), models.TierVeteran},
		{"5 years", now.AddDate(-5, 0, 0), models.TierVeteran},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := models.AccountAgeTierFromAge(tt.createdAt, now)
			if got != tt.want {
				t.Errorf("AccountAgeTierFromAge() = %q, want %q", got, tt.want)
			}
		})
	}
}
