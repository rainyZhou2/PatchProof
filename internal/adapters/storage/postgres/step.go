package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	nonTerminalStepStatusesSQL = "('BLOCKED', 'READY', 'LEASED', 'RUNNING', 'RETRY_WAIT')"
	// Capacity accounting is about work actually occupying an executor, not
	// queued or dependency-blocked steps. Counting READY/RETRY_WAIT here makes
	// a tenant's own backlog consume its concurrency quota and can deadlock the
	// scheduler once the number of queued steps reaches the limit.
	capacityStepStatusesSQL = "('LEASED', 'RUNNING')"
	terminalRunStatusesSQL  = "('COMPLETED', 'PARTIAL', 'INFRA_FAILED', 'CANCELLED', 'REJECTED')"
)

// ListInFlightSteps returns leased/running steps across tenants for scheduler
// restart adoption.
func (s *Store) ListInFlightSteps(ctx context.Context, limit int) ([]domain.Step, error) {
	limit = clampLimit(limit, 500, 2000)
	rows, err := s.pool.Query(ctx, "SELECT "+stepColumns+` FROM steps
		WHERE status IN ('LEASED', 'RUNNING')
		ORDER BY updated_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err, "step", "")
	}
	defer rows.Close()
	return collectSteps(rows)
}

// SetAttemptExecutor persists the executor reference for restart adoption.
func (s *Store) SetAttemptExecutor(ctx context.Context, ref domain.AttemptRef, executorRef string, at time.Time) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := domain.RequireNonEmpty("executor_ref", executorRef); err != nil {
		return err
	}
	at = at.UTC()
	tag, err := s.pool.Exec(ctx, `UPDATE attempts SET executor_ref = $4
		WHERE attempt_id = $1::uuid AND step_id = $2::uuid AND tenant_id = $3::uuid`,
		ref.AttemptID.String(), ref.StepID.String(), ref.TenantID.String(), executorRef)
	if err != nil {
		return mapError(err, "attempt", ref.AttemptID.String())
	}
	if tag.RowsAffected() == 0 {
		return notFound("attempt", ref.AttemptID.String())
	}
	return nil
}

// ListSteps returns every step of a run ordered by creation time.
func (s *Store) ListSteps(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Step, error) {
	if err := tenantID.Validate(); err != nil {
		return nil, err
	}
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+stepColumns+` FROM steps
		WHERE tenant_id = $1::uuid AND run_id = $2::uuid
		ORDER BY created_at, step_id`, tenantID.String(), runID.String())
	if err != nil {
		return nil, mapError(err, "step", runID.String())
	}
	return collectSteps(rows)
}

// ListReadySteps returns leasable steps for a shard in scheduling order.
func (s *Store) ListReadySteps(ctx context.Context, query domain.ReadyQuery) ([]domain.Step, error) {
	limit := clampLimit(query.Limit, 100, 500)
	now := query.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if query.Shard < 0 {
		return nil, &domain.ValidationError{Field: "shard", Reason: "must not be negative"}
	}
	if query.ShardCount < 0 {
		return nil, &domain.ValidationError{Field: "shard_count", Reason: "must not be negative"}
	}
	for _, kind := range query.Kinds {
		if !kind.Valid() {
			return nil, &domain.ValidationError{Field: "kind", Reason: "unknown step kind"}
		}
	}

	args := []any{now}
	conditions := []string{"status = 'READY'", "(next_attempt_at IS NULL OR next_attempt_at <= $1)"}
	if query.ShardCount > 0 {
		args = append(args, query.ShardCount, int(query.Shard))
		conditions = append(conditions,
			fmt.Sprintf("shard %% $%d = $%d", len(args)-1, len(args)))
	} else if query.Shard > 0 {
		args = append(args, int(query.Shard))
		conditions = append(conditions, fmt.Sprintf("shard = $%d", len(args)))
	}
	if query.TenantID != "" {
		if err := query.TenantID.Validate(); err != nil {
			return nil, err
		}
		args = append(args, query.TenantID.String())
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d::uuid", len(args)))
	}
	if query.Region != "" {
		args = append(args, query.Region)
		conditions = append(conditions, fmt.Sprintf(
			"(COALESCE(requirements->>'region', '') = '' OR lower(requirements->>'region') = lower($%d))",
			len(args)))
	}
	if len(query.Kinds) > 0 {
		kinds := make([]string, 0, len(query.Kinds))
		for _, kind := range query.Kinds {
			kinds = append(kinds, string(kind))
		}
		args = append(args, kinds)
		conditions = append(conditions, fmt.Sprintf("kind = ANY($%d)", len(args)))
	}
	args = append(args, limit)
	sql := "SELECT " + stepColumns + " FROM steps WHERE " +
		joinConditions(conditions) +
		fmt.Sprintf(" ORDER BY priority DESC, created_at ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, "step", query.TenantID.String())
	}
	return collectSteps(rows)
}

// PromoteDueSteps moves retry-wait steps whose backoff elapsed back to READY.
func (s *Store) PromoteDueSteps(ctx context.Context, now time.Time, limit int) ([]domain.Step, error) {
	limit = clampLimit(limit, 100, 500)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin promote steps: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT step_id AS candidate_step_id, created_at AS candidate_created_at
		FROM steps
		WHERE status = 'RETRY_WAIT' AND next_attempt_at IS NOT NULL AND next_attempt_at <= $1
		ORDER BY next_attempt_at ASC, created_at ASC
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	)
	UPDATE steps SET status = 'READY', next_attempt_at = NULL, updated_at = $1
	FROM due
	WHERE steps.step_id = due.candidate_step_id AND steps.created_at = due.candidate_created_at
	RETURNING `+stepColumns, now, limit)
	if err != nil {
		return nil, mapError(err, "step", "due")
	}
	steps, err := collectSteps(rows)
	if err != nil {
		return nil, err
	}
	events := make([]domain.OutboxEvent, 0, len(steps))
	for _, step := range steps {
		event, err := stepEvent(domain.EventStepReady, step, "", now)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit promote steps: %w", err)
	}
	finished = true
	return steps, nil
}

