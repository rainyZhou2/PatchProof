package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/app/proof"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/planner"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/policy"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

type Config struct {
	Region               string
	Version              string
	PollInterval         time.Duration
	ValidationWorkers    int
	MaxRepositoriesBytes int64
	MaxChangedFiles      int
	RequireAttestation   bool
	SupportsBrokeredIO   bool
	ShardCount           int
	Clock                ports.Clock
	Logger               *slog.Logger
}

type Service struct {
	store     ports.MetadataStore
	planner   *planner.Planner
	policy    ports.PolicyEngine
	snapshots ports.Snapshotter
	cas       ports.ArtifactStore
	proof     *proof.Service
	clock     ports.Clock
	logger    *slog.Logger
	metrics   *telemetry.Metrics
	config    Config
	inflight  sync.Map
	semaphore chan struct{}
	notify    chan struct{}
}

func New(store ports.MetadataStore, plannerInstance *planner.Planner, policyEngine ports.PolicyEngine, snapshots ports.Snapshotter, cas ports.ArtifactStore, proofService *proof.Service, config Config, metrics *telemetry.Metrics) (*Service, error) {
	if store == nil || plannerInstance == nil || policyEngine == nil || snapshots == nil || cas == nil || proofService == nil {
		return nil, fmt.Errorf("orchestrator: all dependencies are required")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.ValidationWorkers <= 0 {
		config.ValidationWorkers = 4
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Service{
		store: store, planner: plannerInstance, policy: policyEngine, snapshots: snapshots,
		cas: cas, proof: proofService, clock: config.Clock, logger: config.Logger,
		metrics: metrics, config: config, semaphore: make(chan struct{}, config.ValidationWorkers),
		notify: make(chan struct{}, 1),
	}, nil
}

func (s *Service) Wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.tick(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("orchestrator tick failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.notify:
		}
	}
}

func (s *Service) tick(ctx context.Context) error {
	if _, err := s.store.PromoteDueSteps(ctx, s.clock.Now(), 256); err != nil {
		s.logger.Warn("promote retry steps failed", slog.String("error", err.Error()))
	}
	statuses := []domain.RunStatus{
		domain.RunCreated, domain.RunValidating, domain.RunPlanned, domain.RunQueued,
		domain.RunRunning, domain.RunAggregating, domain.RunCancelling,
	}
	filter := domain.RunFilter{Statuses: statuses, Limit: 100}
	for page := 0; page < 10; page++ {
		runs, err := s.store.ListRuns(ctx, filter)
		if err != nil {
			return err
		}
		for _, run := range runs.Runs {
			s.dispatch(ctx, run)
		}
		if runs.NextCursor == "" {
			break
		}
		filter.Cursor = runs.NextCursor
	}
	return nil
}

func (s *Service) dispatch(ctx context.Context, run domain.Run) {
	if _, loaded := s.inflight.LoadOrStore(run.ID, struct{}{}); loaded {
		return
	}
	select {
	case s.semaphore <- struct{}{}:
	default:
		s.inflight.Delete(run.ID)
		return
	}
	go func() {
		defer func() {
			<-s.semaphore
			s.inflight.Delete(run.ID)
			if recovered := recover(); recovered != nil {
				s.logger.Error("orchestrator panic", slog.String("run_id", run.ID.String()), slog.Any("panic", recovered))
			}
		}()
		if err := s.process(ctx, run); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn("run processing failed", slog.String("run_id", run.ID.String()), slog.String("status", string(run.Status)), slog.String("error", err.Error()))
		}
	}()
}

func (s *Service) process(ctx context.Context, run domain.Run) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	switch run.Status {
	case domain.RunCreated:
		return s.transition(ctx, run, domain.RunValidating, domain.VerdictNone, domain.EventRunValidated, map[string]any{"run_id": run.ID.String()})
	case domain.RunValidating:
		return s.validateAndPlan(ctx, run)
	case domain.RunPlanned:
		return s.transition(ctx, run, domain.RunQueued, domain.VerdictNone, domain.EventRunQueued, map[string]any{"run_id": run.ID.String()})
	case domain.RunQueued, domain.RunRunning:
		return s.advanceExecution(ctx, run)
	case domain.RunAggregating:
		return s.aggregate(ctx, run)
	case domain.RunCancelling:
		return s.finishCancellation(ctx, run)
	default:
		return nil
	}
}

