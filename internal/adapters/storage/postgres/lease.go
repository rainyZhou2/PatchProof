package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/patchproof/patchproof/internal/domain"
)

// AcquireLease atomically leases a ready step generation and records the
// attempt plus the step.leased outbox event.
func (s *Store) AcquireLease(ctx context.Context, request domain.LeaseRequest) (domain.Lease, error) {
	if err := request.Validate(); err != nil {
		return domain.Lease{}, err
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	deadline := now.Add(request.TTL)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Lease{}, fmt.Errorf("begin acquire lease: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	runStatus, err := lockRunStatus(ctx, tx, request.TenantID, request.RunID)
	if err != nil {
		return domain.Lease{}, err
	}
	if runStatus.Terminal() || runStatus == domain.RunCancelling {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "run is not active"}
	}
	if request.MaxParallelSteps > 0 {
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM steps
			WHERE tenant_id = $1::uuid AND run_id = $2::uuid AND status IN ('LEASED', 'RUNNING')`,
			request.TenantID.String(), request.RunID.String()).Scan(&active); err != nil {
			return domain.Lease{}, mapError(err, "step", request.StepID.String())
		}
		if active >= request.MaxParallelSteps {
			return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "run parallel step budget is exhausted"}
		}
	}
	if request.MaxCPUSeconds > 0 {
		var used float64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(cpu_seconds), 0) FROM step_metrics
			WHERE tenant_id = $1::uuid AND run_id = $2::uuid`,
			request.TenantID.String(), request.RunID.String()).Scan(&used); err != nil {
			return domain.Lease{}, mapError(err, "step", request.StepID.String())
		}
		if used >= request.MaxCPUSeconds {
			return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "run CPU budget is exhausted"}
		}
	}
	var attemptExists bool
	if err := tx.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM attempts WHERE attempt_id = $1::uuid)",
		request.AttemptID.String()).Scan(&attemptExists); err != nil {
		return domain.Lease{}, mapError(err, "attempt", request.AttemptID.String())
	}
	if attemptExists {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "attempt id already exists"}
	}

	step, err := scanStep(tx.QueryRow(ctx, `WITH candidate AS (
		SELECT step_id AS candidate_step_id, created_at AS candidate_created_at
		FROM steps
		WHERE step_id = $1::uuid AND tenant_id = $7::uuid AND run_id = $8::uuid
			AND status = 'READY' AND generation = $2
			AND (next_attempt_at IS NULL OR next_attempt_at <= $6)
		FOR UPDATE SKIP LOCKED
	)
	UPDATE steps SET
		generation = steps.generation + 1,
		status = 'LEASED',
		current_attempt_id = $3::uuid,
		lease_owner = $4,
		lease_deadline = $5,
		attempt_count = steps.attempt_count + 1,
		next_attempt_at = NULL,
		target_cluster = CASE WHEN $9 = '' THEN steps.target_cluster ELSE $9 END,
		updated_at = $6
	FROM candidate
	WHERE steps.step_id = candidate.candidate_step_id
		AND steps.created_at = candidate.candidate_created_at
	RETURNING `+stepColumns,
		request.StepID.String(), request.ExpectedGeneration, request.AttemptID.String(),
		request.Owner, deadline, now, request.TenantID.String(), request.RunID.String(),
		request.ClusterID.String(),
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Lease{}, &domain.LeaseError{
				StepID: request.StepID, Reason: "step is not ready or the generation is stale",
			}
		}
		return domain.Lease{}, mapError(err, "step", request.StepID.String())
	}

	if _, err := tx.Exec(ctx, `INSERT INTO attempts (
		attempt_id, step_id, step_created_at, run_id, tenant_id, generation, status,
		cluster_id, worker_id, executor_ref, result_digest, error_class, error_message,
		created_at, started_at, finished_at
	) VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::uuid, $6, 'ASSIGNED',
		$7, '', '', '', '', '', $8, NULL, NULL)`,
		request.AttemptID.String(), step.ID.String(), step.CreatedAt, step.RunID.String(),
		step.TenantID.String(), step.Generation, request.ClusterID.String(), now,
	); err != nil {
		return domain.Lease{}, mapError(err, "attempt", request.AttemptID.String())
	}
	event, err := stepEvent(domain.EventStepLeased, step, request.AttemptID, now)
	if err != nil {
		return domain.Lease{}, err
	}
	if err := insertOutbox(ctx, tx, []domain.OutboxEvent{event}); err != nil {
		return domain.Lease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Lease{}, fmt.Errorf("commit acquire lease: %w", err)
	}
	finished = true
	return domain.Lease{
		TenantID:            step.TenantID,
		RunID:               step.RunID,
		StepID:              step.ID,
		AttemptID:           request.AttemptID,
		Generation:          step.Generation,
		Owner:               request.Owner,
		ExpiresAt:           deadline,
		Kind:                step.Kind,
		Requirements:        step.Requirements,
		InputManifestDigest: step.InputManifestDigest,
		Priority:            step.Priority,
	}, nil
}

