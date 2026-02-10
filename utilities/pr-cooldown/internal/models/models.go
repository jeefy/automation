package models

import "time"

// AccountAgeTier represents the age bracket of a GitHub account.
type AccountAgeTier string

const (
	TierNew         AccountAgeTier = "new"         // < 90 days
	TierEstablished AccountAgeTier = "established" // 90 days – 2 years
	TierVeteran     AccountAgeTier = "veteran"     // 2+ years
)

// Verdict is the result of a PR cooldown check.
type Verdict string

const (
	VerdictAllow    Verdict = "allow"
	VerdictCooldown Verdict = "cooldown"
)

// Thresholds defines the number of closed PRs that trigger a cooldown
// for a given account age tier.
type Thresholds struct {
	KeywordFlagged int `json:"keyword_flagged"`
	PlainClosed    int `json:"plain_closed"`
}

// CheckRequest is the payload sent by the GitHub Action to the service.
type CheckRequest struct {
	Repo            string                        `json:"repo"`
	PRNumber        int                           `json:"pr_number"`
	PRAuthor        string                        `json:"pr_author"`
	LookbackDays    int                           `json:"lookback_days"`
	EscalationTiers []int                         `json:"escalation_tiers"`
	Keywords        []string                      `json:"keywords"`
	Thresholds      map[AccountAgeTier]Thresholds `json:"thresholds"`
}

// CheckResponse is the verdict returned by the service to the GitHub Action.
type CheckResponse struct {
	Verdict             Verdict        `json:"verdict"`
	Reason              string         `json:"reason,omitempty"`
	CooldownUntil       *time.Time     `json:"cooldown_until,omitempty"`
	CooldownLevel       *int           `json:"cooldown_level,omitempty"`
	KeywordFlaggedCount int            `json:"keyword_flagged_count"`
	PlainClosedCount    int            `json:"plain_closed_count"`
	AccountAgeTier      AccountAgeTier `json:"account_age_tier"`
}

// UserCache holds cached GitHub user profile data.
type UserCache struct {
	GitHubLogin      string    `json:"github_login"`
	AccountCreatedAt time.Time `json:"account_created_at"`
	CachedAt         time.Time `json:"cached_at"`
}

// PRActivity represents a cached PR and its state.
type PRActivity struct {
	GitHubLogin    string     `json:"github_login"`
	PRNumber       int        `json:"pr_number"`
	RepoFullName   string     `json:"repo_full_name"`
	State          string     `json:"state"`
	KeywordFlagged bool       `json:"keyword_flagged"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	CachedAt       time.Time  `json:"cached_at"`
}

// CooldownHistory records a single cooldown trigger event.
type CooldownHistory struct {
	TriggeredAt time.Time `json:"triggered_at"`
	Reason      string    `json:"reason"`
	Level       int       `json:"level"`
}

// Cooldown holds the cooldown state for a GitHub user.
type Cooldown struct {
	GitHubLogin     string            `json:"github_login"`
	CurrentLevel    int               `json:"current_level"`
	CooldownUntil   *time.Time        `json:"cooldown_until"`
	LastTriggeredAt *time.Time        `json:"last_triggered_at"`
	History         []CooldownHistory `json:"history"`
}

// AccountAgeTierFromAge determines the account age tier from account creation time.
func AccountAgeTierFromAge(createdAt time.Time, now time.Time) AccountAgeTier {
	age := now.Sub(createdAt)
	switch {
	case age < 90*24*time.Hour:
		return TierNew
	case age < 2*365*24*time.Hour:
		return TierEstablished
	default:
		return TierVeteran
	}
}