func (s *Service) validateAndPlan(ctx context.Context, run domain.Run) error {
	tenant, err := s.store.GetTenant(ctx, run.TenantID)
	if err != nil {
		return err
	}
	project, err := s.store.GetProject(ctx, run.TenantID, run.ProjectID)
	if err != nil {
		return err
	}
	storedPolicy, err := s.store.GetPolicy(ctx, run.TenantID, run.PolicyDigest)
	if err != nil {
		return s.reject(ctx, run, domain.VerdictPolicyBlocked, &domain.PolicyError{Code: "POLICY_UNAVAILABLE", Reason: err.Error()})
	}
	policyDocument, err := domain.ParsePolicyDocument(storedPolicy.Document)
	if err != nil {
		return s.reject(ctx, run, domain.VerdictPolicyBlocked, &domain.PolicyError{Code: "POLICY_INVALID", Reason: err.Error()})
	}

	admission := ports.PolicyInput{
		Tenant: tenant, Project: project, Policy: policyDocument, PolicyDigest: run.PolicyDigest,
		PolicyTrusted: storedPolicy.Trusted, RequestedChecks: run.RequestedChecks,
		Isolation: policyDocument.Spec.Execution.IsolationTier, Region: run.Region,
	}
	decision, err := s.policy.Evaluate(ctx, admission)
	if err != nil {
		return s.reject(ctx, run, domain.VerdictPolicyBlocked, err)
	}
	if !decision.Allowed {
		return s.rejectWithDecision(ctx, run, decision)
	}
	if decision.ArtifactTransport == domain.TransportBrokeredOffline && !s.config.SupportsBrokeredIO {
		return s.reject(ctx, run, domain.VerdictInfraFailed, &domain.PolicyError{Code: "BROKER_UNAVAILABLE", Reason: "policy requests brokered offline IO but this control plane has no broker image configured"})
	}

	repository := domain.RepositoryRef{
		Provider: project.Provider, URI: project.RepositoryURI, Commit: run.BaseCommit,
		TenantID: run.TenantID.String(), TrustDomain: "run",
		MaxBytes: s.config.MaxRepositoriesBytes,
	}
	baseSnapshot, err := s.snapshots.Snapshot(ctx, ports.SnapshotRequest{Repository: repository, TenantID: run.TenantID, Budget: run.Budget})
	if err != nil {
		return s.reject(ctx, run, domain.VerdictInconclusive, err)
	}
	repository.Commit = run.HeadCommit
	headSnapshot, err := s.snapshots.Snapshot(ctx, ports.SnapshotRequest{Repository: repository, TenantID: run.TenantID, Budget: run.Budget})
	if err != nil {
		return s.reject(ctx, run, domain.VerdictInconclusive, err)
	}
	changed := changedPaths(baseSnapshot.Manifest, headSnapshot.Manifest)
	repositoryGiB := float64(maxInt64(baseSnapshot.Manifest.TotalBytes, headSnapshot.Manifest.TotalBytes)) / (1 << 30)
	snapshotDigest, err := snapshotDigest(baseSnapshot.Manifest.Digest, headSnapshot.Manifest.Digest)
	if err != nil {
		return err
	}
	frozen, err := s.store.FreezeRunInputs(ctx, domain.RunFreeze{
		TenantID: run.TenantID, RunID: run.ID, ExpectedVersion: run.StateVersion,
		SnapshotDigest: snapshotDigest, BaseSnapshot: baseSnapshot.Archive.URI(), HeadSnapshot: headSnapshot.Archive.URI(),
		At: s.clock.Now(),
	})
	if err != nil {
		return err
	}
	run = frozen

	decision, err = s.policy.Evaluate(ctx, ports.PolicyInput{
		Tenant: tenant, Project: project, Policy: policyDocument, PolicyDigest: run.PolicyDigest,
		PolicyTrusted: storedPolicy.Trusted, RequestedChecks: run.RequestedChecks,
		ChangedPaths: changed, ChangedFiles: len(changed), RepositorySizeGiB: repositoryGiB,
		Submodules: len(baseSnapshot.Manifest.Submodules)+len(headSnapshot.Manifest.Submodules) > 0,
		Isolation:  policyDocument.Spec.Execution.IsolationTier, Region: run.Region,
	})
	if err != nil {
		return s.reject(ctx, run, domain.VerdictPolicyBlocked, err)
	}
	if !decision.Allowed {
		return s.rejectWithDecision(ctx, run, decision)
	}

	plan, err := s.planner.Plan(ctx, ports.PlanRequest{
		Run: run, Tenant: tenant, Project: project, Policy: policyDocument, Decision: decision,
		Budget: run.Budget, ShardCount: s.config.ShardCount, Now: s.clock.Now(),
	})
	if err != nil {
		return s.reject(ctx, run, domain.VerdictPolicyBlocked, err)
	}
	events := make([]domain.OutboxEvent, 0, len(plan.Steps))
	planEvent, err := domain.NewOutboxEvent(run.TenantID, domain.EventRunPlanned, run.ID.String(), run.StateVersion, map[string]any{
		"run_id": run.ID.String(), "steps": len(plan.Steps), "claims": len(plan.Claims),
	}, s.clock.Now())
	if err != nil {
		return err
	}
	events = append(events, planEvent)
	for _, step := range plan.Steps {
		if dependencies := plan.Dependencies[step.ID]; len(dependencies) > 0 {
			continue
		}
		event, err := domain.NewOutboxEvent(run.TenantID, domain.EventStepReady, step.ID.String(), 0, map[string]any{
			"run_id": run.ID.String(), "step_id": step.ID.String(), "kind": string(step.Kind), "shard": step.Shard,
		}, s.clock.Now())
		if err != nil {
			return err
		}
		events = append(events, event)
	}
	planned, err := s.store.PlanRun(ctx, domain.PlanTransaction{
		TenantID: run.TenantID, RunID: run.ID, ExpectedVersion: run.StateVersion,
		Steps: plan.Steps, Dependencies: plan.Dependencies, Claims: plan.Claims, Events: events, At: s.clock.Now(),
	})
	if err != nil {
		return err
	}
	policyViolated, err := s.recordProtectedAssets(ctx, planned, policyDocument, changed)
	if err != nil {
		s.logger.Warn("protected assets check failed", slog.String("run_id", planned.ID.String()), slog.String("error", err.Error()))
	}
	queued, err := s.transitionRun(ctx, planned, domain.RunQueued, domain.VerdictNone, domain.EventRunQueued, map[string]any{"run_id": planned.ID.String()})
	if err != nil {
		return err
	}
	if policyViolated {
		if _, err := s.store.CancelRunSteps(ctx, queued.TenantID, queued.ID, s.clock.Now()); err != nil {
			return err
		}
		running, err := s.transitionRun(ctx, queued, domain.RunRunning, domain.VerdictNone, domain.EventRunStarted, map[string]any{"run_id": queued.ID.String()})
		if err != nil {
			return err
		}
		_, err = s.transitionRun(ctx, running, domain.RunAggregating, domain.VerdictNone, domain.EventRunAggregating, map[string]any{"run_id": running.ID.String()})
		return err
	}
	return nil
}

