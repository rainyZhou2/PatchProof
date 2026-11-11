package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/patchproof/patchproof/internal/domain"
)

// CreateRun atomically inserts a run, its idempotency key, and its outbox
// events. A repeated request key with the same fingerprint returns the
// existing run.
func (s *Store) CreateRun(ctx context.Context, record domain.CreateRunRecord) (domain.Run, error) {
	run := record.Run
	if err := run.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := domain.RequireNonEmpty("request_key", run.RequestKey); err != nil {
		return domain.Run{}, err
	}
	if err := domain.RequireMaxLen("request_key", run.RequestKey, 200); err != nil {
		return domain.Run{}, err
	}
	if run.StateVersion < 0 {
		return domain.Run{}, &domain.ValidationError{Field: "state_version", Reason: "must not be negative"}
	}
	run.CreatedAt = normalizeTime(run.CreatedAt)
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	} else {
		run.UpdatedAt = run.UpdatedAt.UTC()
	}
	if run.RequestedChecks == nil {
		run.RequestedChecks = []string{}
	}
	requested, err := encodeJSON(run.RequestedChecks)
	if err != nil {
		return domain.Run{}, err
	}
	budget, err := encodeJSON(run.Budget)
	if err != nil {
		return domain.Run{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, fmt.Errorf("begin create run: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	var reservedRunID string
	err = tx.QueryRow(ctx, `INSERT INTO run_request_keys (
		tenant_id, project_id, request_key, run_id, created_at
	) VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5)
	ON CONFLICT (tenant_id, project_id, request_key) DO NOTHING
	RETURNING run_id::text`,
		run.TenantID.String(), run.ProjectID.String(), run.RequestKey, run.ID.String(), run.CreatedAt,
	).Scan(&reservedRunID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, mapError(err, "run", run.ID.String())
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Rollback(ctx); err != nil {
			return domain.Run{}, fmt.Errorf("rollback duplicate create run: %w", err)
		}
		finished = true
		return s.existingRunForKey(ctx, run)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO runs (
		run_id, tenant_id, project_id, request_key, fingerprint, base_commit, head_commit,
		patch_digest, snapshot_digest, base_snapshot, head_snapshot, policy_digest,
		policy_ref, requested_checks, budget, status, verdict, state_version,
		failure_class, failure_message, region, created_by, created_at, updated_at,
		planned_at, started_at, finished_at
	) VALUES (
		$1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27
	)`,
		run.ID.String(), run.TenantID.String(), run.ProjectID.String(), run.RequestKey,
		run.Fingerprint, run.BaseCommit, run.HeadCommit, run.PatchDigest, run.SnapshotDigest,
		run.BaseSnapshot, run.HeadSnapshot, run.PolicyDigest, run.PolicyRef, requested,
		budget, string(run.Status), string(run.Verdict), run.StateVersion,
		string(run.FailureClass), run.FailureMessage, run.Region, run.CreatedBy,
		run.CreatedAt, run.UpdatedAt, utcPtr(run.PlannedAt), utcPtr(run.StartedAt),
		utcPtr(run.FinishedAt),
	); err != nil {
		return domain.Run{}, mapError(err, "run", run.ID.String())
	}

	if err := insertOutbox(ctx, tx, record.Events); err != nil {
		return domain.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, fmt.Errorf("commit create run: %w", err)
	}
	finished = true
	return run, nil
}

func (s *Store) existingRunForKey(ctx context.Context, run domain.Run) (domain.Run, error) {
	var existingID string
	if err := s.pool.QueryRow(ctx, `SELECT run_id::text FROM run_request_keys
		WHERE tenant_id = $1::uuid AND project_id = $2::uuid AND request_key = $3`,
		run.TenantID.String(), run.ProjectID.String(), run.RequestKey,
	).Scan(&existingID); err != nil {
		return domain.Run{}, mapError(err, "run", run.ID.String())
	}
	existing, err := scanRun(s.pool.QueryRow(ctx,
		"SELECT "+runColumns+" FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid",
		existingID, run.TenantID.String()))
	if err != nil {
		return domain.Run{}, mapError(err, "run", existingID)
	}
	if existing.Fingerprint != run.Fingerprint {
		return domain.Run{}, &domain.IdempotencyConflictError{RequestKey: run.RequestKey}
	}
	return existing, nil
}

// GetRun loads a run scoped to its tenant.
func (s *Store) GetRun(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Run, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := runID.Validate(); err != nil {
		return domain.Run{}, err
	}
	run, err := scanRun(s.pool.QueryRow(ctx,
		"SELECT "+runColumns+" FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid",
		runID.String(), tenantID.String()))
	if err != nil {
		return domain.Run{}, mapError(err, "run", runID.String())
	}
	return run, nil
}

// ListRuns returns a page of runs ordered by creation time descending, using an
// opaque cursor for stable pagination.
func (s *Store) ListRuns(ctx context.Context, filter domain.RunFilter) (domain.RunPage, error) {
	if filter.TenantID != "" {
		if err := filter.TenantID.Validate(); err != nil {
			return domain.RunPage{}, err
		}
	}
	for _, status := range filter.Statuses {
		if !status.Valid() {
			return domain.RunPage{}, &domain.ValidationError{Field: "run.status", Reason: "unknown status"}
		}
	}
	limit := clampLimit(filter.Limit, 50, 500)
	args := make([]any, 0, 8)
	conditions := make([]string, 0, 6)
	if filter.TenantID != "" {
		args = append(args, filter.TenantID.String())
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d::uuid", len(args)))
	}
	if filter.ProjectID != "" {
		args = append(args, filter.ProjectID.String())
		conditions = append(conditions, fmt.Sprintf("project_id = $%d::uuid", len(args)))
	}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			statuses = append(statuses, string(status))
		}
		args = append(args, statuses)
		conditions = append(conditions, fmt.Sprintf("status = ANY($%d)", len(args)))
	}
	if filter.CreatedAfter != nil {
		args = append(args, filter.CreatedAfter.UTC())
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if filter.CreatedBefore != nil {
		args = append(args, filter.CreatedBefore.UTC())
		conditions = append(conditions, fmt.Sprintf("created_at < $%d", len(args)))
	}
	if filter.Cursor != "" {
		cursor, err := decodeRunCursor(filter.Cursor)
		if err != nil {
			return domain.RunPage{}, err
		}
		args = append(args, cursor.CreatedAt, cursor.RunID.String())
		conditions = append(conditions, fmt.Sprintf("(created_at, run_id) < ($%d, $%d::uuid)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := "SELECT " + runColumns + " FROM runs WHERE " + strings.Join(conditions, " AND ") +
		fmt.Sprintf(" ORDER BY created_at DESC, run_id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return domain.RunPage{}, mapError(err, "run", filter.TenantID.String())
	}
	runs, err := collectRuns(rows)
	if err != nil {
		return domain.RunPage{}, err
	}
	page := domain.RunPage{Runs: runs}
	if len(runs) > limit {
		last := runs[limit-1]
		page.NextCursor = encodeRunCursor(last.CreatedAt, last.ID)
		page.Runs = runs[:limit]
	}
	return page, nil
}

// UpdateRun applies an optimistic run status transition and records the
// optional outbox event in the same transaction.
func (s *Store) UpdateRun(ctx context.Context, update domain.RunUpdate) (domain.Run, error) {
	if err := update.Validate(); err != nil {
		return domain.Run{}, err
	}
	if update.Verdict != "" && !update.To.Terminal() && update.To != domain.RunAggregating {
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + update.RunID.String(), From: string(update.To), To: "verdict",
			Reason: "verdict is only final after aggregation",
		}
	}
	if update.FailureClass != "" && !update.FailureClass.Valid() {
		return domain.Run{}, &domain.ValidationError{Field: "failure_class", Reason: "unknown error class"}
	}
	at := normalizeTime(update.At)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, fmt.Errorf("begin update run: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	run, err := scanRun(tx.QueryRow(ctx, `UPDATE runs SET
		status = $4,
		state_version = state_version + 1,
		updated_at = $5,
		verdict = CASE WHEN $6 = '' THEN verdict ELSE $6 END,
		failure_class = CASE WHEN $7 = '' THEN failure_class ELSE $7 END,
		failure_message = CASE WHEN $8 = '' THEN failure_message ELSE $8 END,
		planned_at = CASE WHEN $4 = 'PLANNED' THEN COALESCE(planned_at, $5) ELSE planned_at END,
		started_at = CASE WHEN $4 = 'RUNNING' THEN COALESCE(started_at, $5) ELSE started_at END,
		finished_at = CASE WHEN $4 IN ('COMPLETED', 'PARTIAL', 'INFRA_FAILED', 'CANCELLED', 'REJECTED')
			THEN $5 ELSE finished_at END
		WHERE run_id = $1::uuid AND tenant_id = $2::uuid AND state_version = $3 AND status = $9
		RETURNING `+runColumns,
		update.RunID.String(), update.TenantID.String(), update.ExpectedVersion, string(update.To),
		at, string(update.Verdict), string(update.FailureClass), update.FailureMessage,
		string(update.From),
	))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Run{}, mapError(err, "run", update.RunID.String())
		}
		current, loadErr := scanRun(tx.QueryRow(ctx,
			"SELECT "+runColumns+" FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid",
			update.RunID.String(), update.TenantID.String()))
		if loadErr != nil {
			return domain.Run{}, mapError(loadErr, "run", update.RunID.String())
		}
		if current.StateVersion != update.ExpectedVersion {
			return domain.Run{}, &domain.VersionConflictError{
				Resource: "run " + update.RunID.String(), Expected: update.ExpectedVersion, Actual: current.StateVersion,
			}
		}
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + update.RunID.String(), From: string(current.Status), To: string(update.To),
			Reason: "expected from-status " + string(update.From),
		}
	}
	if update.Event != nil {
		if err := insertOutbox(ctx, tx, []domain.OutboxEvent{*update.Event}); err != nil {
			return domain.Run{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, fmt.Errorf("commit update run: %w", err)
	}
	finished = true
	return run, nil
}

// FreezeRunInputs stores frozen snapshot digests while the run is still
// validating, guarded by an optimistic version check.
func (s *Store) FreezeRunInputs(ctx context.Context, freeze domain.RunFreeze) (domain.Run, error) {
	if err := freeze.TenantID.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := freeze.RunID.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := domain.RequireNonEmpty("snapshot_digest", freeze.SnapshotDigest); err != nil {
		return domain.Run{}, err
	}
	at := normalizeTime(freeze.At)
	run, err := scanRun(s.pool.QueryRow(ctx, `UPDATE runs SET
		snapshot_digest = $4, base_snapshot = $5, head_snapshot = $6, updated_at = $7,
		state_version = state_version + 1
		WHERE run_id = $1::uuid AND tenant_id = $2::uuid AND state_version = $3
			AND status IN ('CREATED', 'VALIDATING')
		RETURNING `+runColumns,
		freeze.RunID.String(), freeze.TenantID.String(), freeze.ExpectedVersion,
		freeze.SnapshotDigest, freeze.BaseSnapshot, freeze.HeadSnapshot, at,
	))
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, mapError(err, "run", freeze.RunID.String())
	}
	current, loadErr := scanRun(s.pool.QueryRow(ctx,
		"SELECT "+runColumns+" FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid",
		freeze.RunID.String(), freeze.TenantID.String()))
	if loadErr != nil {
		return domain.Run{}, mapError(loadErr, "run", freeze.RunID.String())
	}
	if current.Status != domain.RunCreated && current.Status != domain.RunValidating {
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + freeze.RunID.String(), From: string(current.Status), To: "frozen",
			Reason: "inputs can only be frozen before planning",
		}
	}
	return domain.Run{}, &domain.VersionConflictError{
		Resource: "run " + freeze.RunID.String(), Expected: freeze.ExpectedVersion, Actual: current.StateVersion,
	}
}

// PlanRun stores a validated DAG and transitions the run to PLANNED atomically.
func (s *Store) PlanRun(ctx context.Context, plan domain.PlanTransaction) (domain.Run, error) {
	if err := plan.TenantID.Validate(); err != nil {
		return domain.Run{}, err
	}
	dag := domain.DAG{
		RunID:        plan.RunID,
		Steps:        plan.Steps,
		Dependencies: plan.Dependencies,
		Claims:       plan.Claims,
	}
	if err := dag.Validate(); err != nil {
		return domain.Run{}, err
	}
	for _, step := range plan.Steps {
		if step.TenantID != plan.TenantID {
			return domain.Run{}, &domain.ValidationError{
				Field: "step.tenant_id", Reason: "all steps must belong to the planning tenant",
			}
		}
	}
	at := normalizeTime(plan.At)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, fmt.Errorf("begin plan run: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	run, err := scanRun(tx.QueryRow(ctx,
		"SELECT "+runColumns+" FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid FOR UPDATE",
		plan.RunID.String(), plan.TenantID.String()))
	if err != nil {
		return domain.Run{}, mapError(err, "run", plan.RunID.String())
	}
	if run.StateVersion != plan.ExpectedVersion {
		return domain.Run{}, &domain.VersionConflictError{
			Resource: "run " + plan.RunID.String(), Expected: plan.ExpectedVersion, Actual: run.StateVersion,
		}
	}
	if run.Status != domain.RunValidating && run.Status != domain.RunPlanned {
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + plan.RunID.String(), From: string(run.Status), To: string(domain.RunPlanned),
		}
	}
	if run.Status == domain.RunPlanned {
		for _, statement := range []string{
			"DELETE FROM step_metrics WHERE run_id = $1::uuid",
			"DELETE FROM attempts WHERE run_id = $1::uuid",
			"DELETE FROM step_dependencies WHERE run_id = $1::uuid",
			"DELETE FROM steps WHERE run_id = $1::uuid",
			"DELETE FROM claims WHERE run_id = $1::uuid",
		} {
			if _, err := tx.Exec(ctx, statement, plan.RunID.String()); err != nil {
				return domain.Run{}, mapError(err, "run", plan.RunID.String())
			}
		}
	}

	stepCreatedAt := make(map[domain.StepID]time.Time, len(plan.Steps))
	for _, step := range plan.Steps {
		requirements, err := encodeJSON(step.Requirements)
		if err != nil {
			return domain.Run{}, err
		}
		options := step.Options
		if options == nil {
			options = map[string]string{}
		}
		encodedOptions, err := encodeJSON(options)
		if err != nil {
			return domain.Run{}, err
		}
		createdAt := step.CreatedAt
		if createdAt.IsZero() {
			createdAt = at
		} else {
			createdAt = createdAt.UTC()
		}
		updatedAt := step.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = createdAt
		} else {
			updatedAt = updatedAt.UTC()
		}
		stepCreatedAt[step.ID] = createdAt
		if _, err := tx.Exec(ctx, `INSERT INTO steps (
			step_id, run_id, run_created_at, tenant_id, key, kind, status, generation,
			attempt_count, max_attempts, current_attempt_id, lease_owner, lease_deadline,
			target_cluster, requirements, input_manifest_digest, result_digest,
			output_manifest, failure_class, failure_message, next_attempt_at, priority,
			shard, options, created_at, updated_at, started_at, finished_at
		) VALUES (
			$1::uuid, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, $9, $10,
			NULLIF($11, '')::uuid, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
			$22, $23, $24, $25, $26, $27, $28
		)`,
			step.ID.String(), plan.RunID.String(), run.CreatedAt, plan.TenantID.String(),
			step.Key, string(step.Kind), string(step.Status), step.Generation,
			step.AttemptCount, step.MaxAttempts, step.CurrentAttemptID.String(), step.LeaseOwner,
			utcPtr(step.LeaseDeadline), step.TargetCluster.String(), requirements,
			step.InputManifestDigest, step.ResultDigest, step.OutputManifest,
			string(step.FailureClass), step.FailureMessage, utcPtr(step.NextAttemptAt),
			step.Priority, step.Shard, encodedOptions, createdAt, updatedAt,
			utcPtr(step.StartedAt), utcPtr(step.FinishedAt),
		); err != nil {
			return domain.Run{}, mapError(err, "step", step.ID.String())
		}
	}

	for stepID, dependencies := range plan.Dependencies {
		stepAt, ok := stepCreatedAt[stepID]
		if !ok {
			return domain.Run{}, &domain.ValidationError{Field: "dag.dependencies", Reason: "dependency source is not a step"}
		}
		for _, dependencyID := range dependencies {
			dependencyAt, ok := stepCreatedAt[dependencyID]
			if !ok {
				return domain.Run{}, &domain.ValidationError{Field: "dag.dependencies", Reason: "dependency target is not a step"}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO step_dependencies (
				run_id, step_id, step_created_at, depends_on_step_id, depends_on_created_at, created_at
			) VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6)
			ON CONFLICT (step_id, depends_on_step_id) DO NOTHING`,
				plan.RunID.String(), stepID.String(), stepAt, dependencyID.String(), dependencyAt, at,
			); err != nil {
				return domain.Run{}, mapError(err, "step_dependency", stepID.String())
			}
		}
	}

	for _, claim := range plan.Claims {
		assumptions, err := encodeJSON(claim.Assumptions)
		if err != nil {
			return domain.Run{}, err
		}
		claimAt := claim.CreatedAt
		if claimAt.IsZero() {
			claimAt = at
		}
		if _, err := tx.Exec(ctx, `INSERT INTO claims (
			claim_id, run_id, run_created_at, tenant_id, contract_id, kind, title, predicate,
			domain, assumptions, observable, oracle_ref, input_schema, language, symbol,
			required, minimum_result, created_at
		) VALUES (
			$1, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18
		) ON CONFLICT (run_id, claim_id) DO NOTHING`,
			string(claim.ID), plan.RunID.String(), run.CreatedAt, plan.TenantID.String(),
			claim.ContractID, string(claim.Kind), claim.Title, claim.Predicate, claim.Domain,
			assumptions, claim.Observable, claim.OracleRef, claim.InputSchema, claim.Language,
			claim.Symbol, claim.Required, string(claim.MinimumResult), claimAt.UTC(),
		); err != nil {
			return domain.Run{}, mapError(err, "claim", string(claim.ID))
		}
	}

	if err := insertOutbox(ctx, tx, plan.Events); err != nil {
		return domain.Run{}, err
	}

	updated, err := scanRun(tx.QueryRow(ctx, `UPDATE runs SET
		status = 'PLANNED', state_version = state_version + 1, updated_at = $3,
		planned_at = $3
		WHERE run_id = $1::uuid AND created_at = $2
		RETURNING `+runColumns, plan.RunID.String(), run.CreatedAt, at))
	if err != nil {
		return domain.Run{}, mapError(err, "run", plan.RunID.String())
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, fmt.Errorf("commit plan run: %w", err)
	}
	finished = true
	return updated, nil
}