// RenewLease extends a lease held by the requesting owner and generation.
func (s *Store) RenewLease(ctx context.Context, renewal domain.LeaseRenewal) error {
	if err := renewal.Ref.Validate(); err != nil {
		return err
	}
	if len(renewal.Owner) > 256 {
		return &domain.ValidationError{Field: "lease_owner", Reason: "must be at most 256 characters"}
	}
	if renewal.Extension <= 0 {
		return &domain.ValidationError{Field: "extension", Reason: "must be positive"}
	}
	now := normalizeTime(renewal.Now)
	newDeadline := now.Add(renewal.Extension)
	runStatus, err := lookupRunStatus(ctx, s.pool, renewal.Ref.TenantID, renewal.Ref.RunID)
	if err != nil {
		return err
	}
	if runStatus.Terminal() || runStatus == domain.RunCancelling {
		return &domain.LeaseError{StepID: renewal.Ref.StepID, Reason: "run is not active"}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE steps SET
		lease_deadline = GREATEST(lease_deadline, $4), updated_at = $5
		WHERE step_id = $1::uuid AND tenant_id = $2::uuid AND run_id = $3::uuid
			AND status IN ('LEASED', 'RUNNING') AND generation = $6
			AND current_attempt_id = $7::uuid AND (lease_owner = $8 OR $8 = '')
			AND lease_deadline IS NOT NULL AND lease_deadline > $5`,
		renewal.Ref.StepID.String(), renewal.Ref.TenantID.String(), renewal.Ref.RunID.String(),
		newDeadline, now, renewal.Ref.Generation, renewal.Ref.AttemptID.String(), renewal.Owner,
	)
	if err != nil {
		return mapError(err, "step", renewal.Ref.StepID.String())
	}
	if tag.RowsAffected() == 0 {
		return &domain.LeaseError{
			StepID: renewal.Ref.StepID, Reason: "lease is not held by the requesting owner",
		}
	}
	return nil
}

// StartAttempt moves a leased step and its attempt to RUNNING.
func (s *Store) StartAttempt(ctx context.Context, ref domain.AttemptRef, at time.Time) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	at = normalizeTime(at)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin start attempt: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	runStatus, err := lookupRunStatus(ctx, tx, ref.TenantID, ref.RunID)
	if err != nil {
		return err
	}
	if runStatus.Terminal() || runStatus == domain.RunCancelling {
		return &domain.FencingError{StepID: ref.StepID, AttemptID: ref.AttemptID, Reason: "run is not active"}
	}
	step, err := scanStep(tx.QueryRow(ctx, "SELECT "+stepColumns+` FROM steps
		WHERE step_id = $1::uuid AND tenant_id = $2::uuid AND run_id = $3::uuid
		FOR UPDATE`, ref.StepID.String(), ref.TenantID.String(), ref.RunID.String()))
	if err != nil {
		return mapError(err, "step", ref.StepID.String())
	}
	if step.Generation != ref.Generation {
		return &domain.FencingError{StepID: step.ID, AttemptID: ref.AttemptID, Reason: "generation is stale"}
	}
	if step.CurrentAttemptID != ref.AttemptID {
		return &domain.FencingError{StepID: step.ID, AttemptID: ref.AttemptID, Reason: "attempt is not the current attempt"}
	}
	if step.Status != domain.StepLeased {
		return &domain.FencingError{StepID: step.ID, AttemptID: ref.AttemptID, Reason: "step is not leased"}
	}
	tag, err := tx.Exec(ctx, `UPDATE attempts SET
		status = 'RUNNING', started_at = COALESCE(started_at, $2)
		WHERE attempt_id = $1::uuid AND step_id = $3::uuid AND generation = $4
			AND status IN ('ASSIGNED', 'STARTING')`,
		ref.AttemptID.String(), at, ref.StepID.String(), ref.Generation,
	)
	if err != nil {
		return mapError(err, "attempt", ref.AttemptID.String())
	}
	if tag.RowsAffected() == 0 {
		return &domain.FencingError{
			StepID: ref.StepID, AttemptID: ref.AttemptID, Reason: "attempt is not assigned",
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE steps SET status = 'RUNNING',
		started_at = COALESCE(started_at, $2), updated_at = $2
		WHERE step_id = $1::uuid AND created_at = $3`,
		ref.StepID.String(), at, step.CreatedAt,
	); err != nil {
		return mapError(err, "step", ref.StepID.String())
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit start attempt: %w", err)
	}
	finished = true
	return nil
}