func (s *Service) recordProtectedAssets(ctx context.Context, run domain.Run, document domain.PolicyDocument, changed []string) (bool, error) {
	claimID := domain.ClaimID("protected-assets")
	required := false
	for _, claim := range mustDAGClaims(ctx, s.store, run) {
		if claim.ID == claimID {
			required = claim.Required
			break
		}
	}
	if !required {
		return false, nil
	}
	matcher, err := policy.NewPathMatcher(document.Spec.ProtectedPaths)
	if err != nil {
		return false, err
	}
	matched := matcher.Matches(changed)
	result := domain.ClaimTested
	if len(matched) > 0 {
		result = domain.ClaimViolated
	}
	scope := ports.SecurityScope{TenantID: run.TenantID, TrustDomain: "run", Region: run.Region}
	changedJSON, err := evidence.Canonicalize(map[string]any{"changed": changed, "protected_matches": matched})
	if err != nil {
		return false, err
	}
	artifact, err := s.cas.Put(ctx, scope, strings.NewReader(string(changedJSON)))
	if err != nil {
		return false, err
	}
	entry := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      run.TenantID,
		RunID:         run.ID,
		ClaimID:       claimID,
		Kind:          domain.EvidencePolicy,
		Result:        result,
		Scope: domain.EvidenceScope{
			InputDomain: "all files changed between base and head",
			Details:     map[string]string{"changed_files": fmt.Sprintf("%d", len(changed)), "protected_matches": fmt.Sprintf("%d", len(matched))},
		},
		Assumptions: []string{"path matching uses trusted policy patterns", "changed paths are derived from content digests"},
		Engine:      domain.EngineInfo{Name: "builtin-policy", Version: "0.1.0"},
		Environment: domain.EnvironmentInfo{Arch: runtime.GOARCH, SandboxProfile: "trusted-controller", Region: run.Region},
		Artifacts:   []domain.CASRef{artifact},
		Limits:      []string{"only path-level policy is evaluated; file contents are not inspected"},
		CreatedAt:   s.clock.Now(),
	}
	if err := entry.Validate(); err != nil {
		return false, err
	}
	if err := s.store.AppendEvidence(ctx, []domain.Evidence{entry}); err != nil {
		return false, err
	}
	return result == domain.ClaimViolated, nil
}

