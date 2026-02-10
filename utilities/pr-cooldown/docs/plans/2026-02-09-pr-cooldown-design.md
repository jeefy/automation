# PR Cooldown Service — Design Document

**Date:** 2026-02-09

## Overview

A system to detect and throttle spammy GitHub Pull Request submitters. Consists of two components: a Go HTTP service (decision engine + cache) and a thin GitHub Action (trigger + executor).

## Architecture

```
PR opened → Action triggered → Action calls Service API (POST /check)
    → Service checks active cooldown for user
    → Service checks local cache for user profile + PR history
    → Cache miss? Fetches from GitHub API, caches result
    → Evaluates rules against cached data
    → Returns verdict to Action
    → Action executes configured response (close, comment, or both)
```

The service owns all GitHub API interaction (with caching), all cooldown state, and all evaluation logic. The action is purely a trigger and executor.

## Data Model

Three tables in SQLite, behind a `Store` interface for backend swappability.

### `user_cache`

| Column             | Type      | Notes                          |
|--------------------|-----------|--------------------------------|
| `github_login`     | TEXT (PK) |                                |
| `account_created_at` | TIMESTAMP | For age tier calculation      |
| `cached_at`        | TIMESTAMP | When this was fetched          |
| `cache_ttl`        | DURATION  | Configurable, default 24h     |

### `pr_activity_cache`

| Column             | Type      | Notes                                  |
|--------------------|-----------|----------------------------------------|
| `github_login`     | TEXT      |                                        |
| `pr_number`        | INTEGER   |                                        |
| `repo_full_name`   | TEXT      | e.g., `cncf/automation`               |
| `state`            | TEXT      | open, closed, merged                   |
| `keyword_flagged`  | BOOLEAN   | Closing comment from non-author matched a keyword |
| `closed_at`        | TIMESTAMP |                                        |
| `cached_at`        | TIMESTAMP |                                        |

### `cooldowns`

| Column              | Type      | Notes                                    |
|---------------------|-----------|------------------------------------------|
| `github_login`      | TEXT (PK) |                                          |
| `current_level`     | INTEGER   | Index into escalation tiers              |
| `cooldown_until`    | TIMESTAMP | NULL = permanent ban (tier value is 0)   |
| `last_triggered_at` | TIMESTAMP |                                          |
| `history`           | JSON      | Array of past triggers with timestamps/reasons |

### Store Interface

```go
type Store interface {
    GetUserCache(login string) (*UserCache, error)
    SetUserCache(cache *UserCache) error
    GetPRActivity(login string, since time.Time) ([]PRActivity, error)
    SetPRActivity(activity []PRActivity) error
    GetCooldown(login string) (*Cooldown, error)
    SetCooldown(cooldown *Cooldown) error
}
```

SQLite implements this first. Swap in Postgres or another backend by implementing the same interface.

## Evaluation Logic

### Step 1: Check Active Cooldown

If the user has an active cooldown (`cooldown_until` in the future or NULL for permanent), return verdict `cooldown` immediately. No GitHub API calls needed.

### Step 2: Fetch/Refresh User Profile

Check `user_cache` for the user. If stale or missing, fetch from GitHub API and cache. Determine account age tier:

- **New:** < 90 days
- **Established:** 90 days – 2 years
- **Veteran:** 2+ years

### Step 3: Fetch/Refresh PR Activity

Check `pr_activity_cache` for the user's recent PRs within the configurable lookback window (default 30 days). If stale or missing, fetch from GitHub search API (`is:pr author:{login} is:closed is:unmerged`). For each closed-unmerged PR, check closing comments from non-authors for keyword matches and set `keyword_flagged`.

### Step 4: Apply Thresholds

Using the account age tier, look up configured thresholds:

| Tier         | Keyword-flagged threshold | Plain closure threshold |
|--------------|--------------------------|------------------------|
| New (< 90d)  | 1 (default)              | 2 (default)            |
| Established  | 2 (default)              | 3 (default)            |
| Veteran (2y+)| 2 (default)              | 4 (default)            |

If either threshold is met, trigger cooldown. Escalate `current_level` and compute `cooldown_until` from the configured escalation tiers (e.g., `[3, 7, 21, 0]`). A `0` in the tiers means permanent ban. If no `0` is present, the last value repeats.

### Step 5: Return Verdict

