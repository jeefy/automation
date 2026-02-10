package store

import (
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
)

// Store defines the storage interface for the PR cooldown service.
// Implementations must be safe for concurrent use.
type Store interface {
	// GetUserCache retrieves cached user profile data.
	// Returns nil, nil if the user is not cached.
	GetUserCache(login string) (*models.UserCache, error)

	// SetUserCache stores or updates cached user profile data.
	SetUserCache(cache *models.UserCache) error

	// GetPRActivity retrieves cached PR activity for a user since the given time.
	GetPRActivity(login string, since time.Time) ([]models.PRActivity, error)

	// SetPRActivity stores PR activity records, upserting on (login, repo, pr_number).
	SetPRActivity(activity []models.PRActivity) error

	// GetCooldown retrieves the cooldown state for a user.
	// Returns nil, nil if no cooldown exists.
	GetCooldown(login string) (*models.Cooldown, error)

	// SetCooldown stores or updates cooldown state for a user.
	SetCooldown(cooldown *models.Cooldown) error

	// Close closes the store and releases resources.
	Close() error
}