func (s *Service) advanceExecution(ctx context.Context, run domain.Run) error {
	dag, err := s.store.GetDAG(ctx, run.TenantID, run.ID)
	if err != nil {
		return err
	}
	active := int32(0)
	started := false
	for _, step := range dag.Steps {
		if step.Status.Active() {
			active++
		}
		switch step.Status {
		case domain.StepLeased, domain.StepRunning, domain.StepSucceeded, domain.StepFailed, domain.StepTimedOut, domain.StepCancelled:
			started = true
		}
	}
	if active == 0 {
		if run.Status == domain.RunQueued || run.Status == domain.RunRunning {
			return s.startAggregation(ctx, run)
		}
		return nil
	}
	if run.Status == domain.RunQueued && started {
		_, err := s.transitionRun(ctx, run, domain.RunRunning, domain.VerdictNone, domain.EventRunStarted, map[string]any{"run_id": run.ID.String()})
		return err
	}
	return nil
}

func (s *Service) startAggregation(ctx context.Context, run domain.Run) error {
	current := run
	if run.Status == domain.RunQueued {
		started, err := s.transitionRun(ctx, run, domain.RunRunning, domain.VerdictNone, domain.EventRunStarted, map[string]any{"run_id": run.ID.String()})
		if err != nil {
			return err
		}
		current = started
	}
	_, err := s.transitionRun(ctx, current, domain.RunAggregating, domain.VerdictNone, domain.EventRunAggregating, map[string]any{"run_id": run.ID.String()})
	s.Wake()
	return err
}

func (s *Service) aggregate(ctx context.Context, run domain.Run) error {
	report, err := s.proof.Aggregate(ctx, run)
	if err != nil {
		return err
	}
	status := domain.RunCompleted
	if report.Verdict == domain.VerdictInfraFailed {
		status = domain.RunInfraFailed
	}
	if report.Verdict == domain.VerdictCancelled {
		status = domain.RunCancelled
	}
	if status == domain.RunCompleted && needsPartial(report, s.config.RequireAttestation) {
		status = domain.RunPartial
	}
	_, err = s.transitionRun(ctx, run, status, report.Verdict, domain.EventRunCompleted, map[string]any{
		"run_id": run.ID.String(), "verdict": string(report.Verdict), "report_id": report.ReportID.String(),
	})
	return err
}

func (s *Service) finishCancellation(ctx context.Context, run domain.Run) error {
	if _, err := s.store.CancelRunSteps(ctx, run.TenantID, run.ID, s.clock.Now()); err != nil {
		return err
	}
	if _, err := s.proof.BuildRejectionReport(ctx, run, domain.VerdictCancelled, "CANCELLED", "verification was cancelled before completion", "principal requested cancellation"); err != nil {
		s.logger.Warn("cancellation report failed", slog.String("run_id", run.ID.String()), slog.String("error", err.Error()))
	}
	_, err := s.transitionRun(ctx, run, domain.RunCancelled, domain.VerdictCancelled, domain.EventRunCancelled, map[string]any{
		"run_id": run.ID.String(), "verdict": string(domain.VerdictCancelled),
	})
	return err
}

