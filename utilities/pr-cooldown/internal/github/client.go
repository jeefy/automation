package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
	gh "github.com/google/go-github/v68/github"
)

// Client defines the interface for GitHub API operations needed by the evaluator.
type Client interface {
	// GetUser fetches a GitHub user's profile information.
	GetUser(ctx context.Context, login string) (*models.UserCache, error)

	// GetClosedPRs fetches closed-unmerged PRs authored by the user globally,
	// within the given lookback window.
	GetClosedPRs(ctx context.Context, login string, since time.Time) ([]models.PRActivity, error)

	// CheckKeywordsInComments checks whether any non-author comment on a PR
	// contains any of the given keywords. Returns true if a match is found.
	CheckKeywordsInComments(ctx context.Context, repoFullName string, prNumber int, prAuthor string, keywords []string) (bool, error)

	// ValidateToken checks that the provided token is valid by fetching the
	// authenticated user. Returns the login name or an error.
	ValidateToken(ctx context.Context) (string, error)
}

// GitHubClient implements Client using the go-github library.
type GitHubClient struct {
	client *gh.Client
}

// NewClient creates a new GitHubClient with the given go-github client.
func NewClient(client *gh.Client) *GitHubClient {
	return &GitHubClient{client: client}
}

// ValidateToken checks that the token is valid by fetching the authenticated user.
func (g *GitHubClient) ValidateToken(ctx context.Context) (string, error) {
	user, _, err := g.client.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("validating token: %w", err)
	}
	return user.GetLogin(), nil
}

// GetUser fetches a GitHub user's profile.
func (g *GitHubClient) GetUser(ctx context.Context, login string) (*models.UserCache, error) {
	user, _, err := g.client.Users.Get(ctx, login)
	if err != nil {
		return nil, fmt.Errorf("fetching user %s: %w", login, err)
	}

	return &models.UserCache{
		GitHubLogin:      user.GetLogin(),
		AccountCreatedAt: user.GetCreatedAt().Time,
		CachedAt:         time.Now().UTC(),
	}, nil
}

// GetClosedPRs searches for closed-unmerged PRs authored by the user globally.
func (g *GitHubClient) GetClosedPRs(ctx context.Context, login string, since time.Time) ([]models.PRActivity, error) {
	query := fmt.Sprintf("type:pr author:%s is:closed is:unmerged closed:>=%s",
		login, since.Format("2006-01-02"))

	var allActivities []models.PRActivity
	opts := &gh.SearchOptions{
		Sort:  "updated",
		Order: "desc",
		ListOptions: gh.ListOptions{
			PerPage: 100,
		},
	}

	for {
		result, resp, err := g.client.Search.Issues(ctx, query, opts)
		if err != nil {
			return nil, fmt.Errorf("searching closed PRs for %s: %w", login, err)
		}

		for _, issue := range result.Issues {
			repoURL := issue.GetRepositoryURL()
			repoFullName := extractRepoFullName(repoURL)

			activity := models.PRActivity{
				GitHubLogin:  login,
				PRNumber:     issue.GetNumber(),
				RepoFullName: repoFullName,
				State:        "closed",
				CachedAt:     time.Now().UTC(),
			}

			if issue.ClosedAt != nil {
				closedAt := issue.GetClosedAt().Time
				activity.ClosedAt = &closedAt
			}

			allActivities = append(allActivities, activity)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allActivities, nil
}

// CheckKeywordsInComments checks non-author comments on a PR for keyword matches.
func (g *GitHubClient) CheckKeywordsInComments(ctx context.Context, repoFullName string, prNumber int, prAuthor string, keywords []string) (bool, error) {
	parts := strings.SplitN(repoFullName, "/", 2)
	if len(parts) != 2 {
		return false, fmt.Errorf("invalid repo name: %s", repoFullName)
	}
	owner, repo := parts[0], parts[1]

	opts := &gh.IssueListCommentsOptions{
		Sort:      gh.String("created"),
		Direction: gh.String("desc"),
		ListOptions: gh.ListOptions{
			PerPage: 100,
		},
	}

	for {
		comments, resp, err := g.client.Issues.ListComments(ctx, owner, repo, prNumber, opts)
		if err != nil {
			return false, fmt.Errorf("listing comments for %s#%d: %w", repoFullName, prNumber, err)
		}

		for _, comment := range comments {
			// Skip comments from the PR author
			if comment.GetUser().GetLogin() == prAuthor {
				continue
			}

			body := strings.ToLower(comment.GetBody())
			for _, keyword := range keywords {
				if strings.Contains(body, strings.ToLower(keyword)) {
					return true, nil
				}
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return false, nil
}

// extractRepoFullName extracts "owner/repo" from a GitHub API repository URL.
// e.g., "https://api.github.com/repos/cncf/automation" -> "cncf/automation"
func extractRepoFullName(repoURL string) string {
	const prefix = "/repos/"
	idx := strings.Index(repoURL, prefix)
	if idx == -1 {
		return repoURL
	}
	return repoURL[idx+len(prefix):]
}
