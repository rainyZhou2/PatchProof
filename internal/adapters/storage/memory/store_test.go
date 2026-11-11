package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
)

var testNow = time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)

type fixture struct {
	store     *memory.Store
	tenantID  domain.TenantID
	projectID domain.ProjectID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		store:     memory.New(),
		tenantID:  domain.NewTenantID(),
		projectID: domain.NewProjectID(),
	}
	tenant := domain.Tenant{
		ID:           f.tenantID,
		HomeRegion:   "us-east-1",
		SecurityTier: domain.SecurityTierStandard,
		Status:       domain.TenantActive,
		DisplayName:  "Test tenant",
		CreatedAt:    testNow,
		UpdatedAt:    testNow,
	}
	if err := f.store.CreateTenant(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return f
}

func (f *fixture) createRun(t *testing.T, status domain.RunStatus, at time.Time, key string) domain.Run {
	t.Helper()
	run := domain.Run{
		ID:              domain.NewRunID(),
		TenantID:        f.tenantID,
		ProjectID:       f.projectID,
		RequestKey:      key,
		Fingerprint:     "fingerprint-" + key,
		BaseCommit:      "0123456",
		HeadCommit:      "abcdef1",
		PolicyDigest:    "sha256:" + strings.Repeat("a", 64),
		PolicyRef:       "policy:test",
		RequestedChecks: []string{"compile"},
		Status:          status,
		Region:          "us-east-1",
		CreatedBy:       "tester",
		CreatedAt:       at,
		UpdatedAt:       at,
	}
	created, err := f.store.CreateRun(context.Background(), domain.CreateRunRecord{Run: run})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return created
}

func (f *fixture) plan(t *testing.T, run domain.Run, steps []domain.Step, deps map[domain.StepID][]domain.StepID, claims []domain.Claim, at time.Time) domain.Run {
	t.Helper()
	planned, err := f.store.PlanRun(context.Background(), domain.PlanTransaction{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: run.StateVersion,
		Steps:           steps,
		Dependencies:    deps,
		Claims:          claims,
		At:              at,
	})
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	return planned
}

func (f *fixture) reloadStep(t *testing.T, step domain.Step) domain.Step {
	t.Helper()
	stored, err := f.store.GetStep(context.Background(), step.TenantID, step.ID)
	if err != nil {
		t.Fatalf("get step %s: %v", step.ID, err)
	}
	return stored
}

func (f *fixture) lease(t *testing.T, run domain.Run, step domain.Step, generation int64, at time.Time) domain.Lease {
	t.Helper()
	lease, err := f.store.AcquireLease(context.Background(), domain.LeaseRequest{
		TenantID:           run.TenantID,
		RunID:              run.ID,
		StepID:             step.ID,
		AttemptID:          domain.NewAttemptID(),
		ExpectedGeneration: generation,
		Owner:              "worker-1",
		TTL:                30 * time.Second,
		ClusterID:          "cluster-a",
		Now:                at,
	})
	if err != nil {
		t.Fatalf("acquire lease for %s: %v", step.ID, err)
	}
	return lease
}

func (f *fixture) start(t *testing.T, lease domain.Lease, at time.Time) {
	t.Helper()
	if err := f.store.StartAttempt(context.Background(), lease.Ref(), at); err != nil {
		t.Fatalf("start attempt: %v", err)
	}
}

func (f *fixture) commitSuccess(t *testing.T, lease domain.Lease, at time.Time, metrics domain.StepMetrics) domain.CommitOutcome {
	t.Helper()
	outcome, err := f.store.CommitAttempt(context.Background(), domain.AttemptCommit{
		Ref: lease.Ref(),
		Result: domain.StepResult{
			Outcome:        domain.OutcomeSucceeded,
			ResultDigest:   "sha256:" + strings.Repeat("c", 64),
			OutputManifest: "sha256:" + strings.Repeat("d", 64),
			Metrics:        metrics,
			FinishedAt:     at,
		},
		At: at,
	})
	if err != nil {
		t.Fatalf("commit attempt: %v", err)
	}
	return outcome
}

func newStep(run domain.Run, key string, kind domain.StepKind, status domain.StepStatus, maxAttempts, shard, priority int, at time.Time) domain.Step {
	return domain.Step{
		ID:                  domain.NewStepID(),
		RunID:               run.ID,
		TenantID:            run.TenantID,
		Key:                 key,
		Kind:                kind,
		Status:              status,
		MaxAttempts:         maxAttempts,
		Shard:               shard,
		Priority:            priority,
		InputManifestDigest: "sha256:" + strings.Repeat("e", 64),
		Options:             map[string]string{"timeout": "45s"},
		Requirements: domain.ResourceRequirements{
			Region:            "us-east-1",
			Architecture:      "x86_64",
			Isolation:         domain.IsolationContainer,
			CPUMilli:          1000,
			MemoryMiB:         512,
			Network:           domain.NetworkNone,
			DeadlineSeconds:   60,
			ArtifactTransport: domain.TransportDirect,
			RequiredFeatures:  []string{"feature-a"},
		},
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func planChain(t *testing.T, f *fixture, run domain.Run, at time.Time) (domain.Run, domain.Step, domain.Step) {
	t.Helper()
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 5, at)
	dependent := newStep(run, "regression", domain.StepKindRegression, domain.StepBlocked, 3, 0, 1, at)
	deps := map[domain.StepID][]domain.StepID{dependent.ID: {root.ID}}
	planned := f.plan(t, run, []domain.Step{root, dependent}, deps, nil, at)
	return planned, f.reloadStep(t, root), f.reloadStep(t, dependent)
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "lifecycle")
	run, root, dependent := planChain(t, f, run, testNow)

	if run.Status != domain.RunPlanned || run.PlannedAt == nil {
		t.Fatalf("expected planned run with planned_at, got %s %v", run.Status, run.PlannedAt)
	}
	if run.StateVersion != 1 {
		t.Fatalf("expected state version 1 after planning, got %d", run.StateVersion)
	}

	dag, err := f.store.GetDAG(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get dag: %v", err)
	}
	if len(dag.Steps) != 2 {
		t.Fatalf("expected 2 dag steps, got %d", len(dag.Steps))
	}
	if got := dag.Dependencies[dependent.ID]; len(got) != 1 || got[0] != root.ID {
		t.Fatalf("unexpected dependency map: %v", dag.Dependencies)
	}

	ready, err := f.store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: run.TenantID, Now: testNow})
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != root.ID {
		t.Fatalf("expected only root READY, got %+v", ready)
	}

	lease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	if lease.Generation != 1 || lease.AttemptID == "" || lease.Owner != "worker-1" {
		t.Fatalf("unexpected lease: %+v", lease)
	}
	if got := f.reloadStep(t, root); got.Status != domain.StepLeased || got.AttemptCount != 1 {
		t.Fatalf("expected LEASED with attempt_count 1, got %s/%d", got.Status, got.AttemptCount)
	}
	if got := f.reloadStep(t, root); got.Options["timeout"] != "45s" {
		t.Fatalf("step options were not preserved through leasing: %v", got.Options)
	}
	f.start(t, lease, testNow.Add(2*time.Second))
	if got := f.reloadStep(t, root); got.Status != domain.StepRunning {
		t.Fatalf("expected RUNNING, got %s", got.Status)
	}

	outcome := f.commitSuccess(t, lease, testNow.Add(3*time.Second), domain.StepMetrics{CPUSeconds: 1.5})
	if outcome.Step.Status != domain.StepSucceeded {
		t.Fatalf("expected SUCCEEDED, got %s", outcome.Step.Status)
	}
	if outcome.Attempt.Status != domain.AttemptReported {
		t.Fatalf("expected REPORTED, got %s", outcome.Attempt.Status)
	}
	if outcome.AllTerminal {
		t.Fatal("run must not be terminal while the dependent step is pending")
	}
	if len(outcome.Promoted) != 1 || outcome.Promoted[0].ID != dependent.ID || outcome.Promoted[0].Status != domain.StepReady {
		t.Fatalf("expected dependent promoted to READY, got %+v", outcome.Promoted)
	}

	lease = f.lease(t, run, dependent, 0, testNow.Add(4*time.Second))
	f.start(t, lease, testNow.Add(5*time.Second))
	outcome = f.commitSuccess(t, lease, testNow.Add(6*time.Second), domain.StepMetrics{CPUSeconds: 2.5})
	if !outcome.AllTerminal {
		t.Fatal("expected all steps terminal")
	}
	if outcome.RunStatus != domain.RunPlanned {
		t.Fatalf("commit must not advance run status, got %s", outcome.RunStatus)
	}

	steps, err := f.store.ListSteps(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	for _, step := range steps {
		if step.Status != domain.StepSucceeded {
			t.Fatalf("expected all steps succeeded, got %s", step.Status)
		}
	}
	ready, err = f.store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: run.TenantID, Now: testNow.Add(time.Hour)})
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("expected no ready steps, got %+v", ready)
	}

	pending, err := f.store.PendingOutbox(ctx, 50)
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	counts := map[domain.EventType]int{}
	for _, event := range pending {
		counts[event.Type]++
		if event.RunID != run.ID {
			t.Fatalf("event %s missing run id", event.Type)
		}
	}
	if counts[domain.EventStepLeased] != 2 || counts[domain.EventStepSucceeded] != 2 || counts[domain.EventStepReady] != 1 {
		t.Fatalf("unexpected event counts: %v", counts)
	}
}

