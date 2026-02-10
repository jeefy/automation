package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	gh "github.com/google/go-github/v68/github"
)

func setupTestServer(t *testing.T, mux *http.ServeMux) (*GitHubClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := gh.NewClient(nil)
	baseURL, _ := url.Parse(server.URL + "/")
	client.BaseURL = baseURL
	return NewClient(client), server
}

func TestValidateToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.User{
			Login: gh.Ptr("testuser"),
		})
	})

	client, _ := setupTestServer(t, mux)

	login, err := client.ValidateToken(context.Background())
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if login != "testuser" {
		t.Errorf("login = %q, want %q", login, "testuser")
	}
}

func TestValidateToken_Invalid(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(gh.ErrorResponse{
			Message: "Bad credentials",
		})
	})

	client, _ := setupTestServer(t, mux)

	_, err := client.ValidateToken(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestGetUser(t *testing.T) {
	created := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/testuser", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gh.User{
			Login:     gh.Ptr("testuser"),
			CreatedAt: &gh.Timestamp{Time: created},
		})
	})

	client, _ := setupTestServer(t, mux)

	user, err := client.GetUser(context.Background(), "testuser")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.GitHubLogin != "testuser" {
		t.Errorf("login = %q, want %q", user.GitHubLogin, "testuser")
	}
	if !user.AccountCreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", user.AccountCreatedAt, created)
	}
}

func TestGetClosedPRs(t *testing.T) {
	closedAt := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		result := gh.IssuesSearchResult{
			Total:             gh.Ptr(1),
			IncompleteResults: gh.Ptr(false),
			Issues: []*gh.Issue{
				{
					Number:        gh.Ptr(42),
					RepositoryURL: gh.Ptr("https://api.github.com/repos/cncf/automation"),
					ClosedAt:      &gh.Timestamp{Time: closedAt},
				},
			},
		}
		json.NewEncoder(w).Encode(result)
	})

	client, _ := setupTestServer(t, mux)

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	activities, err := client.GetClosedPRs(context.Background(), "spammer", since)
	if err != nil {
		t.Fatalf("GetClosedPRs: %v", err)
	}
	if len(activities) != 1 {
		t.Fatalf("got %d activities, want 1", len(activities))
	}
	if activities[0].PRNumber != 42 {
		t.Errorf("pr_number = %d, want 42", activities[0].PRNumber)
	}
	if activities[0].RepoFullName != "cncf/automation" {
		t.Errorf("repo = %q, want %q", activities[0].RepoFullName, "cncf/automation")
	}
	if activities[0].ClosedAt == nil || !activities[0].ClosedAt.Equal(closedAt) {
		t.Errorf("closed_at = %v, want %v", activities[0].ClosedAt, closedAt)
	}
}

func TestCheckKeywordsInComments_Found(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/cncf/automation/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []*gh.IssueComment{
			{
				User: &gh.User{Login: gh.Ptr("maintainer")},
				Body: gh.Ptr("This is spam, closing."),
			},
		}
		json.NewEncoder(w).Encode(comments)
	})

	client, _ := setupTestServer(t, mux)

	found, err := client.CheckKeywordsInComments(
		context.Background(), "cncf/automation", 42, "spammer", []string{"spam", "slop"},
	)
	if err != nil {
		t.Fatalf("CheckKeywordsInComments: %v", err)
	}
	if !found {
		t.Error("expected keyword match, got none")
	}
}

func TestCheckKeywordsInComments_AuthorIgnored(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/cncf/automation/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []*gh.IssueComment{
			{
				// Comment from the PR author mentioning "spam" should be ignored
				User: &gh.User{Login: gh.Ptr("spammer")},
				Body: gh.Ptr("This is not spam I swear"),
			},
		}
		json.NewEncoder(w).Encode(comments)
	})

	client, _ := setupTestServer(t, mux)

	found, err := client.CheckKeywordsInComments(
		context.Background(), "cncf/automation", 42, "spammer", []string{"spam"},
	)
	if err != nil {
		t.Fatalf("CheckKeywordsInComments: %v", err)
	}
	if found {
		t.Error("should not match keywords in author's own comments")
	}
}

func TestCheckKeywordsInComments_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/cncf/automation/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []*gh.IssueComment{
			{
				User: &gh.User{Login: gh.Ptr("maintainer")},
				Body: gh.Ptr("Closing because this is a duplicate"),
			},
		}
		json.NewEncoder(w).Encode(comments)
	})

	client, _ := setupTestServer(t, mux)

	found, err := client.CheckKeywordsInComments(
		context.Background(), "cncf/automation", 42, "author", []string{"spam", "slop"},
	)
	if err != nil {
		t.Fatalf("CheckKeywordsInComments: %v", err)
	}
	if found {
		t.Error("should not find keywords that aren't present")
	}
}