// CommitAttempt validates the lease fence, applies the attempt result, and
// promotes dependents when the step succeeds.
func (s *Store) CommitAttempt(ctx context.Context, commit domain.AttemptCommit) (domain.CommitOutcome, error) {
	if err := commit.Validate(); err != nil {
		return domain.CommitOutcome{}, err
	}
	at := normalizeTime(commit.At)
	ref := commit.Ref
	result := commit.Result

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.CommitOutcome{}, fmt.Errorf("begin commit attempt: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	runStatus, err := lockRunStatus(ctx, tx, ref.TenantID, ref.RunID)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	if runStatus.Terminal() || runStatus == domain.RunCancelling {
		return domain.CommitOutcome{}, &domain.FencingError{
			StepID: ref.StepID, AttemptID: ref.AttemptID, Reason: "run is not active",
		}
	}
	step, err := scanStep(tx.QueryRow(ctx, "SELECT "+stepColumns+` FROM steps
		WHERE step_id = $1::uuid AND tenant_id = $2::uuid AND run_id = $3::uuid
		FOR UPDATE`, ref.StepID.String(), ref.TenantID.String(), ref.RunID.String()))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "step", ref.StepID.String())
	}
	attempt, err := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptColumns+` FROM attempts
		WHERE attempt_id = $1::uuid AND step_id = $2::uuid FOR UPDATE`,
		ref.AttemptID.String(), ref.StepID.String()))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "attempt", ref.AttemptID.String())
	}
	if err := domain.ValidateCommitFence(step, attempt, ref, at); err != nil {
		return domain.CommitOutcome{}, err
	}

	nextStatus, delay := domain.NextStepStatus(step, result)
	class := result.ErrorClass
	if nextStatus != domain.StepSucceeded && class == "" {
		switch nextStatus {
		case domain.StepTimedOut:
			class = domain.ErrTimeout
		case domain.StepCancelled:
			class = domain.ErrCancelled
		default:
			class = domain.ErrExecutorError
		}
	}
	attemptStatus := attemptStatusFor(nextStatus, result)
	finishedAt := result.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = at
	} else {
		finishedAt = finishedAt.UTC()
	}

	updatedAttempt, err := scanAttempt(tx.QueryRow(ctx, `UPDATE attempts SET
		status = $2, result_digest = $3, error_class = $4, error_message = $5,
		executor_ref = CASE WHEN $6 = '' THEN executor_ref ELSE $6 END,
		worker_id = CASE WHEN $7 = '' THEN worker_id ELSE $7 END,
		finished_at = $8
		WHERE attempt_id = $1::uuid AND created_at = $9
		RETURNING `+attemptColumns,
		ref.AttemptID.String(), string(attemptStatus), result.ResultDigest, string(class),
		result.Message, result.ExecutorRef, result.WorkerID, finishedAt, attempt.CreatedAt,
	))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "attempt", ref.AttemptID.String())
	}

	var nextAttemptAt any
	var stepFinishedAt any
	if nextStatus == domain.StepRetryWait {
		nextAttemptAt = at.Add(delay)
	}
	if nextStatus.Terminal() {
		stepFinishedAt = finishedAt
	}
	outputManifest, err := result.ResolvedOutputManifest()
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	updatedStep, err := scanStep(tx.QueryRow(ctx, `UPDATE steps SET
		status = $2, result_digest = $3, output_manifest = $4,
		failure_class = $5, failure_message = $6, next_attempt_at = $7,
		lease_owner = '', lease_deadline = NULL, current_attempt_id = NULL,
		started_at = COALESCE(started_at, $8), finished_at = $9, updated_at = $8
		WHERE step_id = $1::uuid AND created_at = $10
		RETURNING `+stepColumns,
		ref.StepID.String(), string(nextStatus), result.ResultDigest, outputManifest,
		string(class), result.Message, nextAttemptAt, at, stepFinishedAt, step.CreatedAt,
	))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "step", ref.StepID.String())
	}

	if err := recordStepMetrics(ctx, tx, updatedStep, result.Metrics, at); err != nil {
		return domain.CommitOutcome{}, err
	}

	outcome := domain.CommitOutcome{
		Step:      updatedStep,
		Attempt:   updatedAttempt,
		RunStatus: runStatus,
	}
	if nextStatus.Terminal() {
		if err := insertCommitEvidence(ctx, tx, updatedStep, result.Evidence, at); err != nil {
			return domain.CommitOutcome{}, err
		}
		if err := insertCounterexamples(ctx, tx, updatedStep, result.Counterexamples, at); err != nil {
			return domain.CommitOutcome{}, err
		}
	}
	if nextStatus == domain.StepSucceeded {
		promoted, err := promoteDependents(ctx, tx, updatedStep, at)
		if err != nil {
			return domain.CommitOutcome{}, err
		}
		outcome.Promoted = promoted
	}

	eventType := domain.EventStepFailed
	switch nextStatus {
	case domain.StepSucceeded:
		eventType = domain.EventStepSucceeded
	case domain.StepRetryWait:
		eventType = domain.EventStepRetry
	}
	event, err := stepEvent(eventType, updatedStep, updatedAttempt.ID, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	events := []domain.OutboxEvent{event}
	if nextStatus.Terminal() && nextStatus != domain.StepSucceeded {
		propagated, propagatedEvents, err := cancelDependentSteps(ctx, tx, updatedStep.TenantID, updatedStep.RunID, []domain.StepID{updatedStep.ID}, at)
		if err != nil {
			return domain.CommitOutcome{}, err
		}
		outcome.Propagated = propagated
		events = append(events, propagatedEvents...)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		return domain.CommitOutcome{}, err
	}

	allTerminal, err := allStepsTerminal(ctx, tx, updatedStep.TenantID, updatedStep.RunID)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	outcome.AllTerminal = allTerminal
	if err := tx.Commit(ctx); err != nil {
		return domain.CommitOutcome{}, fmt.Errorf("commit attempt: %w", err)
	}
	finished = true
	return outcome, nil
}

