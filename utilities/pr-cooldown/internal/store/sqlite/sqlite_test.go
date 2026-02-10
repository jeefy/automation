package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("creating test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUserCache_SetAndGet(t *testing.T) {
	s := newTestStore(t)

	created := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	cached := time.Date(2026, 2, 9, 12, 0, 0, 0, time.UTC)

	err := s.SetUserCache(&models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: created,
		CachedAt:         cached,
	})
	if err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	got, err := s.GetUserCache("testuser")
	if err != nil {
		t.Fatalf("GetUserCache: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil user cache")
	}
	if got.GitHubLogin != "testuser" {
		t.Errorf("login = %q, want %q", got.GitHubLogin, "testuser")
	}
	if !got.AccountCreatedAt.Equal(created) {
		t.Errorf("account_created_at = %v, want %v", got.AccountCreatedAt, created)
	}
	if !got.CachedAt.Equal(cached) {
		t.Errorf("cached_at = %v, want %v", got.CachedAt, cached)
	}
}

func TestUserCache_GetMissing(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetUserCache("nonexistent")
	if err != nil {
		t.Fatalf("GetUserCache: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestUserCache_Upsert(t *testing.T) {
	s := newTestStore(t)

	original := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	updated := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	err := s.SetUserCache(&models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: original,
		CachedAt:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("first SetUserCache: %v", err)
	}

	// Update with new cached_at
	newCachedAt := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	err = s.SetUserCache(&models.UserCache{
		GitHubLogin:      "testuser",
		AccountCreatedAt: updated,
		CachedAt:         newCachedAt,
	})
	if err != nil {
		t.Fatalf("second SetUserCache: %v", err)
	}

	got, err := s.GetUserCache("testuser")
	if err != nil {
		t.Fatalf("GetUserCache: %v", err)
	}
	if !got.AccountCreatedAt.Equal(updated) {
		t.Errorf("account_created_at = %v, want %v", got.AccountCreatedAt, updated)
	}
	if !got.CachedAt.Equal(newCachedAt) {
		t.Errorf("cached_at = %v, want %v", got.CachedAt, newCachedAt)
	}
}

func TestPRActivity_SetAndGet(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	closedAt := now.Add(-1 * time.Hour)

	activities := []models.PRActivity{
		{
			GitHubLogin:    "testuser",
			PRNumber:       1,
			RepoFullName:   "org/repo1",
			State:          "closed",
			KeywordFlagged: true,
			ClosedAt:       &closedAt,
			CachedAt:       now,
		},
		{
			GitHubLogin:    "testuser",
			PRNumber:       2,
			RepoFullName:   "org/repo2",
			State:          "closed",
			KeywordFlagged: false,
			ClosedAt:       &closedAt,
			CachedAt:       now,
		},
	}

	err := s.SetPRActivity(activities)
	if err != nil {
		t.Fatalf("SetPRActivity: %v", err)
	}

	since := now.Add(-24 * time.Hour)
	got, err := s.GetPRActivity("testuser", since)
	if err != nil {
		t.Fatalf("GetPRActivity: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d activities, want 2", len(got))
	}

	// Verify keyword flagging
	flaggedCount := 0
	for _, a := range got {
		if a.KeywordFlagged {
			flaggedCount++
		}
	}
	if flaggedCount != 1 {
		t.Errorf("keyword_flagged count = %d, want 1", flaggedCount)
	}
}

func TestPRActivity_FilterBySince(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	oldClosed := now.Add(-60 * 24 * time.Hour) // 60 days ago
	recentClosed := now.Add(-1 * time.Hour)    // 1 hour ago

	activities := []models.PRActivity{
		{
			GitHubLogin:  "testuser",
			PRNumber:     1,
			RepoFullName: "org/repo",
			State:        "closed",
			ClosedAt:     &oldClosed,
			CachedAt:     now,
		},
		{
			GitHubLogin:  "testuser",
			PRNumber:     2,
			RepoFullName: "org/repo",
			State:        "closed",
			ClosedAt:     &recentClosed,
			CachedAt:     now,
		},
	}

	if err := s.SetPRActivity(activities); err != nil {
		t.Fatalf("SetPRActivity: %v", err)
	}

	// Only get last 30 days
	since := now.Add(-30 * 24 * time.Hour)
	got, err := s.GetPRActivity("testuser", since)
	if err != nil {
		t.Fatalf("GetPRActivity: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d activities, want 1 (only recent)", len(got))
	}
	if got[0].PRNumber != 2 {
		t.Errorf("pr_number = %d, want 2", got[0].PRNumber)
	}
}

func TestPRActivity_Upsert(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	closedAt := now.Add(-1 * time.Hour)

	// Insert initial
	err := s.SetPRActivity([]models.PRActivity{
		{
			GitHubLogin:    "testuser",
			PRNumber:       1,
			RepoFullName:   "org/repo",
			State:          "closed",
			KeywordFlagged: false,
			ClosedAt:       &closedAt,
			CachedAt:       now,
		},
	})
	if err != nil {
		t.Fatalf("first SetPRActivity: %v", err)
	}

	// Upsert with keyword_flagged = true
	err = s.SetPRActivity([]models.PRActivity{
		{
			GitHubLogin:    "testuser",
			PRNumber:       1,
			RepoFullName:   "org/repo",
			State:          "closed",
			KeywordFlagged: true,
			ClosedAt:       &closedAt,
			CachedAt:       now,
		},
	})
	if err != nil {
		t.Fatalf("second SetPRActivity: %v", err)
	}

	since := now.Add(-24 * time.Hour)
	got, err := s.GetPRActivity("testuser", since)
	if err != nil {
		t.Fatalf("GetPRActivity: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d activities, want 1 (upserted)", len(got))
	}
	if !got[0].KeywordFlagged {
		t.Error("expected keyword_flagged to be true after upsert")
	}
}

func TestCooldown_SetAndGet(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	until := now.Add(3 * 24 * time.Hour)

	cd := &models.Cooldown{
		GitHubLogin:     "spammer",
		CurrentLevel:    1,
		CooldownUntil:   &until,
		LastTriggeredAt: &now,
		History: []models.CooldownHistory{
			{TriggeredAt: now, Reason: "2 keyword-flagged closed PRs", Level: 1},
		},
	}

	err := s.SetCooldown(cd)
	if err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	got, err := s.GetCooldown("spammer")
	if err != nil {
		t.Fatalf("GetCooldown: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil cooldown")
	}
	if got.CurrentLevel != 1 {
		t.Errorf("current_level = %d, want 1", got.CurrentLevel)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.Equal(until) {
		t.Errorf("cooldown_until = %v, want %v", got.CooldownUntil, until)
	}
	if len(got.History) != 1 {
		t.Fatalf("history length = %d, want 1", len(got.History))
	}
	if got.History[0].Level != 1 {
		t.Errorf("history[0].level = %d, want 1", got.History[0].Level)
	}
}

func TestCooldown_GetMissing(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetCooldown("nobody")
	if err != nil {
		t.Fatalf("GetCooldown: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestCooldown_PermanentBan(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)

	// Permanent ban = nil CooldownUntil
	cd := &models.Cooldown{
		GitHubLogin:     "permabanned",
		CurrentLevel:    3,
		CooldownUntil:   nil,
		LastTriggeredAt: &now,
		History: []models.CooldownHistory{
			{TriggeredAt: now, Reason: "permanent ban", Level: 3},
		},
	}

	err := s.SetCooldown(cd)
	if err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	got, err := s.GetCooldown("permabanned")
	if err != nil {
		t.Fatalf("GetCooldown: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil cooldown")
	}
	if got.CooldownUntil != nil {
		t.Errorf("cooldown_until should be nil for permanent ban, got %v", got.CooldownUntil)
	}
	if got.CurrentLevel != 3 {
		t.Errorf("current_level = %d, want 3", got.CurrentLevel)
	}
}

func TestCooldown_Upsert(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	until1 := now.Add(3 * 24 * time.Hour)
	until2 := now.Add(7 * 24 * time.Hour)

	// Level 1
	err := s.SetCooldown(&models.Cooldown{
		GitHubLogin:     "escalating",
		CurrentLevel:    1,
		CooldownUntil:   &until1,
		LastTriggeredAt: &now,
		History: []models.CooldownHistory{
			{TriggeredAt: now, Reason: "first offense", Level: 1},
		},
	})
	if err != nil {
		t.Fatalf("first SetCooldown: %v", err)
	}

	// Escalate to level 2
	later := now.Add(4 * 24 * time.Hour)
	err = s.SetCooldown(&models.Cooldown{
		GitHubLogin:     "escalating",
		CurrentLevel:    2,
		CooldownUntil:   &until2,
		LastTriggeredAt: &later,
		History: []models.CooldownHistory{
			{TriggeredAt: now, Reason: "first offense", Level: 1},
			{TriggeredAt: later, Reason: "second offense", Level: 2},
		},
	})
	if err != nil {
		t.Fatalf("second SetCooldown: %v", err)
	}

	got, err := s.GetCooldown("escalating")
	if err != nil {
		t.Fatalf("GetCooldown: %v", err)
	}
	if got.CurrentLevel != 2 {
		t.Errorf("current_level = %d, want 2", got.CurrentLevel)
	}
	if len(got.History) != 2 {
		t.Fatalf("history length = %d, want 2", len(got.History))
	}
}
