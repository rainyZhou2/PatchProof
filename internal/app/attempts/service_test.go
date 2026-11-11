package attempts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

var attemptTestNow = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

type attemptClock struct{ at time.Time }

func (c attemptClock) Now() time.Time { return c.at }

type attemptFixture struct {
	store     *memory.Store
	service   *Service
	tenantID  domain.TenantID
	projectID domain.ProjectID
	runID     domain.RunID
	stepID    domain.StepID
	attemptID domain.AttemptID
	ref       domain.AttemptRef
}

func attemptRequirements() domain.ResourceRequirements {
	return domain.ResourceRequirements{
		Architecture:    "amd64",
		Engine:          "go-build",
		Language:        "go",
		Isolation:       domain.IsolationContainer,
		CPUMilli:        1000,
		MemoryMiB:       1024,
		Network:         domain.NetworkRestricted,
		DeadlineSeconds: 600,
	}
}

// newAttemptFixture builds a VALIDATING run, plans one READY step and acquires
// a lease for it so the attempt service has a realistic starting point.
func newAttemptFixture(t *testing.T) *attemptFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()

	fixture := &attemptFixture{
		store:     store,
		tenantID:  domain.NewTenantID(),
		projectID: domain.NewProjectID(),
		runID:     domain.NewRunID(),
		stepID:    domain.NewStepID(),
		attemptID: domain.NewAttemptID(),
	}
	run := domain.Run{
		ID:          fixture.runID,
		TenantID:    fixture.tenantID,
		ProjectID:   fixture.projectID,
		RequestKey:  "attempt-fixture",
		Fingerprint: "fingerprint",
		BaseCommit:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadCommit:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Status:      domain.RunValidating,
		Region:      "region-a",
		CreatedAt:   attemptTestNow,
		UpdatedAt:   attemptTestNow,
	}
	if _, err := store.CreateRun(ctx, domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	step := domain.Step{
		ID:           fixture.stepID,
		RunID:        fixture.runID,
		TenantID:     fixture.tenantID,
		Key:          "compile",
		Kind:         domain.StepKindCompile,
		Status:       domain.StepReady,
		MaxAttempts:  3,
		Options:      map[string]string{"cache.enabled": "true"},
		Requirements: attemptRequirements(),
		Priority:     10,
		CreatedAt:    attemptTestNow,
		UpdatedAt:    attemptTestNow,
	}
	if _, err := store.PlanRun(ctx, domain.PlanTransaction{
		TenantID: fixture.tenantID, RunID: fixture.runID, ExpectedVersion: 0,
		Steps: []domain.Step{step}, At: attemptTestNow,
	}); err != nil {
		t.Fatalf("plan run: %v", err)
	}
	lease, err := store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: fixture.tenantID, RunID: fixture.runID, StepID: fixture.stepID,
		AttemptID: fixture.attemptID, ExpectedGeneration: 0, Owner: "worker-1",
		TTL: time.Minute, Now: attemptTestNow,
	})
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	fixture.ref = lease.Ref()

	service, err := New(store, attemptClock{at: attemptTestNow}, 2*time.Minute)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	fixture.service = service
	return fixture
}

