package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/patchproof/patchproof/internal/domain"
)

// scanner is implemented by pgx.Row and pgx.Rows and lets the row mappers be
// tested without a database.
type scanner interface {
	Scan(dest ...any) error
}

const runColumns = `run_id::text, tenant_id::text, project_id::text, request_key, fingerprint,
	base_commit, head_commit, patch_digest, snapshot_digest, base_snapshot, head_snapshot,
	policy_digest, policy_ref, requested_checks, budget, replay, status, verdict, state_version,
	failure_class, failure_message, region, created_by, created_at, updated_at,
	planned_at, started_at, finished_at`

const stepColumns = `step_id::text, run_id::text, tenant_id::text, key, kind, status, generation,
	attempt_count, max_attempts, COALESCE(current_attempt_id::text, ''), lease_owner, lease_deadline,
	target_cluster, requirements, input_manifest_digest, result_digest, output_manifest,
	failure_class, failure_message, next_attempt_at, priority, shard, options, created_at,
	updated_at, started_at, finished_at`

const attemptColumns = `attempt_id::text, step_id::text, run_id::text, tenant_id::text, generation,
	status, cluster_id, worker_id, executor_ref, result_digest, error_class, error_message,
	created_at, started_at, finished_at`

const tenantColumns = `tenant_id::text, home_region, security_tier, status, active_writer_epoch,
	quota, display_name, created_at, updated_at`

const projectColumns = `project_id::text, tenant_id::text, provider, repository_uri, default_branch,
	default_policy_digest, allowed_regions, classification, created_at, updated_at`

const policyColumns = `tenant_id::text, COALESCE(project_id::text, ''), digest, version, source,
	trusted, document, created_by, created_at`

const outboxColumns = `event_id::text, tenant_id::text, event_type, aggregate_id,
	COALESCE(run_id::text, ''), generation, created_at, traceparent, payload, delivered_at,
	dead_lettered_at, delivery_attempts`

const clusterColumns = `cluster_id, region, availability_zone, executor_kind, endpoint, namespace,
	architectures, languages, engines, isolation_tiers, runtime_classes, available_cpu_milli,
	available_memory_mib, queue_depth, active_attempts, max_concurrent_steps, cost_tier, cas_region,
	healthy, maintenance, version, labels, last_heartbeat`

const claimColumns = `claim_id, run_id::text, tenant_id::text, contract_id, kind, title, predicate,
	domain, assumptions, observable, oracle_ref, input_schema, language, symbol, required,
	minimum_result, created_at`

func scanRun(row scanner) (domain.Run, error) {
	var (
		run          domain.Run
		runID        string
		tenantID     string
		projectID    string
		requested    []byte
		budget       []byte
		replay       []byte
		status       string
		verdict      string
		failureClass string
		plannedAt    *time.Time
		startedAt    *time.Time
		finishedAt   *time.Time
	)
	if err := row.Scan(
		&runID, &tenantID, &projectID, &run.RequestKey, &run.Fingerprint,
		&run.BaseCommit, &run.HeadCommit, &run.PatchDigest, &run.SnapshotDigest,
		&run.BaseSnapshot, &run.HeadSnapshot, &run.PolicyDigest, &run.PolicyRef,
		&requested, &budget, &replay, &status, &verdict, &run.StateVersion,
		&failureClass, &run.FailureMessage, &run.Region, &run.CreatedBy,
		&run.CreatedAt, &run.UpdatedAt, &plannedAt, &startedAt, &finishedAt,
	); err != nil {
		return domain.Run{}, err
	}
	run.ID = domain.RunID(runID)
	run.TenantID = domain.TenantID(tenantID)
	run.ProjectID = domain.ProjectID(projectID)
	run.Status = domain.RunStatus(status)
	run.Verdict = domain.Verdict(verdict)
	run.FailureClass = domain.ErrorClass(failureClass)
	run.CreatedAt = run.CreatedAt.UTC()
	run.UpdatedAt = run.UpdatedAt.UTC()
	run.PlannedAt = utcPtr(plannedAt)
	run.StartedAt = utcPtr(startedAt)
	run.FinishedAt = utcPtr(finishedAt)
	if err := decodeJSON(requested, &run.RequestedChecks); err != nil {
		return domain.Run{}, fmt.Errorf("scan run requested_checks: %w", err)
	}
	if err := decodeJSON(budget, &run.Budget); err != nil {
		return domain.Run{}, fmt.Errorf("scan run budget: %w", err)
	}
	if len(replay) > 0 {
		var spec domain.ReplaySpec
		if err := decodeJSON(replay, &spec); err != nil {
			return domain.Run{}, fmt.Errorf("scan run replay: %w", err)
		}
		if spec.SourceRunID != "" {
			run.Replay = &spec
		}
	}
	return run, nil
}

