package postgres

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// runCursor is the opaque position used by ListRuns. It matches the
// (created_at DESC, run_id DESC) ordering of the query.
type runCursor struct {
	CreatedAt time.Time
	RunID     domain.RunID
}

func encodeRunCursor(createdAt time.Time, runID domain.RunID) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + runID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRunCursor(encoded string) (runCursor, error) {
	if encoded == "" {
		return runCursor{}, &domain.ValidationError{Field: "cursor", Reason: "must not be empty"}
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(encoded)
	}
	if err != nil {
		return runCursor{}, &domain.ValidationError{Field: "cursor", Reason: "must be base64url encoded"}
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 {
		return runCursor{}, &domain.ValidationError{Field: "cursor", Reason: "must contain a timestamp and run id"}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return runCursor{}, &domain.ValidationError{Field: "cursor", Reason: "contains an invalid timestamp"}
	}
	runID, err := domain.ParseRunID(parts[1])
	if err != nil {
		return runCursor{}, &domain.ValidationError{Field: "cursor", Reason: "contains an invalid run id"}
	}
	return runCursor{CreatedAt: createdAt.UTC(), RunID: runID}, nil
}

func clampLimit(limit, fallback, maximum int) int {
	if limit <= 0 {
		limit = fallback
	}
	if limit > maximum {
		limit = maximum
	}
	return limit
}