func TestConcurrentLeaseExactlyOneWinner(t *testing.T) {
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "concurrent")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)

	const attempts = 16
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	winners := make(chan domain.Lease, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, err := f.store.AcquireLease(context.Background(), domain.LeaseRequest{
				TenantID:           run.TenantID,
				RunID:              run.ID,
				StepID:             root.ID,
				AttemptID:          domain.NewAttemptID(),
				ExpectedGeneration: 0,
				Owner:              fmt.Sprintf("worker-%d", i),
				TTL:                30 * time.Second,
				Now:                testNow.Add(time.Second),
			})
			if err != nil {
				results <- err
				return
			}
			winners <- lease
		}(i)
	}
	wg.Wait()
	close(results)
	close(winners)

	won := 0
	for range winners {
		won++
	}
	if won != 1 {
		t.Fatalf("expected exactly one lease winner, got %d", won)
	}
	for err := range results {
		var leaseErr *domain.LeaseError
		if !errors.As(err, &leaseErr) {
			t.Fatalf("expected LeaseError for losers, got %T: %v", err, err)
		}
	}
	step := f.reloadStep(t, root)
	if step.Generation != 1 || step.AttemptCount != 1 {
		t.Fatalf("expected one generation and one attempt, got %d/%d", step.Generation, step.AttemptCount)
	}
	attemptsList, err := f.store.ListAttempts(context.Background(), run.TenantID, root.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attemptsList) != 1 {
		t.Fatalf("expected one attempt, got %d", len(attemptsList))
	}
}

func TestCreateRunIdempotency(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	first := f.createRun(t, domain.RunValidating, testNow, "same-key")

	duplicate := first
	duplicate.ID = domain.NewRunID()
	duplicate.CreatedAt = testNow.Add(time.Hour)
	got, err := f.store.CreateRun(ctx, domain.CreateRunRecord{Run: duplicate})
	if err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if got.ID != first.ID {
		t.Fatalf("expected existing run %s, got %s", first.ID, got.ID)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("idempotent create must not rewrite the stored run")
	}

	conflict := duplicate
	conflict.Fingerprint = "another-fingerprint"
	_, err = f.store.CreateRun(ctx, domain.CreateRunRecord{Run: conflict})
	var conflictErr *domain.IdempotencyConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected IdempotencyConflictError, got %T: %v", err, err)
	}
}

func TestUpdateRunOptimisticVersion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunCreated, testNow, "version")

	updated, err := f.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: 0,
		From:            domain.RunCreated,
		To:              domain.RunValidating,
		At:              testNow.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("update run: %v", err)
	}
	if updated.Status != domain.RunValidating || updated.StateVersion != 1 {
		t.Fatalf("expected VALIDATING at version 1, got %s/%d", updated.Status, updated.StateVersion)
	}

	_, err = f.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: 0,
		From:            domain.RunValidating,
		To:              domain.RunPlanned,
		At:              testNow.Add(2 * time.Second),
	})
	var versionErr *domain.VersionConflictError
	if !errors.As(err, &versionErr) {
		t.Fatalf("expected VersionConflictError, got %T: %v", err, err)
	}

	_, err = f.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: 1,
		From:            domain.RunCreated,
		To:              domain.RunValidating,
		At:              testNow.Add(2 * time.Second),
	})
	var transitionErr *domain.StateTransitionError
	if !errors.As(err, &transitionErr) {
		t.Fatalf("expected StateTransitionError, got %T: %v", err, err)
	}

	missing := domain.NewRunID()
	_, err = f.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID:        run.TenantID,
		RunID:           missing,
		ExpectedVersion: 0,
		From:            domain.RunCreated,
		To:              domain.RunValidating,
		At:              testNow,
	})
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
}

func TestFreezeRunInputs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunCreated, testNow, "freeze")

	frozen, err := f.store.FreezeRunInputs(ctx, domain.RunFreeze{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: 0,
		SnapshotDigest:  "sha256:snapshot",
		BaseSnapshot:    "sha256:base",
		HeadSnapshot:    "sha256:head",
		At:              testNow.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("freeze inputs: %v", err)
	}
	if frozen.SnapshotDigest != "sha256:snapshot" || frozen.HeadSnapshot != "sha256:head" {
		t.Fatalf("freeze did not persist inputs: %+v", frozen)
	}
	if frozen.StateVersion != 1 {
		t.Fatalf("expected freeze to advance version, got %d", frozen.StateVersion)
	}

	_, err = f.store.FreezeRunInputs(ctx, domain.RunFreeze{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: 0,
		SnapshotDigest:  "sha256:snapshot",
		At:              testNow.Add(2 * time.Second),
	})
	var versionErr *domain.VersionConflictError
	if !errors.As(err, &versionErr) {
		t.Fatalf("expected VersionConflictError, got %T: %v", err, err)
	}
}