// GetStep loads a step scoped to its tenant.
func (s *Store) GetStep(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) (domain.Step, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Step{}, err
	}
	if err := stepID.Validate(); err != nil {
		return domain.Step{}, err
	}
	step, err := scanStep(s.pool.QueryRow(ctx,
		"SELECT "+stepColumns+" FROM steps WHERE step_id = $1::uuid AND tenant_id = $2::uuid",
		stepID.String(), tenantID.String()))
	if err != nil {
		return domain.Step{}, mapError(err, "step", stepID.String())
	}
	return step, nil
}

// ListAttempts returns all attempts of a step in generation order.
func (s *Store) ListAttempts(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) ([]domain.Attempt, error) {
	if err := tenantID.Validate(); err != nil {
		return nil, err
	}
	if err := stepID.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+attemptColumns+` FROM attempts
		WHERE step_id = $1::uuid AND tenant_id = $2::uuid
		ORDER BY generation, created_at`, stepID.String(), tenantID.String())
	if err != nil {
		return nil, mapError(err, "attempt", stepID.String())
	}
	return collectAttempts(rows)
}

// CountActiveSteps counts non-terminal steps for a tenant.
func (s *Store) CountActiveSteps(ctx context.Context, tenantID domain.TenantID) (int, error) {
	if err := tenantID.Validate(); err != nil {
		return 0, err
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM steps
		WHERE tenant_id = $1::uuid AND status IN `+capacityStepStatusesSQL,
		tenantID.String()).Scan(&count); err != nil {
		return 0, mapError(err, "step", tenantID.String())
	}
	return count, nil
}

// TenantUsage returns active run and step counts plus metered CPU usage since
// the supplied timestamp.
func (s *Store) TenantUsage(ctx context.Context, tenantID domain.TenantID, since time.Time) (domain.TenantUsage, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.TenantUsage{}, err
	}
	since = since.UTC()
	var usage domain.TenantUsage
	if err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM runs WHERE tenant_id = $1::uuid AND status NOT IN `+terminalRunStatusesSQL+`),
		(SELECT count(*) FROM steps WHERE tenant_id = $1::uuid AND status IN `+nonTerminalStepStatusesSQL+`),
		COALESCE((SELECT sum(GREATEST(cpu_seconds, execution_ms / 1000.0)) FROM step_metrics
			WHERE tenant_id = $1::uuid AND created_at >= $2), 0),
		COALESCE((SELECT sum(GREATEST(cpu_seconds, execution_ms / 1000.0)) FROM step_metrics
			WHERE tenant_id = $1::uuid AND created_at >= $2 AND step_kind = 'differential-fuzz'), 0),
		COALESCE((SELECT sum(cas_written_bytes) FROM step_metrics
			WHERE tenant_id = $1::uuid), 0)`,
		tenantID.String(), since,
	).Scan(&usage.ActiveRuns, &usage.ActiveSteps, &usage.DailyCPUSeconds, &usage.DailyFuzzSeconds, &usage.CASWrittenBytes); err != nil {
		return domain.TenantUsage{}, mapError(err, "tenant", tenantID.String())
	}
	usage.TenantID = tenantID
	return usage, nil
}

func joinConditions(conditions []string) string {
	out := ""
	for i, condition := range conditions {
		if i > 0 {
			out += " AND "
		}
		out += condition
	}
	return out
}