// FailAttempt terminates or retries an attempt after an executor submission
// failure, using the same fencing and backoff rules as CommitAttempt.
func (s *Store) FailAttempt(ctx context.Context, failure domain.AttemptFailure) (domain.CommitOutcome, error) {
	if err := failure.Validate(); err != nil {
		return domain.CommitOutcome{}, err
	}
	at := normalizeTime(failure.At)
	ref := failure.Ref

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.CommitOutcome{}, fmt.Errorf("begin fail attempt: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	runStatus, err := lockRunStatus(ctx, tx, ref.TenantID, ref.RunID)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	if runStatus.Terminal() || runStatus == domain.RunCancelling {
		return domain.CommitOutcome{}, &domain.FencingError{
			StepID: ref.StepID, AttemptID: ref.AttemptID, Reason: "run is not active",
		}
	}
	step, err := scanStep(tx.QueryRow(ctx, "SELECT "+stepColumns+` FROM steps
		WHERE step_id = $1::uuid AND tenant_id = $2::uuid AND run_id = $3::uuid
		FOR UPDATE`, ref.StepID.String(), ref.TenantID.String(), ref.RunID.String()))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "step", ref.StepID.String())
	}
	attempt, err := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptColumns+` FROM attempts
		WHERE attempt_id = $1::uuid AND step_id = $2::uuid FOR UPDATE`,
		ref.AttemptID.String(), ref.StepID.String()))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "attempt", ref.AttemptID.String())
	}
	if err := domain.ValidateCommitFence(step, attempt, ref, at); err != nil {
		return domain.CommitOutcome{}, err
	}

	outcome := domain.OutcomeFailed
	if failure.Retryable {
		outcome = domain.OutcomeRetryable
	}
	result := domain.StepResult{
		Outcome:    outcome,
		ErrorClass: failure.ErrorClass,
		Message:    failure.Message,
		Retryable:  failure.Retryable,
		FinishedAt: at,
	}
	nextStatus := domain.StepFailed
	var delay time.Duration
	if failure.ErrorClass == domain.ErrCancelled {
		nextStatus = domain.StepCancelled
	} else {
		nextStatus, delay = domain.NextStepStatus(step, result)
	}
	attemptStatus := attemptStatusFor(nextStatus, result)

	updatedAttempt, err := scanAttempt(tx.QueryRow(ctx, `UPDATE attempts SET
		status = $2, error_class = $3, error_message = $4, finished_at = $5
		WHERE attempt_id = $1::uuid AND created_at = $6
		RETURNING `+attemptColumns,
		ref.AttemptID.String(), string(attemptStatus), string(failure.ErrorClass),
		failure.Message, at, attempt.CreatedAt,
	))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "attempt", ref.AttemptID.String())
	}

	var nextAttemptAt any
	var stepFinishedAt any
	if nextStatus == domain.StepRetryWait {
		nextAttemptAt = at.Add(delay)
	}
	if nextStatus.Terminal() {
		stepFinishedAt = at
	}
	updatedStep, err := scanStep(tx.QueryRow(ctx, `UPDATE steps SET
		status = $2, failure_class = $3, failure_message = $4, next_attempt_at = $5,
		lease_owner = '', lease_deadline = NULL, current_attempt_id = NULL,
		started_at = COALESCE(started_at, $6), finished_at = $7, updated_at = $6
		WHERE step_id = $1::uuid AND created_at = $8
		RETURNING `+stepColumns,
		ref.StepID.String(), string(nextStatus), string(failure.ErrorClass), failure.Message,
		nextAttemptAt, at, stepFinishedAt, step.CreatedAt,
	))
	if err != nil {
		return domain.CommitOutcome{}, mapError(err, "step", ref.StepID.String())
	}

	eventType := domain.EventStepFailed
	if nextStatus == domain.StepRetryWait {
		eventType = domain.EventStepRetry
	}
	event, err := stepEvent(eventType, updatedStep, updatedAttempt.ID, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	events := []domain.OutboxEvent{event}
	var propagated []domain.Step
	if nextStatus.Terminal() && nextStatus != domain.StepSucceeded {
		var propagatedEvents []domain.OutboxEvent
		propagated, propagatedEvents, err = cancelDependentSteps(ctx, tx, updatedStep.TenantID, updatedStep.RunID, []domain.StepID{updatedStep.ID}, at)
		if err != nil {
			return domain.CommitOutcome{}, err
		}
		events = append(events, propagatedEvents...)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		return domain.CommitOutcome{}, err
	}
	allTerminal, err := allStepsTerminal(ctx, tx, updatedStep.TenantID, updatedStep.RunID)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.CommitOutcome{}, fmt.Errorf("commit fail attempt: %w", err)
	}
	finished = true
	return domain.CommitOutcome{
		Step:        updatedStep,
		Attempt:     updatedAttempt,
		Propagated:  propagated,
		AllTerminal: allTerminal,
		RunStatus:   runStatus,
	}, nil
}