func TestFencingRejectsStaleAndExpiredAttempts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "fencing")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)
	root = f.reloadStep(t, root)

	firstLease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	f.start(t, firstLease, testNow.Add(2*time.Second))

	expiryAt := testNow.Add(time.Second + 30*time.Second)
	expiries, err := f.store.ExpireLeases(ctx, expiryAt, 10)
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if len(expiries) != 1 || expiries[0].AttemptID != firstLease.AttemptID || expiries[0].Final {
		t.Fatalf("unexpected expiry: %+v", expiries)
	}

	_, err = f.store.CommitAttempt(ctx, domain.AttemptCommit{
		Ref:    firstLease.Ref(),
		Result: domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:late", FinishedAt: expiryAt},
		At:     expiryAt,
	})
	var fenceErr *domain.FencingError
	if !errors.As(err, &fenceErr) {
		t.Fatalf("expected FencingError for late commit, got %T: %v", err, err)
	}

	step := f.reloadStep(t, root)
	if step.Status != domain.StepRetryWait || step.NextAttemptAt == nil {
		t.Fatalf("expected RETRY_WAIT with next attempt, got %s %v", step.Status, step.NextAttemptAt)
	}
	nextAt := *step.NextAttemptAt
	promoted, err := f.store.PromoteDueSteps(ctx, nextAt.Add(-time.Second), 10)
	if err != nil {
		t.Fatalf("promote due: %v", err)
	}
	if len(promoted) != 0 {
		t.Fatalf("step must not promote before backoff, got %+v", promoted)
	}
	promoted, err = f.store.PromoteDueSteps(ctx, nextAt, 10)
	if err != nil {
		t.Fatalf("promote due: %v", err)
	}
	if len(promoted) != 1 || promoted[0].Status != domain.StepReady {
		t.Fatalf("expected promotion to READY, got %+v", promoted)
	}

	secondLease := f.lease(t, run, root, 1, nextAt.Add(time.Second))
	if secondLease.Generation != 2 {
		t.Fatalf("expected generation 2, got %d", secondLease.Generation)
	}

	_, err = f.store.CommitAttempt(ctx, domain.AttemptCommit{
		Ref:    firstLease.Ref(),
		Result: domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:stale", FinishedAt: nextAt.Add(2 * time.Second)},
		At:     nextAt.Add(2 * time.Second),
	})
	if !errors.As(err, &fenceErr) {
		t.Fatalf("expected stale generation to be fenced, got %T: %v", err, err)
	}

	f.start(t, secondLease, nextAt.Add(2*time.Second))
	outcome := f.commitSuccess(t, secondLease, nextAt.Add(3*time.Second), domain.StepMetrics{})
	if outcome.Step.Status != domain.StepSucceeded || !outcome.AllTerminal {
		t.Fatalf("expected successful terminal step, got %s all=%v", outcome.Step.Status, outcome.AllTerminal)
	}
}

func TestRetryBackoffThenSuccess(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "retry")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)
	root = f.reloadStep(t, root)

	lease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	f.start(t, lease, testNow.Add(2*time.Second))
	failAt := testNow.Add(3 * time.Second)
	outcome, err := f.store.FailAttempt(ctx, domain.AttemptFailure{
		Ref:        lease.Ref(),
		ErrorClass: domain.ErrExecutorError,
		Message:    "executor crashed",
		Retryable:  true,
		At:         failAt,
	})
	if err != nil {
		t.Fatalf("fail attempt: %v", err)
	}
	if outcome.Step.Status != domain.StepRetryWait {
		t.Fatalf("expected RETRY_WAIT, got %s", outcome.Step.Status)
	}
	if outcome.Attempt.Status != domain.AttemptFailedInfra {
		t.Fatalf("expected FAILED_INFRA, got %s", outcome.Attempt.Status)
	}
	wantNext := failAt.Add(domain.RetryDelay(1))
	if outcome.Step.NextAttemptAt == nil || !outcome.Step.NextAttemptAt.Equal(wantNext) {
		t.Fatalf("expected backoff until %s, got %v", wantNext, outcome.Step.NextAttemptAt)
	}

	promoted, err := f.store.PromoteDueSteps(ctx, wantNext, 10)
	if err != nil {
		t.Fatalf("promote due: %v", err)
	}
	if len(promoted) != 1 {
		t.Fatalf("expected one promoted step, got %d", len(promoted))
	}
	lease = f.lease(t, run, promoted[0], 1, wantNext.Add(time.Second))
	f.start(t, lease, wantNext.Add(2*time.Second))
	outcome = f.commitSuccess(t, lease, wantNext.Add(3*time.Second), domain.StepMetrics{})
	if outcome.Step.Status != domain.StepSucceeded || outcome.Attempt.Generation != 2 {
		t.Fatalf("unexpected retry outcome: %+v", outcome)
	}
}

func TestExpireLeaseFinalFailureAtMaxAttempts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "expire-final")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 2, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)
	root = f.reloadStep(t, root)

	firstLease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	firstExpiryAt := testNow.Add(time.Second + 30*time.Second)
	expiries, err := f.store.ExpireLeases(ctx, firstExpiryAt, 10)
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if len(expiries) != 1 || expiries[0].Final {
		t.Fatalf("expected non-final expiry, got %+v", expiries)
	}
	step := f.reloadStep(t, root)
	if step.Status != domain.StepRetryWait || step.NextAttemptAt == nil {
		t.Fatalf("expected retry wait after first expiry, got %s", step.Status)
	}
	promoted, err := f.store.PromoteDueSteps(ctx, *step.NextAttemptAt, 10)
	if err != nil {
		t.Fatalf("promote due: %v", err)
	}
	secondLease := f.lease(t, run, promoted[0], 1, step.NextAttemptAt.Add(time.Second))
	secondExpiryAt := step.NextAttemptAt.Add(time.Second + 30*time.Second)
	expiries, err = f.store.ExpireLeases(ctx, secondExpiryAt, 10)
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if len(expiries) != 1 || !expiries[0].Final || expiries[0].AttemptID != secondLease.AttemptID {
		t.Fatalf("expected final expiry for second attempt, got %+v", expiries)
	}
	step = f.reloadStep(t, root)
	if step.Status != domain.StepFailed || step.FailureClass != domain.ErrLeaseExpired {
		t.Fatalf("expected final LEASE_EXPIRED failure, got %s/%s", step.Status, step.FailureClass)
	}
	if step.FinishedAt == nil {
		t.Fatal("final failure must set finished_at")
	}
	attemptsList, err := f.store.ListAttempts(ctx, run.TenantID, root.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attemptsList) != 2 {
		t.Fatalf("expected two attempts, got %d", len(attemptsList))
	}
	for _, attempt := range attemptsList {
		if attempt.Status != domain.AttemptExpired {
			t.Fatalf("expected EXPIRED attempts, got %s", attempt.Status)
		}
	}
	_ = firstLease
}

func TestOutboxDeliveryLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "outbox")

	older, err := domain.NewOutboxEvent(run.TenantID, domain.EventRunCreated, run.ID.String(), 0, map[string]string{"run_id": run.ID.String()}, testNow)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	older.RunID = run.ID
	newer, err := domain.NewOutboxEvent(run.TenantID, domain.EventRunValidated, run.ID.String(), 0, map[string]string{"run_id": run.ID.String()}, testNow.Add(time.Second))
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	newer.RunID = run.ID
	if err := f.store.AppendOutbox(ctx, []domain.OutboxEvent{newer, older}); err != nil {
		t.Fatalf("append outbox: %v", err)
	}

	pending, err := f.store.PendingOutbox(ctx, 1)
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != older.ID {
		t.Fatalf("expected oldest event first, got %+v", pending)
	}
	if err := f.store.RecordOutboxFailure(ctx, domain.OutboxDeliveryFailure{EventID: older.ID, Reason: "broker down", At: testNow}); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	pending, err = f.store.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	for _, event := range pending {
		if event.ID == older.ID && event.Attempts != 1 {
			t.Fatalf("expected one delivery attempt, got %d", event.Attempts)
		}
	}
	if err := f.store.MarkOutboxDelivered(ctx, []domain.EventID{older.ID, newer.ID}, testNow.Add(2*time.Second)); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if err := f.store.MarkOutboxDelivered(ctx, []domain.EventID{older.ID}, testNow.Add(3*time.Second)); err != nil {
		t.Fatalf("mark delivered twice: %v", err)
	}
	pending, err = f.store.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending events, got %+v", pending)
	}
	if err := f.store.AppendOutbox(ctx, []domain.OutboxEvent{older}); err == nil {
		t.Fatal("expected duplicate event id to be rejected")
	}
	if err := f.store.RecordOutboxFailure(ctx, domain.OutboxDeliveryFailure{EventID: domain.NewEventID()}); err == nil {
		t.Fatal("expected missing event failure to be rejected")
	}
	invalid := older
	invalid.ID = ""
	if err := f.store.AppendOutbox(ctx, []domain.OutboxEvent{invalid}); err == nil {
		t.Fatal("expected invalid event to be rejected")
	}
}

func TestCancelRunSteps(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "cancel")
	run, root, dependent := planChain(t, f, run, testNow)

	lease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	f.start(t, lease, testNow.Add(2*time.Second))
	cancelled, err := f.store.CancelRunSteps(ctx, run.TenantID, run.ID, testNow.Add(3*time.Second))
	if err != nil {
		t.Fatalf("cancel run steps: %v", err)
	}
	if cancelled != 2 {
		t.Fatalf("expected 2 cancelled steps, got %d", cancelled)
	}
	for _, step := range []domain.Step{root, dependent} {
		stored := f.reloadStep(t, step)
		if stored.Status != domain.StepCancelled || stored.FinishedAt == nil {
			t.Fatalf("expected cancelled step, got %s", stored.Status)
		}
		if stored.CurrentAttemptID != "" || stored.LeaseDeadline != nil {
			t.Fatalf("expected lease fields cleared, got %+v", stored)
		}
	}
	attemptsList, err := f.store.ListAttempts(ctx, run.TenantID, root.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attemptsList) != 1 || attemptsList[0].Status != domain.AttemptCancelled {
		t.Fatalf("expected cancelled attempt, got %+v", attemptsList)
	}
	active, err := f.store.CountActiveSteps(ctx, run.TenantID)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Fatalf("expected no active steps, got %d", active)
	}
	second, err := f.store.CancelRunSteps(ctx, run.TenantID, run.ID, testNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("cancel again: %v", err)
	}
	if second != 0 {
		t.Fatalf("expected no double cancellation, got %d", second)
	}
}