- `allow` — no cooldown triggered
- `cooldown` — cooldown triggered or already active

## Service API

### `POST /check`

**Headers:**
- `Authorization: Bearer <github-token>`

**Request:**
```json
{
  "repo": "cncf/automation",
  "pr_number": 42,
  "pr_author": "some-user",
  "lookback_days": 30,
  "escalation_tiers": [3, 7, 21, 0],
  "keywords": ["spam", "ai slop", "slop"],
  "thresholds": {
    "new":         {"keyword_flagged": 1, "plain_closed": 2},
    "established": {"keyword_flagged": 2, "plain_closed": 3},
    "veteran":     {"keyword_flagged": 2, "plain_closed": 4}
  }
}
```

**Response:**
```json
{
  "verdict": "allow|cooldown",
  "reason": "Active cooldown until 2026-02-12T00:00:00Z",
  "cooldown_until": "2026-02-12T00:00:00Z",
  "cooldown_level": 1,
  "keyword_flagged_count": 2,
  "plain_closed_count": 1,
  "account_age_tier": "new"
}
```

For `allow` verdicts, `cooldown_until` and `cooldown_level` are omitted.

### `GET /health`

Returns 200 OK.

### Authentication

The GitHub token in the `Authorization` header serves dual purpose: it authenticates the caller (validated by calling `GET /user`, cached per-token with a short TTL) and is used for GitHub API calls. For local development, a personal access token (PAT) works.

## Service Configuration

Startup flags only — no policy config:

```
--port 8080              # Listen port
--db-path ./cooldown.db  # SQLite database path
--cache-ttl 24h          # Default cache TTL for user/PR data
--token-cache-ttl 5m     # TTL for GitHub token validation cache
```

All policy configuration (thresholds, escalation tiers, keywords, lookback window) comes per-request from the action.

## GitHub Action Configuration

```yaml
name: PR Cooldown Check
on:
  pull_request:
    types: [opened]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: cncf/pr-cooldown-action@v1
        with:
          service_url: "https://pr-cooldown.example.com"
          github_token: ${{ secrets.GITHUB_TOKEN }}

          # Action to take: close, close-comment, comment
          action: "close-comment"

          # Comment template. Supports {duration}, {reason}, {login}
          comment: "Suspected spam, auto-closing. @{login} is in cooldown for {duration}."

          # Label to apply (optional)
          label: "pr-cooldown"

          # Lookback window in days
          lookback_days: 30

          # Escalation tiers in days (0 = permanent)
          escalation_tiers: "3,7,21,0"

          # Spam keywords (stored as a secret to prevent gaming)
          keywords: ${{ secrets.PR_COOLDOWN_KEYWORDS }}

          # Thresholds: keyword_flagged,plain_closed per age tier
          threshold_new: "1,2"
          threshold_established: "2,3"
          threshold_veteran: "2,4"
```

## Project Structure

```
pr-cooldown/
├── cmd/
│   └── server/
│       └── main.go              # Entry point, flag parsing, wiring
├── internal/
│   ├── api/
│   │   ├── handler.go           # HTTP handlers (/check, /health)
│   │   └── middleware.go        # Token validation middleware
│   ├── evaluator/
│   │   └── evaluator.go         # Core decision logic (the 5-step flow)
│   ├── github/
│   │   └── client.go            # GitHub API client (user info, PR search, comments)
│   ├── store/
│   │   ├── store.go             # Store interface definition
│   │   └── sqlite/
│   │       └── sqlite.go        # SQLite implementation
│   └── models/
│       └── models.go            # Shared types (Verdict, UserCache, PRActivity, Cooldown, etc.)
├── action/
│   ├── action.yml               # GitHub Action metadata
│   └── entrypoint.sh            # Shell script that calls the service and acts on verdict
├── Dockerfile
├── go.mod
├── go.sum
└── README.md
```

### Key Design Decisions

- `store.go` defines the interface; `sqlite/` implements it. Adding `postgres/` later is just another implementation.
- `github/client.go` wraps all GitHub API calls. The evaluator never calls GitHub directly — it goes through the store (cache-miss → fetch → cache-store).
- `evaluator/` is pure logic with no HTTP or storage concerns. Takes a store and a request, returns a verdict. Easy to unit test.
- The action is just `action.yml` + a shell script. No compiled code on the action side.