// ExpireLeases expires overdue leases, retrying the step when attempts remain.
func (s *Store) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]domain.LeaseExpiry, error) {
	limit = clampLimit(limit, 100, 500)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin expire leases: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	rows, err := tx.Query(ctx, "SELECT "+stepColumns+` FROM steps
		WHERE status IN ('LEASED', 'RUNNING') AND lease_deadline IS NOT NULL AND lease_deadline <= $1
		ORDER BY lease_deadline ASC
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, mapError(err, "step", "")
	}
	steps, err := collectSteps(rows)
	if err != nil {
		return nil, err
	}

	expiries := make([]domain.LeaseExpiry, 0, len(steps))
	events := make([]domain.OutboxEvent, 0, len(steps))
	failedByRun := make(map[domain.RunID]struct {
		tenant domain.TenantID
		steps  []domain.StepID
	})
	for _, step := range steps {
		final := step.AttemptCount >= step.MaxAttempts
		attemptID := step.CurrentAttemptID
		if attemptID != "" {
			if _, err := tx.Exec(ctx, `UPDATE attempts SET
				status = 'EXPIRED', error_class = 'LEASE_EXPIRED', error_message = 'lease expired',
				finished_at = $2
				WHERE attempt_id = $1::uuid
					AND status IN ('ASSIGNED', 'STARTING', 'RUNNING', 'UPLOADING')`,
				attemptID.String(), now,
			); err != nil {
				return nil, mapError(err, "attempt", attemptID.String())
			}
		}
		var nextAttemptAt any
		var finishedAt any
		nextStatus := domain.StepRetryWait
		if final {
			nextStatus = domain.StepFailed
			finishedAt = now
		} else {
			nextAttemptAt = now.Add(domain.RetryDelay(step.AttemptCount))
		}
		updated, err := scanStep(tx.QueryRow(ctx, `UPDATE steps SET
			status = $2, failure_class = 'LEASE_EXPIRED', failure_message = 'lease expired',
			next_attempt_at = $3, lease_owner = '', lease_deadline = NULL,
			current_attempt_id = NULL, finished_at = $4, updated_at = $5
			WHERE step_id = $1::uuid AND created_at = $6
			RETURNING `+stepColumns,
			step.ID.String(), string(nextStatus), nextAttemptAt, finishedAt, now, step.CreatedAt,
		))
		if err != nil {
			return nil, mapError(err, "step", step.ID.String())
		}
		eventType := domain.EventStepExpired
		if !final {
			eventType = domain.EventStepRetry
		}
		event, err := stepEvent(eventType, updated, attemptID, now)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
		expiries = append(expiries, domain.LeaseExpiry{
			TenantID:   updated.TenantID,
			RunID:      updated.RunID,
			StepID:     updated.ID,
			AttemptID:  attemptID,
			Generation: updated.Generation,
			ExpiredAt:  now,
			Final:      final,
		})
		if final {
			group := failedByRun[updated.RunID]
			group.tenant = updated.TenantID
			group.steps = append(group.steps, updated.ID)
			failedByRun[updated.RunID] = group
		}
	}
	for runID, group := range failedByRun {
		_, propagatedEvents, err := cancelDependentSteps(ctx, tx, group.tenant, runID, group.steps, now)
		if err != nil {
			return nil, err
		}
		events = append(events, propagatedEvents...)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit expire leases: %w", err)
	}
	finished = true
	return expiries, nil
}

// CancelRunSteps cancels all non-terminal steps and attempts of a run.
func (s *Store) CancelRunSteps(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, at time.Time) (int, error) {
	if err := tenantID.Validate(); err != nil {
		return 0, err
	}
	if err := runID.Validate(); err != nil {
		return 0, err
	}
	at = normalizeTime(at)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin cancel run steps: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	rows, err := tx.Query(ctx, `UPDATE steps SET
		status = 'CANCELLED', failure_class = 'CANCELLED', failure_message = 'run cancelled',
		next_attempt_at = NULL, lease_owner = '', lease_deadline = NULL,
		current_attempt_id = NULL, finished_at = $3, updated_at = $3
		WHERE tenant_id = $1::uuid AND run_id = $2::uuid AND status IN `+nonTerminalStepStatusesSQL+`
		RETURNING `+stepColumns, tenantID.String(), runID.String(), at)
	if err != nil {
		return 0, mapError(err, "step", runID.String())
	}
	steps, err := collectSteps(rows)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE attempts SET
		status = 'CANCELLED', error_class = 'CANCELLED', error_message = 'run cancelled',
		finished_at = $3
		WHERE tenant_id = $1::uuid AND run_id = $2::uuid
			AND status IN ('ASSIGNED', 'STARTING', 'RUNNING', 'UPLOADING')`,
		tenantID.String(), runID.String(), at,
	); err != nil {
		return 0, mapError(err, "attempt", runID.String())
	}
	events := make([]domain.OutboxEvent, 0, len(steps))
	for _, step := range steps {
		event, err := stepEvent(domain.EventStepFailed, step, "", at)
		if err != nil {
			return 0, err
		}
		events = append(events, event)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit cancel run steps: %w", err)
	}
	finished = true
	return len(steps), nil
}