func scanStep(row scanner) (domain.Step, error) {
	var (
		step           domain.Step
		stepID         string
		runID          string
		tenantID       string
		kind           string
		status         string
		currentAttempt string
		targetCluster  string
		deadline       *time.Time
		requirements   []byte
		failureClass   string
		nextAttemptAt  *time.Time
		shard          int
		options        []byte
		startedAt      *time.Time
		finishedAt     *time.Time
	)
	if err := row.Scan(
		&stepID, &runID, &tenantID, &step.Key, &kind, &status, &step.Generation,
		&step.AttemptCount, &step.MaxAttempts, &currentAttempt, &step.LeaseOwner, &deadline,
		&targetCluster, &requirements, &step.InputManifestDigest, &step.ResultDigest,
		&step.OutputManifest, &failureClass, &step.FailureMessage, &nextAttemptAt,
		&step.Priority, &shard, &options, &step.CreatedAt, &step.UpdatedAt, &startedAt, &finishedAt,
	); err != nil {
		return domain.Step{}, err
	}
	step.ID = domain.StepID(stepID)
	step.RunID = domain.RunID(runID)
	step.TenantID = domain.TenantID(tenantID)
	step.Kind = domain.StepKind(kind)
	step.Status = domain.StepStatus(status)
	step.CurrentAttemptID = domain.AttemptID(currentAttempt)
	step.TargetCluster = domain.ClusterID(targetCluster)
	step.LeaseDeadline = utcPtr(deadline)
	step.FailureClass = domain.ErrorClass(failureClass)
	step.NextAttemptAt = utcPtr(nextAttemptAt)
	step.Shard = shard
	step.CreatedAt = step.CreatedAt.UTC()
	step.UpdatedAt = step.UpdatedAt.UTC()
	step.StartedAt = utcPtr(startedAt)
	step.FinishedAt = utcPtr(finishedAt)
	if err := decodeJSON(requirements, &step.Requirements); err != nil {
		return domain.Step{}, fmt.Errorf("scan step requirements: %w", err)
	}
	if err := decodeJSON(options, &step.Options); err != nil {
		return domain.Step{}, fmt.Errorf("scan step options: %w", err)
	}
	return step, nil
}

func scanAttempt(row scanner) (domain.Attempt, error) {
	var (
		attempt    domain.Attempt
		attemptID  string
		stepID     string
		runID      string
		tenantID   string
		status     string
		clusterID  string
		errorClass string
		startedAt  *time.Time
		finishedAt *time.Time
	)
	if err := row.Scan(
		&attemptID, &stepID, &runID, &tenantID, &attempt.Generation, &status,
		&clusterID, &attempt.WorkerID, &attempt.ExecutorRef, &attempt.ResultDigest,
		&errorClass, &attempt.ErrorMessage, &attempt.CreatedAt, &startedAt, &finishedAt,
	); err != nil {
		return domain.Attempt{}, err
	}
	attempt.ID = domain.AttemptID(attemptID)
	attempt.StepID = domain.StepID(stepID)
	attempt.RunID = domain.RunID(runID)
	attempt.TenantID = domain.TenantID(tenantID)
	attempt.Status = domain.AttemptStatus(status)
	attempt.ClusterID = domain.ClusterID(clusterID)
	attempt.ErrorClass = domain.ErrorClass(errorClass)
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.StartedAt = utcPtr(startedAt)
	attempt.FinishedAt = utcPtr(finishedAt)
	return attempt, nil
}

func scanTenant(row scanner) (domain.Tenant, error) {
	var (
		tenant   domain.Tenant
		tenantID string
		security string
		status   string
		quota    []byte
	)
	if err := row.Scan(
		&tenantID, &tenant.HomeRegion, &security, &status, &tenant.ActiveWriterEpoch,
		&quota, &tenant.DisplayName, &tenant.CreatedAt, &tenant.UpdatedAt,
	); err != nil {
		return domain.Tenant{}, err
	}
	tenant.ID = domain.TenantID(tenantID)
	tenant.SecurityTier = domain.SecurityTier(security)
	tenant.Status = domain.TenantStatus(status)
	tenant.CreatedAt = tenant.CreatedAt.UTC()
	tenant.UpdatedAt = tenant.UpdatedAt.UTC()
	if err := decodeJSON(quota, &tenant.Quota); err != nil {
		return domain.Tenant{}, fmt.Errorf("scan tenant quota: %w", err)
	}
	return tenant, nil
}