func TestDeepCopyIsolation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "deep-copy")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)

	got, err := f.store.GetRun(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	got.RequestedChecks[0] = "mutated"
	got.Status = domain.RunCancelled
	got.UpdatedAt = testNow.Add(time.Hour)
	stored, err := f.store.GetRun(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.RequestedChecks[0] != "compile" || stored.Status != domain.RunPlanned {
		t.Fatalf("run was mutated through a returned value: %+v", stored)
	}

	step, err := f.store.GetStep(ctx, run.TenantID, root.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	step.Requirements.RequiredFeatures[0] = "mutated"
	step.Requirements.CPUMilli = 99
	step.Options["timeout"] = "mutated"
	step.Status = domain.StepSucceeded
	storedStep := f.reloadStep(t, root)
	if storedStep.Requirements.RequiredFeatures[0] != "feature-a" || storedStep.Requirements.CPUMilli != 1000 || storedStep.Status != domain.StepReady {
		t.Fatalf("step was mutated through a returned value: %+v", storedStep)
	}
	if storedStep.Options["timeout"] != "45s" {
		t.Fatalf("step options were mutated through a returned value: %v", storedStep.Options)
	}

	dag, err := f.store.GetDAG(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get dag: %v", err)
	}
	if dag.Steps[0].Options["timeout"] != "45s" {
		t.Fatalf("expected options in DAG, got %v", dag.Steps[0].Options)
	}
	dag.Steps[0].Status = domain.StepSucceeded
	dag.Steps[0].Options["timeout"] = "mutated"
	dag.Dependencies[root.ID] = []domain.StepID{root.ID}
	dag, err = f.store.GetDAG(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get dag: %v", err)
	}
	if dag.Steps[0].Status != domain.StepReady {
		t.Fatalf("dag step was mutated: %+v", dag.Steps[0])
	}
	if dag.Steps[0].Options["timeout"] != "45s" {
		t.Fatalf("dag step options were mutated: %v", dag.Steps[0].Options)
	}
	if len(dag.Dependencies[root.ID]) != 0 {
		t.Fatalf("dag dependencies were mutated: %+v", dag.Dependencies)
	}

	page, err := f.store.ListRuns(ctx, domain.RunFilter{TenantID: run.TenantID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	page.Runs[0].RequestedChecks[0] = "mutated"
	page.Runs[0].Status = domain.RunCancelled
	again, err := f.store.ListRuns(ctx, domain.RunFilter{TenantID: run.TenantID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if again.Runs[0].RequestedChecks[0] != "compile" || again.Runs[0].Status != domain.RunPlanned {
		t.Fatalf("list result was mutated: %+v", again.Runs[0])
	}
}

func TestListRunsCursorPagination(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var created []domain.Run
	for i := 0; i < 5; i++ {
		created = append(created, f.createRun(t, domain.RunCreated, testNow.Add(time.Duration(i)*time.Minute), fmt.Sprintf("page-%d", i)))
	}

	seen := map[domain.RunID]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := f.store.ListRuns(ctx, domain.RunFilter{
			TenantID: f.tenantID,
			Limit:    2,
			Cursor:   cursor,
		})
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		pages++
		for _, run := range page.Runs {
			if seen[run.ID] {
				t.Fatalf("run %s returned twice", run.ID)
			}
			seen[run.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(created) {
		t.Fatalf("expected %d runs across pages, got %d", len(created), len(seen))
	}

	firstPage, err := f.store.ListRuns(ctx, domain.RunFilter{TenantID: f.tenantID, Limit: 2})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(firstPage.Runs) != 2 || firstPage.Runs[0].ID != created[4].ID || firstPage.Runs[1].ID != created[3].ID {
		t.Fatalf("unexpected newest-first order: %+v", firstPage.Runs)
	}

	_, err = f.store.ListRuns(ctx, domain.RunFilter{TenantID: f.tenantID, Cursor: "not-base64!"})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected ValidationError for bad cursor, got %T: %v", err, err)
	}

	if _, err := f.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID:        created[0].TenantID,
		RunID:           created[0].ID,
		ExpectedVersion: 0,
		From:            domain.RunCreated,
		To:              domain.RunValidating,
		At:              testNow.Add(time.Hour),
	}); err != nil {
		t.Fatalf("update run: %v", err)
	}
	filtered, err := f.store.ListRuns(ctx, domain.RunFilter{TenantID: f.tenantID, Statuses: []domain.RunStatus{domain.RunValidating}})
	if err != nil {
		t.Fatalf("list runs by status: %v", err)
	}
	if len(filtered.Runs) != 1 || filtered.Runs[0].ID != created[0].ID {
		t.Fatalf("unexpected status filter result: %+v", filtered.Runs)
	}
}

func TestTenantUsage(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "usage")
	fuzz := newStep(run, "differential", domain.StepKindDifferential, domain.StepReady, 3, 0, 2, testNow)
	compile := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{fuzz, compile}, nil, nil, testNow)

	active, err := f.store.CountActiveSteps(ctx, run.TenantID)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Fatalf("expected queued READY steps not to consume capacity, got %d", active)
	}

	fuzzLease := f.lease(t, run, fuzz, 0, testNow.Add(time.Second))
	compileLease := f.lease(t, run, compile, 0, testNow.Add(2*time.Second))
	active, err = f.store.CountActiveSteps(ctx, run.TenantID)
	if err != nil {
		t.Fatalf("count active after lease: %v", err)
	}
	if active != 2 {
		t.Fatalf("expected 2 leased steps, got %d", active)
	}
	f.commitSuccess(t, fuzzLease, testNow.Add(2*time.Second), domain.StepMetrics{CPUSeconds: 2.5})
	f.commitSuccess(t, compileLease, testNow.Add(4*time.Second), domain.StepMetrics{CPUSeconds: 1.5})

	usage, err := f.store.TenantUsage(ctx, run.TenantID, testNow)
	if err != nil {
		t.Fatalf("tenant usage: %v", err)
	}
	if usage.ActiveRuns != 1 || usage.ActiveSteps != 0 {
		t.Fatalf("unexpected active counts: %+v", usage)
	}
	if usage.DailyCPUSeconds != 4 || usage.DailyFuzzSeconds != 2.5 {
		t.Fatalf("unexpected metric sums: %+v", usage)
	}

	usage, err = f.store.TenantUsage(ctx, run.TenantID, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("tenant usage: %v", err)
	}
	if usage.DailyCPUSeconds != 0 || usage.DailyFuzzSeconds != 0 {
		t.Fatalf("expected no usage after the window, got %+v", usage)
	}
}

func TestEvidenceAndReport(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "evidence")
	run, root, _ := planChain(t, f, run, testNow)

	evidence := testEvidence(run, root.ID, testNow.Add(time.Second))
	second := testEvidence(run, root.ID, testNow.Add(2*time.Second))
	if err := f.store.AppendEvidence(ctx, []domain.Evidence{second, evidence}); err != nil {
		t.Fatalf("append evidence: %v", err)
	}
	evidenceList, err := f.store.ListEvidence(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(evidenceList) != 2 || !evidenceList[0].CreatedAt.Equal(testNow.Add(time.Second)) {
		t.Fatalf("expected evidence ordered by created_at, got %+v", evidenceList)
	}
	evidenceList[0].Limits = append(evidenceList[0].Limits, "mutated")
	evidenceList, err = f.store.ListEvidence(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(evidenceList[0].Limits) != 0 {
		t.Fatal("evidence was mutated through a returned value")
	}
	if err := f.store.AppendEvidence(ctx, []domain.Evidence{evidence}); err == nil {
		t.Fatal("expected duplicate evidence to be rejected")
	}
	if err := f.store.AppendEvidence(ctx, []domain.Evidence{testEvidence(run, root.ID, testNow)}); err != nil {
		t.Fatalf("append more evidence: %v", err)
	}

	report := testReport(run, testNow.Add(time.Second))
	if err := f.store.PutReport(ctx, report); err != nil {
		t.Fatalf("put report: %v", err)
	}
	stored, err := f.store.GetReport(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if stored.ReportID != report.ReportID {
		t.Fatalf("unexpected report: %+v", stored)
	}
	stored.Claims[0].Result = domain.ClaimViolated
	stored.Limitations = append(stored.Limitations, "mutated")
	stored, err = f.store.GetReport(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if stored.Claims[0].Result != domain.ClaimTested || len(stored.Limitations) != 0 {
		t.Fatal("report was mutated through a returned value")
	}

	updated := report
	updated.ReportID = domain.NewReportID()
	if err := f.store.PutReport(ctx, updated); err != nil {
		t.Fatalf("upsert report: %v", err)
	}
	stored, err = f.store.GetReport(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if stored.ReportID != updated.ReportID {
		t.Fatal("report upsert did not replace the stored report")
	}
}

func TestRenewLeaseExtendsDeadline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "renew")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)
	root = f.reloadStep(t, root)

	lease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	renewAt := testNow.Add(2 * time.Second)
	if err := f.store.RenewLease(ctx, domain.LeaseRenewal{
		Ref:       lease.Ref(),
		Owner:     "another-worker",
		Extension: time.Minute,
		Now:       renewAt,
	}); err == nil {
		t.Fatal("expected renewal by a non-owner to fail")
	}
	if err := f.store.RenewLease(ctx, domain.LeaseRenewal{
		Ref:       lease.Ref(),
		Owner:     lease.Owner,
		Extension: time.Minute,
		Now:       renewAt,
	}); err != nil {
		t.Fatalf("renew lease: %v", err)
	}
	extended := renewAt.Add(time.Minute)
	step := f.reloadStep(t, root)
	if step.LeaseDeadline == nil || !step.LeaseDeadline.Equal(extended) {
		t.Fatalf("expected deadline %s, got %v", extended, step.LeaseDeadline)
	}

	f.start(t, lease, testNow.Add(3*time.Second))
	// The lease remains usable past its original TTL after renewal.
	f.commitSuccess(t, lease, testNow.Add(40*time.Second), domain.StepMetrics{})
	if err := f.store.RenewLease(ctx, domain.LeaseRenewal{
		Ref:       lease.Ref(),
		Owner:     lease.Owner,
		Extension: time.Minute,
		Now:       extended.Add(time.Second),
	}); err == nil {
		t.Fatal("expected renewal after completion to fail")
	}
}

func TestPlanRunValidationIsAtomic(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "plan-atomic")
	first := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	second := newStep(run, "regression", domain.StepKindRegression, domain.StepReady, 3, 0, 1, testNow)
	cyclic := map[domain.StepID][]domain.StepID{
		first.ID:  {second.ID},
		second.ID: {first.ID},
	}
	_, err := f.store.PlanRun(ctx, domain.PlanTransaction{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: 0,
		Steps:           []domain.Step{first, second},
		Dependencies:    cyclic,
		At:              testNow,
	})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected ValidationError for a cycle, got %T: %v", err, err)
	}
	stored, err := f.store.GetRun(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != domain.RunValidating || stored.StateVersion != 0 || stored.PlannedAt != nil {
		t.Fatalf("failed plan must not mutate the run: %+v", stored)
	}
	steps, err := f.store.ListSteps(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("failed plan must not insert steps, got %d", len(steps))
	}

	created := f.createRun(t, domain.RunCreated, testNow.Add(time.Minute), "plan-from-created")
	_, err = f.store.PlanRun(ctx, domain.PlanTransaction{
		TenantID:        created.TenantID,
		RunID:           created.ID,
		ExpectedVersion: 0,
		Steps:           []domain.Step{newStep(created, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)},
		At:              testNow,
	})
	var transitionErr *domain.StateTransitionError
	if !errors.As(err, &transitionErr) {
		t.Fatalf("expected StateTransitionError when planning a CREATED run, got %T: %v", err, err)
	}
}

func TestClusterAndShardLeases(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	clusterA := domain.Cluster{ID: "cluster-b", Region: "us-east-1", ExecutorKind: "k8s", Architectures: []string{"x86_64"}, Languages: []string{"go"}, IsolationTiers: []domain.IsolationTier{domain.IsolationContainer}, Labels: map[string]string{"tier": "a"}, LastHeartbeat: testNow}
	clusterB := domain.Cluster{ID: "cluster-a", Region: "eu-west-1", ExecutorKind: "k8s", Architectures: []string{"arm64"}}
	if err := f.store.UpsertCluster(ctx, clusterA); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}
	if err := f.store.UpsertCluster(ctx, clusterB); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}
	all, err := f.store.ListClusters(ctx, "")
	if err != nil {
		t.Fatalf("list clusters: %v", err)
	}
	if len(all) != 2 || all[0].ID != clusterB.ID || all[1].ID != clusterA.ID {
		t.Fatalf("expected clusters sorted by id, got %+v", all)
	}
	all[1].Labels["tier"] = "mutated"
	stored, err := f.store.GetCluster(ctx, clusterA.ID)
	if err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	if stored.Labels["tier"] != "a" {
		t.Fatal("cluster labels were mutated through a returned value")
	}
	east, err := f.store.ListClusters(ctx, "us-east-1")
	if err != nil {
		t.Fatalf("list clusters by region: %v", err)
	}
	if len(east) != 1 || east[0].ID != clusterA.ID {
		t.Fatalf("unexpected region filter: %+v", east)
	}
	if err := f.store.DeleteCluster(ctx, clusterA.ID); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
	if _, err := f.store.GetCluster(ctx, clusterA.ID); err == nil {
		t.Fatal("expected deleted cluster to be missing")
	}

	now := testNow
	first, err := f.store.AcquireShardLeases(ctx, []int{2, 1, 2}, "owner-a", 10*time.Second, now)
	if err != nil {
		t.Fatalf("acquire shard leases: %v", err)
	}
	if len(first) != 2 || first[0] != 1 || first[1] != 2 {
		t.Fatalf("expected sorted unique shards [1 2], got %v", first)
	}
	blocked, err := f.store.AcquireShardLeases(ctx, []int{1, 2, 3}, "owner-b", 10*time.Second, now.Add(time.Second))
	if err != nil {
		t.Fatalf("acquire shard leases: %v", err)
	}
	if len(blocked) != 1 || blocked[0] != 3 {
		t.Fatalf("expected only shard 3 for the second owner, got %v", blocked)
	}
	afterExpiry, err := f.store.AcquireShardLeases(ctx, []int{1, 2}, "owner-b", 10*time.Second, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("acquire expired shard leases: %v", err)
	}
	if len(afterExpiry) != 2 {
		t.Fatalf("expected expired leases to be reclaimable, got %v", afterExpiry)
	}
	if err := f.store.ReleaseShardLeases(ctx, "owner-b"); err != nil {
		t.Fatalf("release shard leases: %v", err)
	}
	released, err := f.store.AcquireShardLeases(ctx, []int{1, 2}, "owner-c", 10*time.Second, now.Add(11*time.Second))
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if len(released) != 2 {
		t.Fatalf("expected released shards to be claimable, got %v", released)
	}
}