func lookupRunStatus(ctx context.Context, q querier, tenantID domain.TenantID, runID domain.RunID) (domain.RunStatus, error) {
	var status string
	if err := q.QueryRow(ctx, `SELECT status FROM runs
		WHERE run_id = $1::uuid AND tenant_id = $2::uuid`,
		runID.String(), tenantID.String()).Scan(&status); err != nil {
		return "", mapError(err, "run", runID.String())
	}
	return domain.RunStatus(status), nil
}

func lockRunStatus(ctx context.Context, q querier, tenantID domain.TenantID, runID domain.RunID) (domain.RunStatus, error) {
	var status string
	if err := q.QueryRow(ctx, `SELECT status FROM runs
		WHERE run_id = $1::uuid AND tenant_id = $2::uuid FOR UPDATE`,
		runID.String(), tenantID.String()).Scan(&status); err != nil {
		return "", mapError(err, "run", runID.String())
	}
	return domain.RunStatus(status), nil
}

func attemptStatusFor(next domain.StepStatus, result domain.StepResult) domain.AttemptStatus {
	switch next {
	case domain.StepSucceeded:
		return domain.AttemptReported
	case domain.StepCancelled:
		return domain.AttemptCancelled
	case domain.StepRetryWait:
		return domain.AttemptFailedInfra
	}
	if result.Outcome == domain.OutcomeCancelled || result.ErrorClass == domain.ErrCancelled {
		return domain.AttemptCancelled
	}
	return domain.AttemptFailedInfra
}