func TestCheckKeywordsInComments_CaseInsensitive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/cncf/automation/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []*gh.IssueComment{
			{
				User: &gh.User{Login: gh.Ptr("maintainer")},
				Body: gh.Ptr("This looks like AI SLOP to me"),
			},
		}
		json.NewEncoder(w).Encode(comments)
	})

	client, _ := setupTestServer(t, mux)

	found, err := client.CheckKeywordsInComments(
		context.Background(), "cncf/automation", 42, "author", []string{"ai slop"},
	)
	if err != nil {
		t.Fatalf("CheckKeywordsInComments: %v", err)
	}
	if !found {
		t.Error("keyword matching should be case-insensitive")
	}
}

func TestGetUser_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/baduser", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(gh.ErrorResponse{Message: "Not Found"})
	})

	client, _ := setupTestServer(t, mux)

	_, err := client.GetUser(context.Background(), "baduser")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
}

func TestGetClosedPRs_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(gh.ErrorResponse{Message: "Validation failed"})
	})

	client, _ := setupTestServer(t, mux)

	_, err := client.GetClosedPRs(context.Background(), "user", time.Now())
	if err == nil {
		t.Fatal("expected error for failed search")
	}
}

func TestGetClosedPRs_NilClosedAt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		result := gh.IssuesSearchResult{
			Total:             gh.Ptr(1),
			IncompleteResults: gh.Ptr(false),
			Issues: []*gh.Issue{
				{
					Number:        gh.Ptr(99),
					RepositoryURL: gh.Ptr("https://api.github.com/repos/org/repo"),
					ClosedAt:      nil, // no closed_at
				},
			},
		}
		json.NewEncoder(w).Encode(result)
	})

	client, _ := setupTestServer(t, mux)

	activities, err := client.GetClosedPRs(context.Background(), "user", time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("GetClosedPRs: %v", err)
	}
	if len(activities) != 1 {
		t.Fatalf("got %d activities, want 1", len(activities))
	}
	if activities[0].ClosedAt != nil {
		t.Errorf("expected nil ClosedAt, got %v", activities[0].ClosedAt)
	}
}

func TestCheckKeywordsInComments_InvalidRepo(t *testing.T) {
	mux := http.NewServeMux()
	client, _ := setupTestServer(t, mux)

	_, err := client.CheckKeywordsInComments(
		context.Background(), "invalid-no-slash", 1, "author", []string{"spam"},
	)
	if err == nil {
		t.Fatal("expected error for invalid repo name")
	}
}

func TestCheckKeywordsInComments_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(gh.ErrorResponse{Message: "Internal error"})
	})

	client, _ := setupTestServer(t, mux)

	_, err := client.CheckKeywordsInComments(
		context.Background(), "org/repo", 1, "author", []string{"spam"},
	)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
}

func TestGetClosedPRs_EmptyResult(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		result := gh.IssuesSearchResult{
			Total:             gh.Ptr(0),
			IncompleteResults: gh.Ptr(false),
			Issues:            []*gh.Issue{},
		}
		json.NewEncoder(w).Encode(result)
	})

	client, _ := setupTestServer(t, mux)

	activities, err := client.GetClosedPRs(context.Background(), "clean-user", time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("GetClosedPRs: %v", err)
	}
	if len(activities) != 0 {
		t.Errorf("got %d activities, want 0", len(activities))
	}
}

func TestCheckKeywordsInComments_NoComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*gh.IssueComment{})
	})

	client, _ := setupTestServer(t, mux)

	found, err := client.CheckKeywordsInComments(
		context.Background(), "org/repo", 1, "author", []string{"spam"},
	)
	if err != nil {
		t.Fatalf("CheckKeywordsInComments: %v", err)
	}
	if found {
		t.Error("should not find keywords when no comments exist")
	}
}

func TestExtractRepoFullName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://api.github.com/repos/cncf/automation", "cncf/automation"},
		{"https://api.github.com/repos/org/repo-name", "org/repo-name"},
		{"something-unexpected", "something-unexpected"},
	}
	for _, tt := range tests {
		got := extractRepoFullName(tt.input)
		if got != tt.want {
			t.Errorf("extractRepoFullName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
