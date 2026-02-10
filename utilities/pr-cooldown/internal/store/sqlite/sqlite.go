package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/store"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements store.Store using SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// Ensure SQLiteStore implements store.Store at compile time.
var _ store.Store = (*SQLiteStore)(nil)

// New creates a new SQLiteStore and initializes the database schema.
func New(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db: %w", err)
	}

	// Enable WAL mode for better concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating tables: %w", err)
	}

	return s, nil
}

func (s *SQLiteStore) createTables() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS user_cache (
			github_login TEXT PRIMARY KEY,
			account_created_at TIMESTAMP NOT NULL,
			cached_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pr_activity_cache (
			github_login TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			repo_full_name TEXT NOT NULL,
			state TEXT NOT NULL,
			keyword_flagged BOOLEAN NOT NULL DEFAULT 0,
			closed_at TIMESTAMP,
			cached_at TIMESTAMP NOT NULL,
			PRIMARY KEY (github_login, repo_full_name, pr_number)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pr_activity_login_closed
			ON pr_activity_cache(github_login, closed_at)`,
		`CREATE TABLE IF NOT EXISTS cooldowns (
			github_login TEXT PRIMARY KEY,
			current_level INTEGER NOT NULL DEFAULT 0,
			cooldown_until TIMESTAMP,
			last_triggered_at TIMESTAMP,
			history TEXT NOT NULL DEFAULT '[]'
		)`,
	}

	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("executing %q: %w", stmt, err)
		}
	}
	return nil
}

// GetUserCache retrieves cached user profile data.
func (s *SQLiteStore) GetUserCache(login string) (*models.UserCache, error) {
	row := s.db.QueryRow(
		`SELECT github_login, account_created_at, cached_at FROM user_cache WHERE github_login = ?`,
		login,
	)

	var u models.UserCache
	var createdAt, cachedAt string
	err := row.Scan(&u.GitHubLogin, &createdAt, &cachedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning user cache: %w", err)
	}

	u.AccountCreatedAt, err = time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing account_created_at: %w", err)
	}
	u.CachedAt, err = time.Parse(time.RFC3339, cachedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing cached_at: %w", err)
	}

	return &u, nil
}

// SetUserCache stores or updates cached user profile data.
func (s *SQLiteStore) SetUserCache(cache *models.UserCache) error {
	_, err := s.db.Exec(
		`INSERT INTO user_cache (github_login, account_created_at, cached_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(github_login) DO UPDATE SET
			account_created_at = excluded.account_created_at,
			cached_at = excluded.cached_at`,
		cache.GitHubLogin,
		cache.AccountCreatedAt.Format(time.RFC3339),
		cache.CachedAt.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("upserting user cache: %w", err)
	}
	return nil
}

// GetPRActivity retrieves cached PR activity for a user since the given time.
func (s *SQLiteStore) GetPRActivity(login string, since time.Time) ([]models.PRActivity, error) {
	rows, err := s.db.Query(
		`SELECT github_login, pr_number, repo_full_name, state, keyword_flagged, closed_at, cached_at
		 FROM pr_activity_cache
		 WHERE github_login = ? AND closed_at >= ?
		 ORDER BY closed_at DESC`,
		login,
		since.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying pr activity: %w", err)
	}
	defer rows.Close()

	var activities []models.PRActivity
	for rows.Next() {
		var a models.PRActivity
		var closedAt, cachedAt sql.NullString
		err := rows.Scan(&a.GitHubLogin, &a.PRNumber, &a.RepoFullName, &a.State, &a.KeywordFlagged, &closedAt, &cachedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning pr activity: %w", err)
		}
		if closedAt.Valid {
			t, err := time.Parse(time.RFC3339, closedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parsing closed_at: %w", err)
			}
			a.ClosedAt = &t
		}
		if cachedAt.Valid {
			t, err := time.Parse(time.RFC3339, cachedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parsing cached_at: %w", err)
			}
			a.CachedAt = t
		}
		activities = append(activities, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pr activity rows: %w", err)
	}

	return activities, nil
}

// SetPRActivity stores PR activity records, upserting on (login, repo, pr_number).
func (s *SQLiteStore) SetPRActivity(activities []models.PRActivity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO pr_activity_cache (github_login, pr_number, repo_full_name, state, keyword_flagged, closed_at, cached_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(github_login, repo_full_name, pr_number) DO UPDATE SET
			state = excluded.state,
			keyword_flagged = excluded.keyword_flagged,
			closed_at = excluded.closed_at,
			cached_at = excluded.cached_at`,
	)
	if err != nil {
		return fmt.Errorf("preparing statement: %w", err)
	}
	defer stmt.Close()

	for _, a := range activities {
		var closedAt *string
		if a.ClosedAt != nil {
			s := a.ClosedAt.Format(time.RFC3339)
			closedAt = &s
		}
		_, err := stmt.Exec(
			a.GitHubLogin,
			a.PRNumber,
			a.RepoFullName,
			a.State,
			a.KeywordFlagged,
			closedAt,
			a.CachedAt.Format(time.RFC3339),
		)
		if err != nil {
			return fmt.Errorf("inserting pr activity: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// GetCooldown retrieves the cooldown state for a user.
func (s *SQLiteStore) GetCooldown(login string) (*models.Cooldown, error) {
	row := s.db.QueryRow(
		`SELECT github_login, current_level, cooldown_until, last_triggered_at, history
		 FROM cooldowns WHERE github_login = ?`,
		login,
	)

	var c models.Cooldown
	var cooldownUntil, lastTriggeredAt sql.NullString
	var historyJSON string

	err := row.Scan(&c.GitHubLogin, &c.CurrentLevel, &cooldownUntil, &lastTriggeredAt, &historyJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning cooldown: %w", err)
	}

	if cooldownUntil.Valid {
		t, err := time.Parse(time.RFC3339, cooldownUntil.String)
		if err != nil {
			return nil, fmt.Errorf("parsing cooldown_until: %w", err)
		}
		c.CooldownUntil = &t
	}
	if lastTriggeredAt.Valid {
		t, err := time.Parse(time.RFC3339, lastTriggeredAt.String)
		if err != nil {
			return nil, fmt.Errorf("parsing last_triggered_at: %w", err)
		}
		c.LastTriggeredAt = &t
	}

	if err := json.Unmarshal([]byte(historyJSON), &c.History); err != nil {
		return nil, fmt.Errorf("parsing history JSON: %w", err)
	}

	return &c, nil
}

// SetCooldown stores or updates cooldown state for a user.
func (s *SQLiteStore) SetCooldown(cooldown *models.Cooldown) error {
	historyJSON, err := json.Marshal(cooldown.History)
	if err != nil {
		return fmt.Errorf("marshaling history: %w", err)
	}

	var cooldownUntil *string
	if cooldown.CooldownUntil != nil {
		s := cooldown.CooldownUntil.Format(time.RFC3339)
		cooldownUntil = &s
	}

	var lastTriggeredAt *string
	if cooldown.LastTriggeredAt != nil {
		s := cooldown.LastTriggeredAt.Format(time.RFC3339)
		lastTriggeredAt = &s
	}

	_, err = s.db.Exec(
		`INSERT INTO cooldowns (github_login, current_level, cooldown_until, last_triggered_at, history)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(github_login) DO UPDATE SET
			current_level = excluded.current_level,
			cooldown_until = excluded.cooldown_until,
			last_triggered_at = excluded.last_triggered_at,
			history = excluded.history`,
		cooldown.GitHubLogin,
		cooldown.CurrentLevel,
		cooldownUntil,
		lastTriggeredAt,
		string(historyJSON),
	)
	if err != nil {
		return fmt.Errorf("upserting cooldown: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