// GetDAG loads the steps, dependencies, and claims of a planned run.
func (s *Store) GetDAG(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.DAG, error) {
	run, err := s.GetRun(ctx, tenantID, runID)
	if err != nil {
		return domain.DAG{}, err
	}
	steps, err := s.ListSteps(ctx, tenantID, runID)
	if err != nil {
		return domain.DAG{}, err
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+claimColumns+" FROM claims WHERE run_id = $1::uuid AND tenant_id = $2::uuid ORDER BY claim_id",
		runID.String(), tenantID.String())
	if err != nil {
		return domain.DAG{}, mapError(err, "claim", runID.String())
	}
	claims, err := collectClaims(rows)
	if err != nil {
		return domain.DAG{}, err
	}
	dependencyRows, err := s.pool.Query(ctx, `SELECT step_id::text, depends_on_step_id::text
		FROM step_dependencies WHERE run_id = $1::uuid ORDER BY step_id, depends_on_step_id`,
		runID.String())
	if err != nil {
		return domain.DAG{}, mapError(err, "step_dependency", runID.String())
	}
	dependencies := make(map[domain.StepID][]domain.StepID, len(steps))
	if err := func() error {
		defer dependencyRows.Close()
		for dependencyRows.Next() {
			var stepID, dependencyID string
			if err := dependencyRows.Scan(&stepID, &dependencyID); err != nil {
				return err
			}
			source := domain.StepID(stepID)
			dependencies[source] = append(dependencies[source], domain.StepID(dependencyID))
		}
		return dependencyRows.Err()
	}(); err != nil {
		return domain.DAG{}, err
	}
	return domain.DAG{
		RunID:        run.ID,
		Steps:        steps,
		Dependencies: dependencies,
		Claims:       claims,
	}, nil
}
