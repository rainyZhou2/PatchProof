package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return &domain.ValidationError{Field: "content_type", Reason: "must be application/json"}
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return &domain.ValidationError{Field: "body", Reason: "must not be empty"}
		}
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return &domain.ValidationError{Field: "body", Reason: "exceeds the request size limit"}
		}
		return &domain.ValidationError{Field: "body", Reason: "must be valid JSON matching the schema"}
	}
	if decoder.More() {
		return &domain.ValidationError{Field: "body", Reason: "must contain a single JSON object"}
	}
	return nil
}

func parseRunID(w http.ResponseWriter, r *http.Request) (domain.RunID, bool) {
	runID, err := domain.ParseRunID(r.PathValue("run_id"))
	if err != nil {
		writeError(w, r, err)
		return "", false
	}
	return runID, true
}

func parseProjectID(value string) (domain.ProjectID, error) {
	return domain.ParseProjectID(value)
}

func parseLimit(raw string, fallback, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, &domain.ValidationError{Field: "limit", Reason: "must be a positive integer"}
	}
	if max > 0 && value > max {
		value = max
	}
	return value, nil
}

func requireRoles(w http.ResponseWriter, r *http.Request, roles ...string) (string, bool) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication is required")
		return "", false
	}
	if !hasRole(principal.Roles, roles...) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "principal lacks the required role")
		return "", false
	}
	return principal.Subject, true
}

func hasRole(roles []string, allowed ...string) bool {
	for _, role := range roles {
		for _, candidate := range allowed {
			if strings.EqualFold(role, candidate) {
				return true
			}
		}
	}
	return false
}