func (f *attemptFixture) step(t *testing.T) domain.Step {
	t.Helper()
	step, err := f.store.GetStep(context.Background(), f.tenantID, f.stepID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	return step
}

func (f *attemptFixture) attempt(t *testing.T) domain.Attempt {
	t.Helper()
	attempts, err := f.store.ListAttempts(context.Background(), f.tenantID, f.stepID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	for _, attempt := range attempts {
		if attempt.ID == f.attemptID {
			return attempt
		}
	}
	t.Fatalf("attempt %s not found", f.attemptID)
	return domain.Attempt{}
}

func TestStartTransitionsLeaseToRunningAndIsIdempotent(t *testing.T) {
	fixture := newAttemptFixture(t)
	ctx := context.Background()

	if step := fixture.step(t); step.Status != domain.StepLeased {
		t.Fatalf("precondition: step status = %s, want LEASED", step.Status)
	}
	if err := fixture.service.Start(ctx, fixture.ref); err != nil {
		t.Fatalf("start: %v", err)
	}
	step := fixture.step(t)
	attempt := fixture.attempt(t)
	if step.Status != domain.StepRunning {
		t.Fatalf("step status = %s, want RUNNING", step.Status)
	}
	if attempt.Status != domain.AttemptRunning {
		t.Fatalf("attempt status = %s, want RUNNING", attempt.Status)
	}
	if attempt.StartedAt == nil {
		t.Fatal("attempt StartedAt is nil")
	}
	startedAt := *attempt.StartedAt

	// A repeated Start must be a no-op rather than a fencing error.
	if err := fixture.service.Start(ctx, fixture.ref); err != nil {
		t.Fatalf("second start: %v", err)
	}
	step = fixture.step(t)
	attempt = fixture.attempt(t)
	if step.Status != domain.StepRunning || attempt.Status != domain.AttemptRunning {
		t.Fatalf("repeated start changed state: step=%s attempt=%s", step.Status, attempt.Status)
	}
	if attempt.StartedAt == nil || !attempt.StartedAt.Equal(startedAt) {
		t.Fatalf("repeated start changed StartedAt: %v -> %v", attempt.StartedAt, attempt.StartedAt)
	}
}

// TestHeartbeatExtendsLease verifies that a worker heartbeat renews the lease
// using its attempt identity, without requiring the scheduler's owner id.
func TestHeartbeatExtendsLease(t *testing.T) {
	fixture := newAttemptFixture(t)
	ctx := context.Background()
	if err := fixture.service.Start(ctx, fixture.ref); err != nil {
		t.Fatalf("start: %v", err)
	}
	before := fixture.step(t)
	if before.LeaseDeadline == nil {
		t.Fatal("precondition: lease deadline is nil")
	}
	if err := fixture.service.Heartbeat(ctx, fixture.ref); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	after := fixture.step(t)
	if after.LeaseDeadline == nil || !after.LeaseDeadline.After(*before.LeaseDeadline) {
		t.Fatalf("heartbeat did not extend lease: before=%v after=%v", before.LeaseDeadline, after.LeaseDeadline)
	}
}

// TestHeartbeatFailsAfterExpiry verifies that a heartbeat cannot revive an
// expired lease.
func TestHeartbeatFailsAfterExpiry(t *testing.T) {
	fixture := newAttemptFixture(t)
	ctx := context.Background()
	if err := fixture.service.Start(ctx, fixture.ref); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := fixture.store.ExpireLeases(ctx, attemptTestNow.Add(2*time.Minute), 10); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if err := fixture.service.Heartbeat(ctx, fixture.ref); err == nil {
		t.Fatal("heartbeat succeeded after the lease expired")
	}
}

func TestCompleteCommitsSucceededResult(t *testing.T) {
	fixture := newAttemptFixture(t)
	ctx := context.Background()

	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      fixture.tenantID,
		RunID:         fixture.runID,
		ClaimID:       "compile",
		Kind:          domain.EvidenceCompile,
		Result:        domain.ClaimTested,
		Scope:         domain.EvidenceScope{InputDomain: "all packages"},
		Engine:        domain.EngineInfo{Name: "go-build", Version: "1.0.0"},
		Environment:   domain.EnvironmentInfo{Arch: "arm64", SandboxProfile: "test"},
		CreatedAt:     attemptTestNow,
	}
	outcome, err := fixture.service.Complete(ctx, fixture.ref, domain.StepResult{
		Outcome:      domain.OutcomeSucceeded,
		ResultDigest: "result-digest",
		Evidence:     []domain.Evidence{evidence},
		WorkerID:     "worker-1",
		FinishedAt:   attemptTestNow,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if outcome.Step.Status != domain.StepSucceeded {
		t.Fatalf("step status = %s, want SUCCEEDED", outcome.Step.Status)
	}
	if outcome.Attempt.Status != domain.AttemptReported {
		t.Fatalf("attempt status = %s, want REPORTED", outcome.Attempt.Status)
	}
	if !outcome.AllTerminal {
		t.Fatal("AllTerminal = false, want true for a single-step run")
	}
	if step := fixture.step(t); step.Status != domain.StepSucceeded {
		t.Fatalf("stored step status = %s, want SUCCEEDED", step.Status)
	}
	stored, err := fixture.store.ListEvidence(ctx, fixture.tenantID, fixture.runID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(stored) != 1 || stored[0].StepID != fixture.stepID {
		t.Fatalf("stored evidence = %+v, want one entry bound to step %s", stored, fixture.stepID)
	}
}

func TestCompleteRejectsMismatchedResults(t *testing.T) {
	otherRun := domain.NewRunID()
	otherTenant := domain.NewTenantID()
	otherStep := domain.NewStepID()

	cases := []struct {
		name  string
		field string
		build func() domain.StepResult
	}{
		{
			name:  "evidence run mismatch",
			field: "evidence.run_id",
			build: func() domain.StepResult {
				return domain.StepResult{Outcome: domain.OutcomeSucceeded, Evidence: []domain.Evidence{{RunID: otherRun}}}
			},
		},
		{
			name:  "evidence tenant mismatch",
			field: "evidence.tenant_id",
			build: func() domain.StepResult {
				return domain.StepResult{Outcome: domain.OutcomeSucceeded, Evidence: []domain.Evidence{{TenantID: otherTenant}}}
			},
		},
		{
			name:  "evidence step mismatch",
			field: "evidence.step_id",
			build: func() domain.StepResult {
				return domain.StepResult{Outcome: domain.OutcomeSucceeded, Evidence: []domain.Evidence{{StepID: otherStep}}}
			},
		},
		{
			name:  "counterexample run mismatch",
			field: "counterexample.run_id",
			build: func() domain.StepResult {
				return domain.StepResult{Outcome: domain.OutcomeSucceeded, Counterexamples: []domain.Counterexample{{RunID: otherRun}}}
			},
		},
		{
			name:  "counterexample tenant mismatch",
			field: "counterexample.tenant_id",
			build: func() domain.StepResult {
				return domain.StepResult{Outcome: domain.OutcomeSucceeded, Counterexamples: []domain.Counterexample{{TenantID: otherTenant}}}
			},
		},
		{
			name:  "empty outcome",
			field: "result.outcome",
			build: func() domain.StepResult { return domain.StepResult{} },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAttemptFixture(t)
			_, err := fixture.service.Complete(context.Background(), fixture.ref, testCase.build())
			var validation *domain.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error = %v, want *domain.ValidationError", err)
			}
			if validation.Field != testCase.field {
				t.Fatalf("error field = %q, want %q", validation.Field, testCase.field)
			}
			if step := fixture.step(t); step.Status != domain.StepLeased {
				t.Fatalf("step status = %s after rejected commit, want LEASED", step.Status)
			}
			if evidence, err := fixture.store.ListEvidence(context.Background(), fixture.tenantID, fixture.runID); err != nil || len(evidence) != 0 {
				t.Fatalf("evidence stored after rejected commit: %v, err=%v", evidence, err)
			}
		})
	}
}

func TestFailClassifiesRetryAndTerminalFailures(t *testing.T) {
	t.Run("retryable executor error schedules a retry", func(t *testing.T) {
		fixture := newAttemptFixture(t)
		outcome, err := fixture.service.Fail(context.Background(), fixture.ref, domain.ErrExecutorError, "transient", true)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if outcome.Step.Status != domain.StepRetryWait {
			t.Fatalf("step status = %s, want RETRY_WAIT", outcome.Step.Status)
		}
		if outcome.Step.NextAttemptAt == nil {
			t.Fatal("NextAttemptAt is nil for a retryable failure")
		}
		if outcome.AllTerminal {
			t.Fatal("AllTerminal = true for a retryable failure")
		}
		if outcome.Attempt.Status != domain.AttemptFailedInfra {
			t.Fatalf("attempt status = %s, want FAILED_INFRA", outcome.Attempt.Status)
		}
	})

	t.Run("non-retryable compilation failure is terminal", func(t *testing.T) {
		fixture := newAttemptFixture(t)
		outcome, err := fixture.service.Fail(context.Background(), fixture.ref, domain.ErrCompilationFailed, "build failed", false)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if outcome.Step.Status != domain.StepFailed {
			t.Fatalf("step status = %s, want FAILED", outcome.Step.Status)
		}
		if outcome.Step.FinishedAt == nil {
			t.Fatal("FinishedAt is nil for a terminal failure")
		}
		if !outcome.AllTerminal {
			t.Fatal("AllTerminal = false for a terminal failure")
		}
	})

	t.Run("empty error class defaults to executor error", func(t *testing.T) {
		fixture := newAttemptFixture(t)
		outcome, err := fixture.service.Fail(context.Background(), fixture.ref, "", "unknown", true)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if outcome.Attempt.ErrorClass != domain.ErrExecutorError {
			t.Fatalf("attempt error class = %q, want %q", outcome.Attempt.ErrorClass, domain.ErrExecutorError)
		}
		if outcome.Step.Status != domain.StepRetryWait {
			t.Fatalf("step status = %s, want RETRY_WAIT", outcome.Step.Status)
		}
	})
}

func TestFailRejectsInvalidErrorClass(t *testing.T) {
	fixture := newAttemptFixture(t)
	_, err := fixture.service.Fail(context.Background(), fixture.ref, domain.ErrorClass("NOT_A_CLASS"), "boom", true)
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "error_class" {
		t.Fatalf("error field = %q, want error_class", validation.Field)
	}
	if step := fixture.step(t); step.Status != domain.StepLeased {
		t.Fatalf("step status = %s after rejected failure, want LEASED", step.Status)
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(nil, nil, 0); err == nil {
		t.Fatal("New(nil, ...) returned no error")
	}
	var _ ports.Clock = attemptClock{}
}
