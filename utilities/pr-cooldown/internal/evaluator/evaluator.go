package evaluator

import (
	"context"
	"fmt"
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/github"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/store"
)

// Evaluator implements the 5-step PR cooldown evaluation logic.
type Evaluator struct {
	store    store.Store
	gh       github.Client
	cacheTTL time.Duration
}

// New creates a new Evaluator.
func New(s store.Store, gh github.Client, cacheTTL time.Duration) *Evaluator {
	return &Evaluator{
		store:    s,
		gh:       gh,
		cacheTTL: cacheTTL,
	}
}

// Check evaluates a PR author and returns a verdict.
func (e *Evaluator) Check(ctx context.Context, req models.CheckRequest) (*models.CheckResponse, error) {
	now := time.Now().UTC()

	// Step 1: Check active cooldown
	cooldown, err := e.store.GetCooldown(req.PRAuthor)
	if err != nil {
		return nil, fmt.Errorf("checking cooldown: %w", err)
	}
	if cooldown != nil {
		if isActiveCooldown(cooldown, now) {
			level := cooldown.CurrentLevel
			return &models.CheckResponse{
				Verdict:       models.VerdictCooldown,
				Reason:        formatCooldownReason(cooldown),
				CooldownUntil: cooldown.CooldownUntil,
				CooldownLevel: &level,
			}, nil
		}
	}

	// Step 2: Fetch/refresh user profile
	userCache, err := e.getOrFetchUser(ctx, req.PRAuthor, now)
	if err != nil {
		return nil, fmt.Errorf("fetching user: %w", err)
	}

	tier := models.AccountAgeTierFromAge(userCache.AccountCreatedAt, now)

	// Step 3: Fetch/refresh PR activity
	lookbackSince := now.AddDate(0, 0, -req.LookbackDays)
	activities, err := e.getOrFetchPRActivity(ctx, req.PRAuthor, lookbackSince, req.Keywords, now)
	if err != nil {
		return nil, fmt.Errorf("fetching PR activity: %w", err)
	}

	// Count keyword-flagged and plain closed PRs
	keywordFlagged := 0
	plainClosed := 0
	for _, a := range activities {
		if a.State == "closed" {
			if a.KeywordFlagged {
				keywordFlagged++
			} else {
				plainClosed++
			}
		}
	}

	// Step 4: Apply thresholds
	thresholds, ok := req.Thresholds[tier]
	if !ok {
		// Default to most lenient if tier not configured
		thresholds = models.Thresholds{KeywordFlagged: 2, PlainClosed: 4}
	}

	triggered := false
	reason := ""
	if thresholds.KeywordFlagged > 0 && keywordFlagged >= thresholds.KeywordFlagged {
		triggered = true
		reason = fmt.Sprintf("%d keyword-flagged closed PRs (threshold: %d for %s accounts)",
			keywordFlagged, thresholds.KeywordFlagged, tier)
	} else if thresholds.PlainClosed > 0 && plainClosed >= thresholds.PlainClosed {
		triggered = true
		reason = fmt.Sprintf("%d closed PRs without merge (threshold: %d for %s accounts)",
			plainClosed, thresholds.PlainClosed, tier)
	}

	if triggered {
		newCooldown, err := e.escalateCooldown(cooldown, req.PRAuthor, reason, req.EscalationTiers, now)
		if err != nil {
			return nil, fmt.Errorf("escalating cooldown: %w", err)
		}

		level := newCooldown.CurrentLevel
		return &models.CheckResponse{
			Verdict:             models.VerdictCooldown,
			Reason:              reason,
			CooldownUntil:       newCooldown.CooldownUntil,
			CooldownLevel:       &level,
			KeywordFlaggedCount: keywordFlagged,
			PlainClosedCount:    plainClosed,
			AccountAgeTier:      tier,
		}, nil
	}

	// Step 5: Allow
	return &models.CheckResponse{
		Verdict:             models.VerdictAllow,
		KeywordFlaggedCount: keywordFlagged,
		PlainClosedCount:    plainClosed,
		AccountAgeTier:      tier,
	}, nil
}