func TestLeaseRejections(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "rejections")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	run = f.plan(t, run, []domain.Step{root}, nil, nil, testNow)
	root = f.reloadStep(t, root)

	_, err := f.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID:           run.TenantID,
		RunID:              run.ID,
		StepID:             root.ID,
		AttemptID:          domain.NewAttemptID(),
		ExpectedGeneration: 5,
		Owner:              "worker",
		TTL:                30 * time.Second,
		Now:                testNow.Add(time.Second),
	})
	var leaseErr *domain.LeaseError
	if !errors.As(err, &leaseErr) {
		t.Fatalf("expected generation mismatch LeaseError, got %T: %v", err, err)
	}

	updated, err := f.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: run.StateVersion,
		From:            domain.RunPlanned,
		To:              domain.RunCancelling,
		At:              testNow.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("update run: %v", err)
	}
	if updated.Status != domain.RunCancelling {
		t.Fatalf("expected CANCELLING, got %s", updated.Status)
	}
	_, err = f.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID:           run.TenantID,
		RunID:              run.ID,
		StepID:             root.ID,
		AttemptID:          domain.NewAttemptID(),
		ExpectedGeneration: 0,
		Owner:              "worker",
		TTL:                30 * time.Second,
		Now:                testNow.Add(2 * time.Second),
	})
	if !errors.As(err, &leaseErr) {
		t.Fatalf("expected cancelled run LeaseError, got %T: %v", err, err)
	}
}

