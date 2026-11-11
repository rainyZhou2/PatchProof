package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/patchproof/patchproof/internal/domain"
)

// cancelDependentSteps cancels every transitively blocked step that can no
// longer run because one of the supplied roots reached a terminal non-success
// state. Steps already cancelled or failed are traversed so recovery is
// idempotent across retries and controller failover.
func cancelDependentSteps(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.RunID, roots []domain.StepID, at time.Time) ([]domain.Step, []domain.OutboxEvent, error) {
	if len(roots) == 0 {
		return nil, nil, nil
	}
	parsed := make([]uuid.UUID, 0, len(roots))
	for _, root := range roots {
		value, err := uuid.Parse(root.String())
		if err != nil {
			return nil, nil, &domain.ValidationError{Field: "step_id", Reason: "must be a UUID"}
		}
		parsed = append(parsed, value)
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE reachable AS (
		SELECT s.step_id, s.created_at, s.status
		FROM step_dependencies sd
		JOIN steps s ON s.step_id = sd.step_id AND s.created_at = sd.step_created_at
		WHERE sd.run_id = $1::uuid AND sd.depends_on_step_id = ANY($2::uuid[])
		UNION
		SELECT s.step_id, s.created_at, s.status
		FROM reachable r
		JOIN step_dependencies sd ON sd.depends_on_step_id = r.step_id
		JOIN steps s ON s.step_id = sd.step_id AND s.created_at = sd.step_created_at
		WHERE sd.run_id = $1::uuid
			AND (r.status = 'BLOCKED' OR r.status IN ('FAILED', 'CANCELLED', 'TIMED_OUT'))
			AND (s.status = 'BLOCKED' OR s.status IN ('FAILED', 'CANCELLED', 'TIMED_OUT'))
	)
	UPDATE steps SET
		status = 'CANCELLED', failure_class = $4, failure_message = $5,
		next_attempt_at = NULL, lease_owner = '', lease_deadline = NULL,
		current_attempt_id = NULL, finished_at = $3, updated_at = $3
	WHERE tenant_id = $6::uuid
		AND (step_id, created_at) IN (SELECT step_id, created_at FROM reachable WHERE status = 'BLOCKED')
	RETURNING `+stepColumns,
		runID.String(), parsed, at, string(domain.ErrDependencyFailed), "a required dependency did not succeed",
		tenantID.String())
	if err != nil {
		return nil, nil, mapError(err, "step", runID.String())
	}
	propagated, err := collectSteps(rows)
	if err != nil {
		return nil, nil, err
	}
	events := make([]domain.OutboxEvent, 0, len(propagated))
	for _, step := range propagated {
		event, err := stepEvent(domain.EventStepFailed, step, "", at)
		if err != nil {
			return nil, nil, err
		}
		events = append(events, event)
	}
	return propagated, events, nil
}