func promoteDependents(ctx context.Context, tx pgx.Tx, step domain.Step, at time.Time) ([]domain.Step, error) {
	rows, err := tx.Query(ctx, `WITH promotable AS (
		SELECT s.step_id AS candidate_step_id, s.created_at AS candidate_created_at
		FROM steps s
		WHERE s.run_id = $1::uuid AND s.tenant_id = $2::uuid AND s.status = 'BLOCKED'
			AND NOT EXISTS (
				SELECT 1 FROM step_dependencies sd
				JOIN steps dependency
					ON dependency.step_id = sd.depends_on_step_id
					AND dependency.created_at = sd.depends_on_created_at
				WHERE sd.step_id = s.step_id AND dependency.status <> 'SUCCEEDED'
			)
		FOR UPDATE SKIP LOCKED
	)
	UPDATE steps SET status = 'READY', updated_at = $3
	FROM promotable
	WHERE steps.step_id = promotable.candidate_step_id
		AND steps.created_at = promotable.candidate_created_at
	RETURNING `+stepColumns,
		step.RunID.String(), step.TenantID.String(), at)
	if err != nil {
		return nil, mapError(err, "step", step.ID.String())
	}
	promoted, err := collectSteps(rows)
	if err != nil {
		return nil, err
	}
	events := make([]domain.OutboxEvent, 0, len(promoted))
	for _, candidate := range promoted {
		event, err := stepEvent(domain.EventStepReady, candidate, "", at)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		return nil, err
	}
	return promoted, nil
}

