package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/cncf/automation/utilities/pr-cooldown/internal/evaluator"
	ghlib "github.com/cncf/automation/utilities/pr-cooldown/internal/github"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/models"
	"github.com/cncf/automation/utilities/pr-cooldown/internal/store"
	gh "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

// Handler holds the HTTP handlers for the PR cooldown API.
type Handler struct {
	store    store.Store
	cacheTTL time.Duration
}

// NewHandler creates a new Handler.
func NewHandler(s store.Store, cacheTTL time.Duration) *Handler {
	return &Handler{
		store:    s,
		cacheTTL: cacheTTL,
	}
}

// HealthCheck handles GET /health.
func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Check handles POST /check.
func (h *Handler) Check(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req models.CheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if err := validateRequest(&req); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	// Create a GitHub client using the token from the request
	token := TokenFromContext(r.Context())
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(r.Context(), ts)
	ghClient := ghlib.NewClient(gh.NewClient(httpClient))

	eval := evaluator.New(h.store, ghClient, h.cacheTTL)
	resp, err := eval.Check(r.Context(), req)
	if err != nil {
		log.Printf("ERROR: evaluator check for %s: %v", req.PRAuthor, err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func validateRequest(req *models.CheckRequest) error {
	if req.PRAuthor == "" {
		return errMissing("pr_author")
	}
	if req.Repo == "" {
		return errMissing("repo")
	}
	if req.PRNumber <= 0 {
		return errMissing("pr_number")
	}
	if req.LookbackDays <= 0 {
		req.LookbackDays = 30
	}
	if len(req.EscalationTiers) == 0 {
		req.EscalationTiers = []int{3, 7, 21}
	}
	if req.Thresholds == nil {
		req.Thresholds = map[models.AccountAgeTier]models.Thresholds{
			models.TierNew:         {KeywordFlagged: 1, PlainClosed: 2},
			models.TierEstablished: {KeywordFlagged: 2, PlainClosed: 3},
			models.TierVeteran:     {KeywordFlagged: 2, PlainClosed: 4},
		}
	}
	return nil
}

type validationError struct {
	field string
}

func (e *validationError) Error() string {
	return "missing required field: " + e.field
}

func errMissing(field string) error {
	return &validationError{field: field}
}