func TestListReadyFilters(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "ready-filters")
	low := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 3, 0, 1, testNow)
	high := newStep(run, "differential", domain.StepKindDifferential, domain.StepReady, 3, 1, 9, testNow)
	high2 := newStep(run, "regression", domain.StepKindRegression, domain.StepReady, 3, 2, 9, testNow)
	run = f.plan(t, run, []domain.Step{low, high, high2}, nil, nil, testNow)

	all, err := f.store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: run.TenantID, Now: testNow})
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(all) != 3 || all[0].Priority != 9 || all[1].Priority != 9 || all[2].Priority != 1 {
		t.Fatalf("expected priority DESC ordering, got %+v", all)
	}
	sharded, err := f.store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: run.TenantID, Now: testNow, Shard: 1, ShardCount: 2})
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(sharded) != 1 || sharded[0].ID != high.ID {
		t.Fatalf("unexpected shard filter result: %+v", sharded)
	}
	byKind, err := f.store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: run.TenantID, Now: testNow, Kinds: []domain.StepKind{domain.StepKindRegression}})
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(byKind) != 1 || byKind[0].ID != high2.ID {
		t.Fatalf("unexpected kind filter result: %+v", byKind)
	}
	limited, err := f.store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: run.TenantID, Now: testNow, Limit: 1})
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected one limited step, got %d", len(limited))
	}
}

func TestCloseAndZeroValueStore(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "closed")
	if err := f.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatalf("close twice: %v", err)
	}
	if err := f.store.Ping(ctx); err == nil {
		t.Fatal("expected closed store ping to fail")
	}
	if _, err := f.store.GetRun(ctx, run.TenantID, run.ID); err == nil {
		t.Fatal("expected closed store read to fail")
	}
	if err := f.store.AppendAudit(ctx, domain.AuditEntry{ID: "a", Action: "test"}); err == nil {
		t.Fatal("expected closed store write to fail")
	}

	var zero memory.Store
	zeroRun := domain.Run{
		ID:           domain.NewRunID(),
		TenantID:     domain.NewTenantID(),
		ProjectID:    domain.NewProjectID(),
		RequestKey:   "zero",
		Fingerprint:  "fp-zero",
		BaseCommit:   "0123456",
		HeadCommit:   "abcdef1",
		PolicyDigest: "sha256:zero",
		Status:       domain.RunValidating,
	}
	if _, err := zero.CreateRun(ctx, domain.CreateRunRecord{Run: zeroRun}); err != nil {
		t.Fatalf("zero value store create run: %v", err)
	}
	if _, err := zero.GetRun(ctx, zeroRun.TenantID, zeroRun.ID); err != nil {
		t.Fatalf("zero value store get run: %v", err)
	}
	if err := zero.Close(); err != nil {
		t.Fatalf("zero value close: %v", err)
	}
}

func TestCounterexamplesOrdered(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "counterexamples")
	run, root, _ := planChain(t, f, run, testNow)
	root = f.reloadStep(t, root)

	lease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	later := testCounterexample(run, "case-b", testNow.Add(2*time.Second))
	earlier := testCounterexample(run, "case-a", testNow.Add(time.Second))
	_, err := f.store.CommitAttempt(ctx, domain.AttemptCommit{
		Ref: lease.Ref(),
		Result: domain.StepResult{
			Outcome:         domain.OutcomeSucceeded,
			ResultDigest:    "sha256:" + strings.Repeat("c", 64),
			Counterexamples: []domain.Counterexample{later, earlier},
			FinishedAt:      testNow.Add(2 * time.Second),
		},
		At: testNow.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("commit attempt: %v", err)
	}

	got, err := f.store.ListCounterexamples(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list counterexamples: %v", err)
	}
	if len(got) != 2 || got[0].CaseID != "case-a" || got[1].CaseID != "case-b" {
		t.Fatalf("expected counterexamples ordered by created_at, got %+v", got)
	}
	got[0].Nondeterminism = append(got[0].Nondeterminism, "mutated")
	got, err = f.store.ListCounterexamples(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list counterexamples: %v", err)
	}
	if len(got[0].Nondeterminism) != 1 {
		t.Fatal("counterexample was mutated through a returned value")
	}
	if missing, err := f.store.ListCounterexamples(ctx, run.TenantID, domain.NewRunID()); err != nil || len(missing) != 0 {
		t.Fatalf("expected empty counterexamples for unknown run, got %v/%v", missing, err)
	}
}

func testCounterexample(run domain.Run, caseID string, at time.Time) domain.Counterexample {
	return domain.Counterexample{
		CaseID:             caseID,
		TenantID:           run.TenantID,
		RunID:              run.ID,
		ClaimID:            "claim-1",
		InputCodec:         "json",
		InputRef:           domain.CASRef{Algorithm: "sha256", Digest: strings.Repeat("1", 64), SizeBytes: 1},
		BaseObservationRef: domain.CASRef{Algorithm: "sha256", Digest: strings.Repeat("2", 64), SizeBytes: 1},
		HeadObservationRef: domain.CASRef{Algorithm: "sha256", Digest: strings.Repeat("3", 64), SizeBytes: 1},
		Reproduced:         true,
		Scope:              "unit",
		Nondeterminism:     []string{"timing"},
		CreatedAt:          at,
	}
}

func testEvidence(run domain.Run, stepID domain.StepID, at time.Time) domain.Evidence {
	return domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      run.TenantID,
		RunID:         run.ID,
		StepID:        stepID,
		ClaimID:       "claim-1",
		Kind:          domain.EvidenceCompile,
		Result:        domain.ClaimTested,
		Scope:         domain.EvidenceScope{InputDomain: "unit"},
		Engine:        domain.EngineInfo{Name: "go", Version: "1.26"},
		Environment:   domain.EnvironmentInfo{Arch: "x86_64", SandboxProfile: "container"},
		CreatedAt:     at,
	}
}

func testReport(run domain.Run, at time.Time) domain.Report {
	return domain.Report{
		SchemaVersion: domain.ReportSchemaV1,
		ReportID:      domain.NewReportID(),
		RunID:         run.ID,
		TenantID:      run.TenantID,
		ProjectID:     run.ProjectID,
		Verdict:       domain.VerdictVerified,
		Manifest: domain.CASRef{
			Algorithm: "sha256",
			Digest:    strings.Repeat("b", 64),
			SizeBytes: 1,
		},
		MerkleRoot:  "merkle-root",
		Claims:      []domain.ClaimAssessment{{ClaimID: "claim-1", Kind: domain.ClaimKindCompile, Result: domain.ClaimTested, MinimumResult: domain.ClaimTested}},
		Attestation: domain.AttestationPending,
		GeneratedAt: at,
	}
}