func scanProject(row scanner) (domain.Project, error) {
	var (
		project       domain.Project
		projectID     string
		tenantID      string
		allowedRegion []byte
	)
	if err := row.Scan(
		&projectID, &tenantID, &project.Provider, &project.RepositoryURI,
		&project.DefaultBranch, &project.DefaultPolicyDigest, &allowedRegion,
		&project.Classification, &project.CreatedAt, &project.UpdatedAt,
	); err != nil {
		return domain.Project{}, err
	}
	project.ID = domain.ProjectID(projectID)
	project.TenantID = domain.TenantID(tenantID)
	project.CreatedAt = project.CreatedAt.UTC()
	project.UpdatedAt = project.UpdatedAt.UTC()
	if err := decodeJSON(allowedRegion, &project.AllowedRegions); err != nil {
		return domain.Project{}, fmt.Errorf("scan project allowed_regions: %w", err)
	}
	return project, nil
}

func scanPolicy(row scanner) (domain.Policy, error) {
	var (
		policy    domain.Policy
		tenantID  string
		projectID string
		source    string
	)
	if err := row.Scan(
		&tenantID, &projectID, &policy.Digest, &policy.Version, &source,
		&policy.Trusted, &policy.Document, &policy.CreatedBy, &policy.CreatedAt,
	); err != nil {
		return domain.Policy{}, err
	}
	policy.TenantID = domain.TenantID(tenantID)
	policy.ProjectID = domain.ProjectID(projectID)
	policy.Source = domain.PolicySource(source)
	policy.CreatedAt = policy.CreatedAt.UTC()
	return policy, nil
}

func scanEvidence(row scanner) (domain.Evidence, error) {
	var document []byte
	if err := row.Scan(&document); err != nil {
		return domain.Evidence{}, err
	}
	var evidence domain.Evidence
	if err := decodeJSON(document, &evidence); err != nil {
		return domain.Evidence{}, fmt.Errorf("scan evidence document: %w", err)
	}
	evidence.CreatedAt = evidence.CreatedAt.UTC()
	return evidence, nil
}

func scanCounterexample(row scanner) (domain.Counterexample, error) {
	var document []byte
	if err := row.Scan(&document); err != nil {
		return domain.Counterexample{}, err
	}
	var counterexample domain.Counterexample
	if err := decodeJSON(document, &counterexample); err != nil {
		return domain.Counterexample{}, fmt.Errorf("scan counterexample document: %w", err)
	}
	counterexample.CreatedAt = counterexample.CreatedAt.UTC()
	return counterexample, nil
}

func scanReport(row scanner) (domain.Report, error) {
	var document []byte
	if err := row.Scan(&document); err != nil {
		return domain.Report{}, err
	}
	var report domain.Report
	if err := decodeJSON(document, &report); err != nil {
		return domain.Report{}, fmt.Errorf("scan report document: %w", err)
	}
	report.GeneratedAt = report.GeneratedAt.UTC()
	return report, nil
}

func scanOutboxEvent(row scanner) (domain.OutboxEvent, error) {
	var (
		event          domain.OutboxEvent
		eventID        string
		tenantID       string
		eventType      string
		runID          string
		payload        []byte
		deliveredAt    *time.Time
		deadLetteredAt *time.Time
	)
	if err := row.Scan(
		&eventID, &tenantID, &eventType, &event.AggregateID, &runID,
		&event.Generation, &event.OccurredAt, &event.TraceParent, &payload,
		&deliveredAt, &deadLetteredAt, &event.Attempts,
	); err != nil {
		return domain.OutboxEvent{}, err
	}
	event.ID = domain.EventID(eventID)
	event.TenantID = domain.TenantID(tenantID)
	event.Type = domain.EventType(eventType)
	event.RunID = domain.RunID(runID)
	event.OccurredAt = event.OccurredAt.UTC()
	event.DeliveredAt = utcPtr(deliveredAt)
	event.DeadLetteredAt = utcPtr(deadLetteredAt)
	event.Payload = payload
	return event, nil
}

