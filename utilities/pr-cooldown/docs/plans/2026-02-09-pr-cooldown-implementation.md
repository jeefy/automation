# PR Cooldown Service Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a Go HTTP service and GitHub Action that detects and throttles spammy PR submitters using cached GitHub API data and configurable escalating cooldowns.

**Architecture:** Standalone Go HTTP service with SQLite storage behind an interface, a core evaluator with no I/O dependencies, and a thin GitHub Action shell client. The service receives policy config per-request so each repo controls its own rules.

**Tech Stack:** Go 1.22+, `net/http` stdlib router, `modernc.org/sqlite` (pure-Go SQLite), `google/go-github/v68` for GitHub API, `stretchr/testify` for tests.

---

### Task 1: Initialize Go Module and Project Skeleton

**Files:**
- Create: `go.mod`
- Create: `cmd/server/main.go`
- Create: `internal/models/models.go`

**Step 1:** Initialize go module

Run: `go mod init github.com/cncf/automation/utilities/pr-cooldown`

**Step 2:** Create `internal/models/models.go` with all shared types:

```go
package models

import "time"

type AccountAgeTier string

const (
	TierNew         AccountAgeTier = "new"
	TierEstablished AccountAgeTier = "established"
	TierVeteran     AccountAgeTier = "veteran"
)

type Verdict string

const (
	VerdictAllow    Verdict = "allow"
	VerdictCooldown Verdict = "cooldown"
)

type Thresholds struct {
	KeywordFlagged int `json:"keyword_flagged"`
	PlainClosed    int `json:"plain_closed"`
}

type CheckRequest struct {
	Repo            string                        `json:"repo"`
	PRNumber        int                           `json:"pr_number"`
	PRAuthor        string                        `json:"pr_author"`
	LookbackDays    int                           `json:"lookback_days"`
	EscalationTiers []int                         `json:"escalation_tiers"`
	Keywords        []string                      `json:"keywords"`
	Thresholds      map[AccountAgeTier]Thresholds `json:"thresholds"`
}

type CheckResponse struct {
	Verdict             Verdict        `json:"verdict"`
	Reason              string         `json:"reason,omitempty"`
	CooldownUntil       *time.Time     `json:"cooldown_until,omitempty"`
	CooldownLevel       *int           `json:"cooldown_level,omitempty"`
	KeywordFlaggedCount int            `json:"keyword_flagged_count"`
	PlainClosedCount    int            `json:"plain_closed_count"`
	AccountAgeTier      AccountAgeTier `json:"account_age_tier"`
}

type UserCache struct {
	GitHubLogin      string    `json:"github_login"`
	AccountCreatedAt time.Time `json:"account_created_at"`
	CachedAt         time.Time `json:"cached_at"`
}

type PRActivity struct {
	GitHubLogin    string     `json:"github_login"`
	PRNumber       int        `json:"pr_number"`
	RepoFullName   string     `json:"repo_full_name"`
	State          string     `json:"state"`
	KeywordFlagged bool       `json:"keyword_flagged"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	CachedAt       time.Time  `json:"cached_at"`
}

type CooldownHistory struct {
	TriggeredAt time.Time `json:"triggered_at"`
	Reason      string    `json:"reason"`
	Level       int       `json:"level"`
}

type Cooldown struct {
	GitHubLogin     string            `json:"github_login"`
	CurrentLevel    int               `json:"current_level"`
	CooldownUntil   *time.Time        `json:"cooldown_until"`
	LastTriggeredAt *time.Time        `json:"last_triggered_at"`
	History         []CooldownHistory `json:"history"`
}
```

**Step 3:** Create minimal `cmd/server/main.go`:

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("pr-cooldown server starting...")
	os.Exit(0)
}
```

**Step 4:** Run `go mod tidy && go build ./cmd/server/` to verify compilation.

---

### Task 2: Store Interface and SQLite Implementation

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/sqlite/sqlite.go`
- Create: `internal/store/sqlite/sqlite_test.go`

**Step 1:** Create `internal/store/store.go` with the Store interface.

**Step 2:** Create `internal/store/sqlite/sqlite.go` implementing the Store interface with table creation, CRUD operations.

**Step 3:** Write tests in `internal/store/sqlite/sqlite_test.go` covering all Store methods.

**Step 4:** Run tests to verify.

---

### Task 3: GitHub Client

**Files:**
- Create: `internal/github/client.go`
- Create: `internal/github/client_test.go`

**Step 1:** Create `internal/github/client.go` wrapping go-github for: user profile fetch, PR search (closed unmerged), PR comment fetching, keyword detection in non-author closing comments.

**Step 2:** Write tests using a mock HTTP server.

**Step 3:** Run tests to verify.

---

### Task 4: Evaluator (Core Logic)

**Files:**
- Create: `internal/evaluator/evaluator.go`
- Create: `internal/evaluator/evaluator_test.go`

**Step 1:** Create `internal/evaluator/evaluator.go` implementing the 5-step evaluation flow. Takes a Store and GitHub client, returns a CheckResponse.

**Step 2:** Write comprehensive tests with mock store and mock GitHub client covering: allow on no history, allow on first PR new account, cooldown on keyword threshold, cooldown on plain threshold, escalation levels, permanent ban, active cooldown short-circuit, all three account age tiers.

**Step 3:** Run tests to verify.

---

### Task 5: HTTP API Handlers

**Files:**
- Create: `internal/api/handler.go`
- Create: `internal/api/middleware.go`
- Create: `internal/api/handler_test.go`

**Step 1:** Create `internal/api/middleware.go` with GitHub token validation middleware.

**Step 2:** Create `internal/api/handler.go` with `/check` and `/health` endpoints.

**Step 3:** Write handler tests.

**Step 4:** Run tests to verify.

---

### Task 6: Wire Up main.go

**Files:**
- Modify: `cmd/server/main.go`

**Step 1:** Wire up flag parsing, SQLite store initialization, HTTP server with handlers and middleware.

**Step 2:** Verify `go build ./cmd/server/` compiles.

---

### Task 7: Dockerfile

**Files:**
- Create: `Dockerfile`

**Step 1:** Multi-stage Dockerfile: build stage with Go, runtime stage with minimal image + SQLite data volume.

---

### Task 8: GitHub Action

**Files:**
- Create: `action/action.yml`
- Create: `action/entrypoint.sh`

**Step 1:** Create `action/action.yml` with all configurable inputs.

**Step 2:** Create `action/entrypoint.sh` that calls the service API and executes the configured response.

---