// getOrFetchUser retrieves the user from cache or fetches from GitHub.
func (e *Evaluator) getOrFetchUser(ctx context.Context, login string, now time.Time) (*models.UserCache, error) {
	cached, err := e.store.GetUserCache(login)
	if err != nil {
		return nil, err
	}
	if cached != nil && now.Sub(cached.CachedAt) < e.cacheTTL {
		return cached, nil
	}

	user, err := e.gh.GetUser(ctx, login)
	if err != nil {
		return nil, err
	}

	if err := e.store.SetUserCache(user); err != nil {
		return nil, fmt.Errorf("caching user: %w", err)
	}

	return user, nil
}

// getOrFetchPRActivity retrieves PR activity from cache or fetches from GitHub.
func (e *Evaluator) getOrFetchPRActivity(ctx context.Context, login string, since time.Time, keywords []string, now time.Time) ([]models.PRActivity, error) {
	cached, err := e.store.GetPRActivity(login, since)
	if err != nil {
		return nil, err
	}

	// Check if cache is fresh enough
	if len(cached) > 0 && now.Sub(cached[0].CachedAt) < e.cacheTTL {
		return cached, nil
	}

	// Fetch from GitHub
	activities, err := e.gh.GetClosedPRs(ctx, login, since)
	if err != nil {
		return nil, err
	}

	// Check keywords in comments for each PR
	for i := range activities {
		if len(keywords) > 0 {
			flagged, err := e.gh.CheckKeywordsInComments(
				ctx, activities[i].RepoFullName, activities[i].PRNumber, login, keywords,
			)
			if err != nil {
				return nil, fmt.Errorf("checking keywords for %s#%d: %w",
					activities[i].RepoFullName, activities[i].PRNumber, err)
			}
			activities[i].KeywordFlagged = flagged
		}
	}

	// Cache the results
	if len(activities) > 0 {
		if err := e.store.SetPRActivity(activities); err != nil {
			return nil, fmt.Errorf("caching PR activity: %w", err)
		}
	}

	return activities, nil
}

// escalateCooldown creates or escalates a cooldown for the user.
func (e *Evaluator) escalateCooldown(existing *models.Cooldown, login, reason string, tiers []int, now time.Time) (*models.Cooldown, error) {
	var cd models.Cooldown
	if existing != nil {
		cd = *existing
	} else {
		cd = models.Cooldown{
			GitHubLogin:  login,
			CurrentLevel: 0,
			History:      []models.CooldownHistory{},
		}
	}

	// Determine the tier index
	tierIdx := cd.CurrentLevel
	if existing != nil {
		// Escalate to next level
		tierIdx = cd.CurrentLevel + 1
	}

	// Cap at the last tier
	if tierIdx >= len(tiers) {
		tierIdx = len(tiers) - 1
	}

	cd.CurrentLevel = tierIdx

	// Calculate cooldown_until
	days := tiers[tierIdx]
	if days == 0 {
		// Permanent ban
		cd.CooldownUntil = nil
	} else {
		until := now.AddDate(0, 0, days)
		cd.CooldownUntil = &until
	}

	cd.LastTriggeredAt = &now
	cd.History = append(cd.History, models.CooldownHistory{
		TriggeredAt: now,
		Reason:      reason,
		Level:       tierIdx,
	})

	if err := e.store.SetCooldown(&cd); err != nil {
		return nil, fmt.Errorf("saving cooldown: %w", err)
	}

	return &cd, nil
}

// isActiveCooldown checks if a cooldown is currently active.
func isActiveCooldown(cd *models.Cooldown, now time.Time) bool {
	if cd.CooldownUntil == nil {
		// Permanent ban
		return true
	}
	return cd.CooldownUntil.After(now)
}

// formatCooldownReason returns a human-readable description of the active cooldown.
func formatCooldownReason(cd *models.Cooldown) string {
	if cd.CooldownUntil == nil {
		return "Permanently banned"
	}
	return fmt.Sprintf("Active cooldown until %s", cd.CooldownUntil.Format(time.RFC3339))
}