func (s *Service) reject(ctx context.Context, run domain.Run, verdict domain.Verdict, cause error) error {
	reason := cause.Error()
	s.logger.Warn("run rejected", slog.String("run_id", run.ID.String()), slog.String("reason", reason))
	report, reportErr := s.proof.BuildRejectionReport(ctx, run, verdict, rejectionCode(verdict), "run was rejected before verification completed", reason)
	if reportErr != nil {
		s.logger.Warn("rejection report failed", slog.String("run_id", run.ID.String()), slog.String("error", reportErr.Error()))
	}
	status := domain.RunInfraFailed
	if verdict == domain.VerdictPolicyBlocked {
		status = domain.RunRejected
	}
	_ = report
	return s.transition(ctx, run, status, verdict, domain.EventRunCompleted, map[string]any{"run_id": run.ID.String(), "reason": reason})
}

func (s *Service) rejectWithDecision(ctx context.Context, run domain.Run, decision ports.PolicyDecision) error {
	reasons := make([]string, 0, len(decision.Violations))
	for _, violation := range decision.Violations {
		reasons = append(reasons, violation.Code+": "+violation.Message)
		if s.metrics != nil {
			s.metrics.PolicyBlock.WithLabelValues(violation.Code).Inc()
		}
	}
	return s.reject(ctx, run, domain.VerdictPolicyBlocked, &domain.PolicyError{Code: "POLICY_VIOLATION", Reason: strings.Join(reasons, "; ")})
}

func (s *Service) transition(ctx context.Context, run domain.Run, next domain.RunStatus, verdict domain.Verdict, eventType domain.EventType, payload map[string]any) error {
	_, err := s.transitionRun(ctx, run, next, verdict, eventType, payload)
	return err
}

func (s *Service) transitionRun(ctx context.Context, run domain.Run, next domain.RunStatus, verdict domain.Verdict, eventType domain.EventType, payload map[string]any) (domain.Run, error) {
	if !run.Status.CanTransitionTo(next) {
		return run, &domain.StateTransitionError{Resource: "run " + run.ID.String(), From: string(run.Status), To: string(next)}
	}
	event, err := domain.NewOutboxEvent(run.TenantID, eventType, run.ID.String(), run.StateVersion, payload, s.clock.Now())
	if err != nil {
		return run, err
	}
	return s.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID: run.TenantID, RunID: run.ID, ExpectedVersion: run.StateVersion,
		From: run.Status, To: next, Verdict: verdict, At: s.clock.Now(), Event: &event,
	})
}

func changedPaths(base, head domain.SnapshotManifest) []string {
	baseFiles := make(map[string]string, len(base.Files))
	for _, file := range base.Files {
		baseFiles[file.Path] = file.Digest
	}
	headFiles := make(map[string]string, len(head.Files))
	for _, file := range head.Files {
		headFiles[file.Path] = file.Digest
	}
	set := map[string]struct{}{}
	for path, digest := range baseFiles {
		if other, ok := headFiles[path]; !ok || other != digest {
			set[path] = struct{}{}
		}
	}
	for path := range headFiles {
		if _, ok := baseFiles[path]; !ok {
			set[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func snapshotDigest(base, head string) (string, error) {
	return evidence.CanonicalDigest(struct {
		Base string `json:"base"`
		Head string `json:"head"`
	}{Base: base, Head: head})
}

func needsPartial(report domain.Report, requireAttestation bool) bool {
	if requireAttestation && report.Attestation != domain.AttestationSigned {
		return true
	}
	for _, claim := range report.Claims {
		if claim.Required && (claim.Result == domain.ClaimUnknown || claim.Result == domain.ClaimNotRun) {
			return true
		}
	}
	requested := map[string]struct{}{}
	for _, check := range report.RequestedChecks {
		requested[check] = struct{}{}
	}
	for _, check := range report.CompletedChecks {
		delete(requested, check)
	}
	delete(requested, "protected-assets")
	return len(requested) > 0
}

func rejectionCode(verdict domain.Verdict) string {
	if verdict == domain.VerdictPolicyBlocked {
		return "POLICY_BLOCKED"
	}
	return "REJECTED"
}

func mustDAGClaims(ctx context.Context, store ports.MetadataStore, run domain.Run) []domain.Claim {
	dag, err := store.GetDAG(ctx, run.TenantID, run.ID)
	if err != nil {
		return nil
	}
	return dag.Claims
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