func TestFailureCancelsTransitiveDependents(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "cascade")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 1, 0, 10, testNow)
	mid := newStep(run, "regression", domain.StepKindRegression, domain.StepBlocked, 1, 0, 5, testNow)
	leaf := newStep(run, "differential", domain.StepKindDifferential, domain.StepBlocked, 1, 0, 1, testNow)
	dependencies := map[domain.StepID][]domain.StepID{
		mid.ID:  {root.ID},
		leaf.ID: {mid.ID},
	}
	run = f.plan(t, run, []domain.Step{root, mid, leaf}, dependencies, nil, testNow)
	root = f.reloadStep(t, root)

	lease := f.lease(t, run, root, 0, testNow.Add(time.Second))
	f.start(t, lease, testNow.Add(2*time.Second))
	violated := testEvidence(run, root.ID, testNow.Add(3*time.Second))
	violated.Result = domain.ClaimTested
	outcome, err := f.store.CommitAttempt(ctx, domain.AttemptCommit{
		Ref: lease.Ref(),
		Result: domain.StepResult{
			Outcome:    domain.OutcomeFailed,
			ErrorClass: domain.ErrCompilationFailed,
			Message:    "compile failed",
			Evidence:   []domain.Evidence{violated},
			FinishedAt: testNow.Add(3 * time.Second),
		},
		At: testNow.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("commit attempt: %v", err)
	}
	if len(outcome.Propagated) != 2 {
		t.Fatalf("propagated = %d, want 2", len(outcome.Propagated))
	}
	stored, err := f.store.ListEvidence(ctx, run.TenantID, run.ID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(stored) != 1 || stored[0].Result != domain.ClaimTested {
		t.Fatalf("failed attempt evidence was not persisted: %+v", stored)
	}
	for _, want := range []domain.StepID{mid.ID, leaf.ID} {
		step, err := f.store.GetStep(ctx, run.TenantID, want)
		if err != nil {
			t.Fatalf("get step %s: %v", want, err)
		}
		if step.Status != domain.StepCancelled {
			t.Fatalf("step %s status = %s, want CANCELLED", step.Key, step.Status)
		}
		if step.FailureClass != domain.ErrDependencyFailed {
			t.Fatalf("step %s failure class = %s, want %s", step.Key, step.FailureClass, domain.ErrDependencyFailed)
		}
	}
	if !outcome.AllTerminal {
		t.Fatal("run should be terminal after dependent propagation")
	}
}

func TestTerminalRunRequestKeyCanBeReused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	first := f.createRun(t, domain.RunCancelled, testNow, "reusable")
	second := first
	second.ID = domain.NewRunID()
	second.CreatedAt = testNow.Add(time.Second)
	second.UpdatedAt = second.CreatedAt
	created, err := f.store.CreateRun(ctx, domain.CreateRunRecord{Run: second})
	if err != nil {
		t.Fatalf("create replacement run: %v", err)
	}
	if created.ID != second.ID {
		t.Fatalf("idempotency key was not reassigned: got %s want %s", created.ID, second.ID)
	}
}

func TestCreateRunEnforcesActiveRunQuota(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.createRun(t, domain.RunValidating, testNow, "active")
	blocked := domain.Run{
		ID: domain.NewRunID(), TenantID: f.tenantID, ProjectID: f.projectID,
		RequestKey: "blocked", Fingerprint: "fp-blocked", BaseCommit: "0123456",
		HeadCommit: "abcdef1", PolicyDigest: "sha256:" + strings.Repeat("a", 64),
		Status: domain.RunValidating, Region: "us-east-1", CreatedAt: testNow, UpdatedAt: testNow,
	}
	_, err := f.store.CreateRun(ctx, domain.CreateRunRecord{Run: blocked, MaxActiveRuns: 1})
	if !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("expected quota error, got %v", err)
	}
}

func TestCountActiveStepsCountsOnlyLeasedOrRunning(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "capacity")
	root := newStep(run, "compile", domain.StepKindCompile, domain.StepReady, 2, 0, 5, testNow)
	dependent := newStep(run, "regression", domain.StepKindRegression, domain.StepBlocked, 2, 0, 1, testNow)
	f.plan(t, run, []domain.Step{root, dependent}, map[domain.StepID][]domain.StepID{dependent.ID: {root.ID}}, nil, testNow)
	count, err := f.store.CountActiveSteps(ctx, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("queued/blocked steps = %d, want 0 capacity-consuming steps", count)
	}
	f.lease(t, run, root, 0, testNow.Add(time.Second))
	if count, err = f.store.CountActiveSteps(ctx, f.tenantID); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active steps = %d, want 1 leased step", count)
	}
}

func TestLeaseEnforcesRunParallelBudget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "parallel-budget")
	first := newStep(run, "first", domain.StepKindCompile, domain.StepReady, 2, 0, 5, testNow)
	second := newStep(run, "second", domain.StepKindRegression, domain.StepReady, 2, 0, 4, testNow)
	f.plan(t, run, []domain.Step{first, second}, nil, nil, testNow)
	first = f.reloadStep(t, first)
	second = f.reloadStep(t, second)

	if _, err := f.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: run.TenantID, RunID: run.ID, StepID: first.ID, AttemptID: domain.NewAttemptID(),
		ExpectedGeneration: 0, Owner: "worker-1", TTL: 30 * time.Second, ClusterID: "cluster-a",
		Now: testNow.Add(time.Second), MaxParallelSteps: 1,
	}); err != nil {
		t.Fatalf("first lease: %v", err)
	}
	_, err := f.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: run.TenantID, RunID: run.ID, StepID: second.ID, AttemptID: domain.NewAttemptID(),
		ExpectedGeneration: 0, Owner: "worker-1", TTL: 30 * time.Second, ClusterID: "cluster-a",
		Now: testNow.Add(2 * time.Second), MaxParallelSteps: 1,
	})
	var leaseErr *domain.LeaseError
	if !errors.As(err, &leaseErr) {
		t.Fatalf("expected parallel budget lease error, got %v", err)
	}
}

func TestLeaseEnforcesRunCPUBudget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := f.createRun(t, domain.RunValidating, testNow, "cpu-budget")
	first := newStep(run, "first", domain.StepKindCompile, domain.StepReady, 2, 0, 5, testNow)
	second := newStep(run, "second", domain.StepKindRegression, domain.StepReady, 2, 0, 4, testNow)
	f.plan(t, run, []domain.Step{first, second}, nil, nil, testNow)
	first = f.reloadStep(t, first)
	second = f.reloadStep(t, second)
	lease := f.lease(t, run, first, 0, testNow.Add(time.Second))
	f.start(t, lease, testNow.Add(2*time.Second))
	f.commitSuccess(t, lease, testNow.Add(3*time.Second), domain.StepMetrics{CPUSeconds: 5})

	_, err := f.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: run.TenantID, RunID: run.ID, StepID: second.ID, AttemptID: domain.NewAttemptID(),
		ExpectedGeneration: 0, Owner: "worker-1", TTL: 30 * time.Second, ClusterID: "cluster-a",
		Now: testNow.Add(4 * time.Second), MaxCPUSeconds: 5,
	})
	var leaseErr *domain.LeaseError
	if !errors.As(err, &leaseErr) {
		t.Fatalf("expected CPU budget lease error, got %v", err)
	}
}
