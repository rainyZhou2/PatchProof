package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/security/auth"
)

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Instance  string `json:"instance,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	document := problem{
		Type:      "about:blank",
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		Code:      code,
		RequestID: requestIDFrom(r.Context()),
		Instance:  r.URL.Path,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(document)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *domain.ValidationError
	var notFound *domain.NotFoundError
	var conflict *domain.ConflictError
	var version *domain.VersionConflictError
	var idempotency *domain.IdempotencyConflictError
	var transition *domain.StateTransitionError
	var lease *domain.LeaseError
	var fencing *domain.FencingError
	var policyErr *domain.PolicyError
	var tokenErr *auth.TokenError
	switch {
	case errors.As(err, &tokenErr):
		writeProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", tokenErr.Error())
	case errors.As(err, &validation):
		writeProblem(w, r, http.StatusBadRequest, "INVALID_REQUEST", validation.Error())
	case errors.As(err, &notFound), errors.Is(err, domain.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "the requested resource is not visible to this principal")
	case errors.As(err, &idempotency):
		writeProblem(w, r, http.StatusConflict, "IDEMPOTENCY_CONFLICT", idempotency.Error())
	case errors.As(err, &version), errors.As(err, &conflict), errors.As(err, &transition), errors.As(err, &lease), errors.As(err, &fencing):
		writeProblem(w, r, http.StatusConflict, "CONFLICT", err.Error())
	case errors.As(err, &policyErr):
		writeProblem(w, r, http.StatusForbidden, "POLICY_REJECTED", policyErr.Error())
	case errors.Is(err, domain.ErrQuotaExceeded):
		writeProblem(w, r, http.StatusTooManyRequests, "QUOTA_EXCEEDED", "tenant quota is exhausted")
	case errors.Is(err, domain.ErrUnsupported):
		writeProblem(w, r, http.StatusUnprocessableEntity, "UNSUPPORTED", err.Error())
	case errors.Is(err, domain.ErrEvidenceInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "INVALID_EVIDENCE", err.Error())
	case errors.Is(err, domain.ErrRequestCancelled):
		writeProblem(w, r, http.StatusRequestTimeout, "CANCELLED", "request was cancelled")
	default:
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}