func scanCluster(row scanner) (domain.Cluster, error) {
	var (
		cluster       domain.Cluster
		architectures []byte
		languages     []byte
		engines       []byte
		isolation     []byte
		runtime       []byte
		labels        []byte
		clusterID     string
	)
	if err := row.Scan(
		&clusterID, &cluster.Region, &cluster.AvailabilityZone, &cluster.ExecutorKind,
		&cluster.Endpoint, &cluster.Namespace, &architectures, &languages, &engines,
		&isolation, &runtime, &cluster.AvailableCPUMilli, &cluster.AvailableMemoryMiB,
		&cluster.QueueDepth, &cluster.ActiveAttempts, &cluster.MaxConcurrentSteps,
		&cluster.CostTier, &cluster.CasRegion, &cluster.Healthy, &cluster.Maintenance,
		&cluster.Version, &labels, &cluster.LastHeartbeat,
	); err != nil {
		return domain.Cluster{}, err
	}
	cluster.ID = domain.ClusterID(clusterID)
	if err := decodeJSON(architectures, &cluster.Architectures); err != nil {
		return domain.Cluster{}, fmt.Errorf("scan cluster architectures: %w", err)
	}
	if err := decodeJSON(languages, &cluster.Languages); err != nil {
		return domain.Cluster{}, fmt.Errorf("scan cluster languages: %w", err)
	}
	if err := decodeJSON(engines, &cluster.Engines); err != nil {
		return domain.Cluster{}, fmt.Errorf("scan cluster engines: %w", err)
	}
	var tiers []string
	if err := decodeJSON(isolation, &tiers); err != nil {
		return domain.Cluster{}, fmt.Errorf("scan cluster isolation_tiers: %w", err)
	}
	cluster.IsolationTiers = make([]domain.IsolationTier, 0, len(tiers))
	for _, tier := range tiers {
		cluster.IsolationTiers = append(cluster.IsolationTiers, domain.IsolationTier(tier))
	}
	if err := decodeJSON(runtime, &cluster.RuntimeClasses); err != nil {
		return domain.Cluster{}, fmt.Errorf("scan cluster runtime_classes: %w", err)
	}
	if err := decodeJSON(labels, &cluster.Labels); err != nil {
		return domain.Cluster{}, fmt.Errorf("scan cluster labels: %w", err)
	}
	cluster.LastHeartbeat = cluster.LastHeartbeat.UTC()
	return cluster, nil
}

func scanClaim(row scanner) (domain.Claim, error) {
	var (
		claim       domain.Claim
		runID       string
		tenantID    string
		kind        string
		assumptions []byte
		minimum     string
		claimID     string
	)
	if err := row.Scan(
		&claimID, &runID, &tenantID, &claim.ContractID, &kind, &claim.Title,
		&claim.Predicate, &claim.Domain, &assumptions, &claim.Observable,
		&claim.OracleRef, &claim.InputSchema, &claim.Language, &claim.Symbol,
		&claim.Required, &minimum, &claim.CreatedAt,
	); err != nil {
		return domain.Claim{}, err
	}
	claim.ID = domain.ClaimID(claimID)
	claim.RunID = domain.RunID(runID)
	claim.TenantID = domain.TenantID(tenantID)
	claim.Kind = domain.ClaimKind(kind)
	claim.MinimumResult = domain.ClaimResult(minimum)
	claim.CreatedAt = claim.CreatedAt.UTC()
	if err := decodeJSON(assumptions, &claim.Assumptions); err != nil {
		return domain.Claim{}, fmt.Errorf("scan claim assumptions: %w", err)
	}
	return claim, nil
}

func collectRuns(rows pgx.Rows) ([]domain.Run, error) {
	defer rows.Close()
	out := make([]domain.Run, 0, 8)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func collectSteps(rows pgx.Rows) ([]domain.Step, error) {
	defer rows.Close()
	out := make([]domain.Step, 0, 8)
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, step)
	}
	return out, rows.Err()
}

func collectAttempts(rows pgx.Rows) ([]domain.Attempt, error) {
	defer rows.Close()
	out := make([]domain.Attempt, 0, 4)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, attempt)
	}
	return out, rows.Err()
}

func collectEvidence(rows pgx.Rows) ([]domain.Evidence, error) {
	defer rows.Close()
	out := make([]domain.Evidence, 0, 8)
	for rows.Next() {
		evidence, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, evidence)
	}
	return out, rows.Err()
}

func collectCounterexamples(rows pgx.Rows) ([]domain.Counterexample, error) {
	defer rows.Close()
	out := make([]domain.Counterexample, 0, 4)
	for rows.Next() {
		counterexample, err := scanCounterexample(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, counterexample)
	}
	return out, rows.Err()
}

func collectClusters(rows pgx.Rows) ([]domain.Cluster, error) {
	defer rows.Close()
	out := make([]domain.Cluster, 0, 4)
	for rows.Next() {
		cluster, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cluster)
	}
	return out, rows.Err()
}

func collectClaims(rows pgx.Rows) ([]domain.Claim, error) {
	defer rows.Close()
	out := make([]domain.Claim, 0, 8)
	for rows.Next() {
		claim, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, claim)
	}
	return out, rows.Err()
}

func encodeJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode json: %w", err)
	}
	return raw, nil
}

func decodeJSON(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

func utcPtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