func allStepsTerminal(ctx context.Context, q querier, tenantID domain.TenantID, runID domain.RunID) (bool, error) {
	var total, active int
	if err := q.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE status IN `+nonTerminalStepStatusesSQL+`)
		FROM steps WHERE tenant_id = $1::uuid AND run_id = $2::uuid`,
		tenantID.String(), runID.String()).Scan(&total, &active); err != nil {
		return false, mapError(err, "step", runID.String())
	}
	return total > 0 && active == 0, nil
}

func recordStepMetrics(ctx context.Context, q querier, step domain.Step, metrics domain.StepMetrics, at time.Time) error {
	if _, err := q.Exec(ctx, `INSERT INTO step_metrics (
		step_id, run_id, tenant_id, step_kind, cpu_seconds, memory_gib_seconds,
		ephemeral_disk_gib_seconds, egress_bytes, cas_written_bytes, queue_wait_ms,
		execution_ms, upload_ms, created_at
	) VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	ON CONFLICT (step_id) DO UPDATE SET
		step_kind = EXCLUDED.step_kind,
		cpu_seconds = step_metrics.cpu_seconds + EXCLUDED.cpu_seconds,
		memory_gib_seconds = step_metrics.memory_gib_seconds + EXCLUDED.memory_gib_seconds,
		ephemeral_disk_gib_seconds = step_metrics.ephemeral_disk_gib_seconds + EXCLUDED.ephemeral_disk_gib_seconds,
		egress_bytes = step_metrics.egress_bytes + EXCLUDED.egress_bytes,
		cas_written_bytes = step_metrics.cas_written_bytes + EXCLUDED.cas_written_bytes,
		queue_wait_ms = step_metrics.queue_wait_ms + EXCLUDED.queue_wait_ms,
		execution_ms = step_metrics.execution_ms + EXCLUDED.execution_ms,
		upload_ms = step_metrics.upload_ms + EXCLUDED.upload_ms,
		created_at = LEAST(step_metrics.created_at, EXCLUDED.created_at)`,
		step.ID.String(), step.RunID.String(), step.TenantID.String(), string(step.Kind),
		metrics.CPUSeconds, metrics.MemoryGiBSeconds, metrics.EphemeralDiskGiBSeconds,
		metrics.EgressBytes, metrics.CASWrittenBytes, metrics.QueueWaitMS,
		metrics.ExecutionMS, metrics.UploadMS, at,
	); err != nil {
		return mapError(err, "step_metrics", step.ID.String())
	}
	return nil
}

func insertCommitEvidence(ctx context.Context, q querier, step domain.Step, evidence []domain.Evidence, at time.Time) error {
	seen := make(map[domain.EvidenceID]struct{}, len(evidence))
	for i := range evidence {
		document := evidence[i]
		if _, duplicate := seen[document.ID]; duplicate {
			return &domain.ConflictError{Resource: "evidence", ID: string(document.ID), Reason: "duplicate evidence id in result"}
		}
		seen[document.ID] = struct{}{}
		var exists bool
		if err := q.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM evidence_documents WHERE evidence_id = $1::uuid)",
			document.ID.String()).Scan(&exists); err != nil {
			return mapError(err, "evidence", document.ID.String())
		}
		if exists {
			return &domain.ConflictError{Resource: "evidence", ID: string(document.ID), Reason: "already exists"}
		}
	}
	for i := range evidence {
		document := evidence[i]
		switch {
		case document.RunID == "":
			document.RunID = step.RunID
		case document.RunID != step.RunID:
			return &domain.ValidationError{Field: "evidence.run_id", Reason: "must match the committing step"}
		}
		switch {
		case document.TenantID == "":
			document.TenantID = step.TenantID
		case document.TenantID != step.TenantID:
			return &domain.ValidationError{Field: "evidence.tenant_id", Reason: "must match the committing step"}
		}
		if document.StepID == "" {
			document.StepID = step.ID
		} else if document.StepID != step.ID {
			return &domain.ValidationError{Field: "evidence.step_id", Reason: "must match the committing step"}
		}
		if err := document.Validate(); err != nil {
			return err
		}
		createdAt := document.CreatedAt
		if createdAt.IsZero() {
			createdAt = at
		} else {
			createdAt = createdAt.UTC()
		}
		raw, err := encodeJSON(document)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `INSERT INTO evidence_documents (
			evidence_id, tenant_id, run_id, step_id, claim_id, kind, result, document, created_at
		) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9)`,
			document.ID.String(), document.TenantID.String(), document.RunID.String(),
			document.StepID.String(), string(document.ClaimID), string(document.Kind),
			string(document.Result), raw, createdAt,
		); err != nil {
			return mapError(err, "evidence", document.ID.String())
		}
	}
	return nil
}

func insertCounterexamples(ctx context.Context, q querier, step domain.Step, counterexamples []domain.Counterexample, at time.Time) error {
	for _, counterexample := range counterexamples {
		switch {
		case counterexample.RunID == "":
			counterexample.RunID = step.RunID
		case counterexample.RunID != step.RunID:
			return &domain.ValidationError{Field: "counterexample.run_id", Reason: "must match the committing step"}
		}
		switch {
		case counterexample.TenantID == "":
			counterexample.TenantID = step.TenantID
		case counterexample.TenantID != step.TenantID:
			return &domain.ValidationError{Field: "counterexample.tenant_id", Reason: "must match the committing step"}
		}
		if err := counterexample.Validate(); err != nil {
			return err
		}
		createdAt := counterexample.CreatedAt
		if createdAt.IsZero() {
			createdAt = at
		} else {
			createdAt = createdAt.UTC()
		}
		raw, err := encodeJSON(counterexample)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `INSERT INTO counterexamples (
			run_id, tenant_id, case_id, claim_id, document, created_at
		) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
		ON CONFLICT (run_id, case_id) DO NOTHING`,
			counterexample.RunID.String(), counterexample.TenantID.String(), counterexample.CaseID,
			string(counterexample.ClaimID), raw, createdAt,
		); err != nil {
			return mapError(err, "counterexample", counterexample.CaseID)
		}
	}
	return nil
}
