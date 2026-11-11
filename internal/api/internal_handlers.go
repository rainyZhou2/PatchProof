package api

import (
	"net/http"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type attemptEnvelope struct {
	Generation int64             `json:"generation"`
	StartedAt  *time.Time        `json:"started_at,omitempty"`
	Result     domain.StepResult `json:"result"`
	ErrorClass string            `json:"error_class"`
	Message    string            `json:"message"`
	Retryable  bool              `json:"retryable"`
}

func (s *Server) handleAttemptStart(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.verifyAttemptToken(w, r)
	if !ok {
		return
	}
	var body attemptEnvelope
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Generation != 0 && body.Generation != ref.Generation {
		writeProblem(w, r, http.StatusConflict, "STALE_GENERATION", "generation does not match the attempt token")
		return
	}
	if err := s.deps.Attempts.Start(r.Context(), ref); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAttemptHeartbeat(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.verifyAttemptToken(w, r)
	if !ok {
		return
	}
	if err := s.deps.Attempts.Heartbeat(r.Context(), ref); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAttemptComplete(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.verifyAttemptToken(w, r)
	if !ok {
		return
	}
	var body attemptEnvelope
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Generation != ref.Generation {
		writeProblem(w, r, http.StatusConflict, "STALE_GENERATION", "generation does not match the attempt token")
		return
	}
	outcome, err := s.deps.Attempts.Complete(r.Context(), ref, body.Result)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attempt_id": ref.AttemptID, "step_id": outcome.Step.ID, "status": outcome.Step.Status,
		"generation": outcome.Step.Generation, "all_terminal": outcome.AllTerminal,
	})
}

func (s *Server) handleAttemptFail(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.verifyAttemptToken(w, r)
	if !ok {
		return
	}
	var body attemptEnvelope
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Generation != ref.Generation {
		writeProblem(w, r, http.StatusConflict, "STALE_GENERATION", "generation does not match the attempt token")
		return
	}
	outcome, err := s.deps.Attempts.Fail(r.Context(), ref, domain.ErrorClass(body.ErrorClass), body.Message, body.Retryable)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attempt_id": ref.AttemptID, "step_id": outcome.Step.ID, "status": outcome.Step.Status,
		"generation": outcome.Step.Generation,
	})
}

func (s *Server) verifyAttemptToken(w http.ResponseWriter, r *http.Request) (domain.AttemptRef, bool) {
	header := r.Header.Get("Authorization")
	if len(header) < 8 || header[:7] != "Bearer " {
		writeProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "attempt token is required")
		return domain.AttemptRef{}, false
	}
	ref, err := s.deps.Tokens.Verify(r.Context(), header[7:])
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "attempt token is invalid or expired")
		return domain.AttemptRef{}, false
	}
	attemptID, err := domain.ParseAttemptID(r.PathValue("attempt_id"))
	if err != nil || attemptID != ref.AttemptID {
		writeProblem(w, r, http.StatusForbidden, "ATTEMPT_MISMATCH", "attempt token does not match the path")
		return domain.AttemptRef{}, false
	}
	return ref, true
}
