package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/patchproof/patchproof/internal/domain"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PATCHPROOF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PATCHPROOF_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	store, err := New(ctx, Config{
		DSN:              dsn,
		MaxConns:         12,
		MinConns:         1,
		MaxConnLifetime:  time.Hour,
		MaxConnIdleTime:  5 * time.Minute,
		StatementTimeout: 30 * time.Second,
		ApplicationName:  "patchproof-store-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	return store
}

func seedTenantProject(t *testing.T, store *Store) (domain.Tenant, domain.Project) {
	t.Helper()
	ctx := context.Background()
	tenant := domain.Tenant{
		ID:           domain.NewTenantID(),
		HomeRegion:   "region-a",
		SecurityTier: domain.SecurityTierStandard,
		Status:       domain.TenantActive,
		DisplayName:  "test tenant",
	}
	if err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	project := domain.Project{
		ID:                  domain.NewProjectID(),
		TenantID:            tenant.ID,
		Provider:            "github",
		RepositoryURI:       "https://github.com/example/repository",
		DefaultBranch:       "main",
		AllowedRegions:      []string{"region-a"},
		Classification:      "internal",
		DefaultPolicyDigest: "sha256:" + strings.Repeat("d", 64),
	}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return tenant, project
}

func newRunRecord(t *testing.T, tenant domain.Tenant, project domain.Project, requestKey, fingerprint string) domain.CreateRunRecord {
	t.Helper()
	now := time.Now().UTC()
	run := domain.Run{
		ID:              domain.NewRunID(),
		TenantID:        tenant.ID,
		ProjectID:       project.ID,
		RequestKey:      requestKey,
		Fingerprint:     fingerprint,
		BaseCommit:      strings.Repeat("a", 40),
		HeadCommit:      strings.Repeat("b", 40),
		PatchDigest:     "sha256:" + strings.Repeat("c", 64),
		PolicyRef:       "policy:main",
		PolicyDigest:    "sha256:" + strings.Repeat("d", 64),
		RequestedChecks: []string{"compile"},
		Budget:          domain.Budget{CPUSeconds: 100, WallSeconds: 300, MaxParallelSteps: 4},
		Status:          domain.RunCreated,
		Region:          "region-a",
		CreatedBy:       "tester",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	event, err := domain.NewOutboxEvent(tenant.ID, domain.EventRunCreated, run.ID.String(), 0,
		map[string]any{"run_id": run.ID.String(), "request_key": requestKey}, now)
	if err != nil {
		t.Fatalf("NewOutboxEvent: %v", err)
	}
	return domain.CreateRunRecord{Run: run, Events: []domain.OutboxEvent{event}}
}

func validRequirements() domain.ResourceRequirements {
	return domain.ResourceRequirements{
		Architecture:    "amd64",
		Isolation:       domain.IsolationContainer,
		CPUMilli:        1000,
		MemoryMiB:       1024,
		Network:         domain.NetworkNone,
		DeadlineSeconds: 300,
	}
}

func newStep(run domain.Run, key string, kind domain.StepKind, status domain.StepStatus, shard int) domain.Step {
	return domain.Step{
		ID:                  domain.NewStepID(),
		RunID:               run.ID,
		TenantID:            run.TenantID,
		Key:                 key,
		Kind:                kind,
		Status:              status,
		MaxAttempts:         3,
		Requirements:        validRequirements(),
		InputManifestDigest: "sha256:" + strings.Repeat("1", 64),
		Priority:            5,
		Shard:               shard,
		Options:             map[string]string{"goflags": "-race", "tags": "unit"},
	}
}

func newClaim(run domain.Run, claimID string) domain.Claim {
	return domain.Claim{
		ID:            domain.ClaimID(claimID),
		RunID:         run.ID,
		TenantID:      run.TenantID,
		Kind:          domain.ClaimKindTest,
		Title:         "test claim",
		Domain:        "semantic",
		Language:      "go",
		Required:      true,
		MinimumResult: domain.ClaimTested,
	}
}

func transitionRun(t *testing.T, store *Store, run domain.Run, from, to domain.RunStatus) domain.Run {
	t.Helper()
	updated, err := store.UpdateRun(context.Background(), domain.RunUpdate{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: run.StateVersion,
		From:            from,
		To:              to,
		At:              time.Now().UTC(),
		Event: func() *domain.OutboxEvent {
			event, err := domain.NewOutboxEvent(run.TenantID, domain.EventRunValidated, run.ID.String(), 0,
				map[string]any{"run_id": run.ID.String()}, time.Now().UTC())
			if err != nil {
				t.Fatalf("NewOutboxEvent: %v", err)
			}
			return &event
		}(),
	})
	if err != nil {
		t.Fatalf("UpdateRun %s -> %s: %v", from, to, err)
	}
	return updated
}

func planRun(t *testing.T, store *Store, run domain.Run, steps []domain.Step, dependencies map[domain.StepID][]domain.StepID, claims []domain.Claim) domain.Run {
	t.Helper()
	if run.Status == domain.RunCreated {
		run = transitionRun(t, store, run, domain.RunCreated, domain.RunValidating)
	}
	planned, err := store.PlanRun(context.Background(), domain.PlanTransaction{
		TenantID:        run.TenantID,
		RunID:           run.ID,
		ExpectedVersion: run.StateVersion,
		Steps:           steps,
		Dependencies:    dependencies,
		Claims:          claims,
		At:              time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("PlanRun: %v", err)
	}
	return planned
}

func acquireLease(t *testing.T, store *Store, step domain.Step, owner string, generation int64, now time.Time) domain.Lease {
	t.Helper()
	lease, err := store.AcquireLease(context.Background(), domain.LeaseRequest{
		TenantID:           step.TenantID,
		RunID:              step.RunID,
		StepID:             step.ID,
		AttemptID:          domain.NewAttemptID(),
		ExpectedGeneration: generation,
		Owner:              owner,
		TTL:                30 * time.Second,
		Now:                now,
		Priority:           step.Priority,
	})
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	return lease
}

func TestIntegrationCreateRunIsIdempotent(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	record := newRunRecord(t, tenant, project, "request-1", "fingerprint-1")

	created, err := store.CreateRun(ctx, record)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if created.Status != domain.RunCreated || created.StateVersion != 0 {
		t.Fatalf("unexpected run state: %+v", created)
	}
	retried, err := store.CreateRun(ctx, record)
	if err != nil {
		t.Fatalf("idempotent CreateRun: %v", err)
	}
	if retried.ID != created.ID {
		t.Fatalf("idempotent retry returned %s, want %s", retried.ID, created.ID)
	}

	duplicateKey := newRunRecord(t, tenant, project, "request-1", "fingerprint-1")
	sameFingerprint, err := store.CreateRun(ctx, duplicateKey)
	if err != nil {
		t.Fatalf("CreateRun with same key: %v", err)
	}
	if sameFingerprint.ID != created.ID {
		t.Fatalf("same key returned %s, want %s", sameFingerprint.ID, created.ID)
	}

	conflicting := newRunRecord(t, tenant, project, "request-1", "fingerprint-2")
	if _, err := store.CreateRun(ctx, conflicting); err == nil {
		t.Fatal("expected idempotency conflict")
	} else {
		var conflict *domain.IdempotencyConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("expected IdempotencyConflictError, got %T: %v", err, err)
		}
	}

	loaded, err := store.GetRun(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if loaded.Fingerprint != created.Fingerprint || loaded.RequestKey != created.RequestKey {
		t.Fatalf("loaded run mismatch: %+v", loaded)
	}
	page, err := store.ListRuns(ctx, domain.RunFilter{TenantID: tenant.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].ID != created.ID {
		t.Fatalf("ListRuns returned %+v", page.Runs)
	}
}

func TestIntegrationPlanRunVersionConflict(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "plan-request", "plan-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step := newStep(created, "compile:go", domain.StepKindCompile, domain.StepReady, 0)
	claim := newClaim(created, "claim-compile")
	validating := transitionRun(t, store, created, domain.RunCreated, domain.RunValidating)

	_, err = store.PlanRun(ctx, domain.PlanTransaction{
		TenantID:        tenant.ID,
		RunID:           created.ID,
		ExpectedVersion: validating.StateVersion + 1,
		Steps:           []domain.Step{step},
		Claims:          []domain.Claim{claim},
		At:              time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected version conflict")
	}
	var conflict *domain.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError, got %T: %v", err, err)
	}

	planned := planRun(t, store, validating, []domain.Step{step}, nil, []domain.Claim{claim})
	if planned.Status != domain.RunPlanned {
		t.Fatalf("planned run status = %s", planned.Status)
	}
	if planned.StateVersion != validating.StateVersion+1 {
		t.Fatalf("planned state version = %d, want %d", planned.StateVersion, validating.StateVersion+1)
	}
	if planned.PlannedAt == nil {
		t.Fatal("planned_at was not recorded")
	}
	steps, err := store.ListSteps(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected one step, got %d", len(steps))
	}
	if steps[0].Options["goflags"] != "-race" || steps[0].Options["tags"] != "unit" {
		t.Fatalf("step options not persisted: %+v", steps[0].Options)
	}
	dag, err := store.GetDAG(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("GetDAG: %v", err)
	}
	if len(dag.Steps) != 1 || len(dag.Claims) != 1 {
		t.Fatalf("unexpected DAG: %+v", dag)
	}

	replacement := newStep(planned, "compile:go:v2", domain.StepKindCompile, domain.StepReady, 0)
	replanned := planRun(t, store, planned, []domain.Step{replacement}, nil, nil)
	if replanned.StateVersion != planned.StateVersion+1 {
		t.Fatalf("replanned state version = %d, want %d", replanned.StateVersion, planned.StateVersion+1)
	}
	if replanned.PlannedAt == nil || !replanned.PlannedAt.Equal(*planned.PlannedAt) && !replanned.PlannedAt.After(*planned.PlannedAt) {
		t.Fatalf("replanned_at not advanced: %v", replanned.PlannedAt)
	}
	replacedSteps, err := store.ListSteps(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("ListSteps after replan: %v", err)
	}
	if len(replacedSteps) != 1 || replacedSteps[0].ID != replacement.ID {
		t.Fatalf("replanned steps = %+v", replacedSteps)
	}
	if _, err := store.GetStep(ctx, tenant.ID, step.ID); !domain.IsNotFound(err) {
		t.Fatalf("old step still exists after replan: %v", err)
	}
	_, err = store.UpdateRun(ctx, domain.RunUpdate{
		TenantID: tenant.ID, RunID: created.ID, ExpectedVersion: replanned.StateVersion,
		From: domain.RunCreated, To: domain.RunValidating, At: time.Now().UTC(),
	})
	var transition *domain.StateTransitionError
	if !errors.As(err, &transition) {
		t.Fatalf("expected StateTransitionError for from-status mismatch, got %T: %v", err, err)
	}
}

func TestIntegrationAcquireLeaseSingleWinner(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "lease-request", "lease-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step := newStep(created, "compile:go", domain.StepKindCompile, domain.StepReady, 0)
	planRun(t, store, created, []domain.Step{step}, nil, nil)
	t.Cleanup(func() { _, _ = store.CancelRunSteps(ctx, tenant.ID, created.ID, time.Now()) })

	now := time.Now().UTC()
	const contenders = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  int
		losers   int
		failures []error
	)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := store.AcquireLease(ctx, domain.LeaseRequest{
				TenantID:           tenant.ID,
				RunID:              created.ID,
				StepID:             step.ID,
				AttemptID:          domain.NewAttemptID(),
				ExpectedGeneration: 0,
				Owner:              "controller-" + uuid.NewString(),
				TTL:                30 * time.Second,
				Now:                now,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
				return
			}
			var leaseErr *domain.LeaseError
			if errors.As(err, &leaseErr) {
				losers++
				return
			}
			failures = append(failures, err)
		}(i)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("unexpected lease errors: %v", failures)
	}
	if winners != 1 || losers != contenders-1 {
		t.Fatalf("winners=%d losers=%d, want exactly one winner", winners, losers)
	}
	loaded, err := store.GetStep(ctx, tenant.ID, step.ID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if loaded.Status != domain.StepLeased || loaded.Generation != 1 || loaded.AttemptCount != 1 {
		t.Fatalf("leased step mismatch: %+v", loaded)
	}
	attempts, err := store.ListAttempts(ctx, tenant.ID, step.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != domain.AttemptAssigned {
		t.Fatalf("attempts = %+v", attempts)
	}
}

func TestIntegrationStaleCommitRejected(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "fence-request", "fence-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step := newStep(created, "compile:go", domain.StepKindCompile, domain.StepReady, 0)
	planRun(t, store, created, []domain.Step{step}, nil, nil)
	lease := acquireLease(t, store, step, "controller-a", 0, time.Now().UTC())
	if err := store.StartAttempt(ctx, lease.Ref(), time.Now().UTC()); err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	commit := domain.AttemptCommit{
		Ref: lease.Ref(),
		Result: domain.StepResult{
			Outcome:      domain.OutcomeSucceeded,
			ResultDigest: "sha256:" + strings.Repeat("2", 64),
			FinishedAt:   time.Now().UTC(),
		},
		At: time.Now().UTC(),
	}
	stale := commit
	stale.Ref.Generation = lease.Generation + 1
	if _, err := store.CommitAttempt(ctx, stale); err == nil {
		t.Fatal("expected fencing rejection")
	} else {
		var fencing *domain.FencingError
		if !errors.As(err, &fencing) {
			t.Fatalf("expected FencingError, got %T: %v", err, err)
		}
	}
	outcome, err := store.CommitAttempt(ctx, commit)
	if err != nil {
		t.Fatalf("CommitAttempt: %v", err)
	}
	if outcome.Step.Status != domain.StepSucceeded || outcome.Attempt.Status != domain.AttemptReported {
		t.Fatalf("unexpected commit outcome: %+v", outcome)
	}
}

func TestIntegrationCommitPromotesDependent(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "promote-request", "promote-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	first := newStep(created, "compile:go", domain.StepKindCompile, domain.StepReady, 0)
	second := newStep(created, "regression:go", domain.StepKindRegression, domain.StepBlocked, 0)
	dependencies := map[domain.StepID][]domain.StepID{second.ID: {first.ID}}
	claim := newClaim(created, "claim-regression")
	planRun(t, store, created, []domain.Step{first, second}, dependencies, []domain.Claim{claim})

	lease := acquireLease(t, store, first, "controller-a", 0, time.Now().UTC())
	if err := store.StartAttempt(ctx, lease.Ref(), time.Now().UTC()); err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      tenant.ID,
		RunID:         created.ID,
		ClaimID:       claim.ID,
		Kind:          domain.EvidenceRegression,
		Result:        domain.ClaimTested,
		Scope:         domain.EvidenceScope{InputDomain: "uint64"},
		Engine:        domain.EngineInfo{Name: "go-test", Version: "1.0.0"},
		Environment:   domain.EnvironmentInfo{Arch: "amd64", SandboxProfile: "container"},
		CreatedAt:     time.Now().UTC(),
	}
	counterexample := domain.Counterexample{
		CaseID:             "case-1",
		TenantID:           tenant.ID,
		RunID:              created.ID,
		ClaimID:            claim.ID,
		InputCodec:         "raw",
		InputRef:           domain.NewCASRef(strings.Repeat("3", 64), 16),
		BaseObservationRef: domain.NewCASRef(strings.Repeat("4", 64), 16),
		HeadObservationRef: domain.NewCASRef(strings.Repeat("5", 64), 16),
		Reproduced:         false,
		Scope:              "uint64",
		CreatedAt:          time.Now().UTC(),
	}
	outcome, err := store.CommitAttempt(ctx, domain.AttemptCommit{
		Ref: lease.Ref(),
		Result: domain.StepResult{
			Outcome:         domain.OutcomeSucceeded,
			ResultDigest:    "sha256:" + strings.Repeat("6", 64),
			OutputManifest:  "sha256:" + strings.Repeat("7", 64),
			Evidence:        []domain.Evidence{evidence},
			Counterexamples: []domain.Counterexample{counterexample},
			Metrics:         domain.StepMetrics{CPUSeconds: 2.5, MemoryGiBSeconds: 1.5, CASWrittenBytes: 1024},
			FinishedAt:      time.Now().UTC(),
		},
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CommitAttempt: %v", err)
	}
	if len(outcome.Promoted) != 1 || outcome.Promoted[0].ID != second.ID {
		t.Fatalf("promoted = %+v", outcome.Promoted)
	}
	promoted, err := store.GetStep(ctx, tenant.ID, second.ID)
	if err != nil {
		t.Fatalf("GetStep promoted: %v", err)
	}
	if promoted.Status != domain.StepReady {
		t.Fatalf("dependent status = %s, want READY", promoted.Status)
	}
	evidenceList, err := store.ListEvidence(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("ListEvidence: %v", err)
	}
	if len(evidenceList) != 1 || evidenceList[0].ID != evidence.ID {
		t.Fatalf("evidence = %+v", evidenceList)
	}
	if evidenceList[0].StepID != first.ID {
		t.Fatalf("evidence step id = %s, want %s", evidenceList[0].StepID, first.ID)
	}
	counterexamples, err := store.ListCounterexamples(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("ListCounterexamples: %v", err)
	}
	if len(counterexamples) != 1 || counterexamples[0].CaseID != counterexample.CaseID {
		t.Fatalf("counterexamples = %+v", counterexamples)
	}
	usage, err := store.TenantUsage(ctx, tenant.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("TenantUsage: %v", err)
	}
	if usage.DailyCPUSeconds != 2.5 {
		t.Fatalf("daily cpu seconds = %v, want 2.5", usage.DailyCPUSeconds)
	}
	if usage.ActiveSteps != 1 {
		t.Fatalf("active steps = %d, want 1", usage.ActiveSteps)
	}
}

func TestIntegrationExpireLeasesRetries(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "expire-request", "expire-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step := newStep(created, "compile:go", domain.StepKindCompile, domain.StepReady, 0)
	planRun(t, store, created, []domain.Step{step}, nil, nil)
	leaseAt := time.Now().UTC().Add(-time.Minute)
	acquireLease(t, store, step, "controller-expire", 0, leaseAt)
	expiredAt := leaseAt.Add(2 * time.Minute)
	expiries, err := store.ExpireLeases(ctx, expiredAt, 100)
	if err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	var expiry *domain.LeaseExpiry
	for i := range expiries {
		if expiries[i].StepID == step.ID {
			expiry = &expiries[i]
		}
	}
	if expiry == nil {
		t.Fatalf("step %s missing from expiries %+v", step.ID, expiries)
	}
	if expiry.Final {
		t.Fatal("first expiry must not be final")
	}
	loaded, err := store.GetStep(ctx, tenant.ID, step.ID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if loaded.Status != domain.StepRetryWait || loaded.NextAttemptAt == nil {
		t.Fatalf("expired step = %+v", loaded)
	}
	attempts, err := store.ListAttempts(ctx, tenant.ID, step.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != domain.AttemptExpired || attempts[0].ErrorClass != domain.ErrLeaseExpired {
		t.Fatalf("expired attempt = %+v", attempts)
	}
	promoted, err := store.PromoteDueSteps(ctx, loaded.NextAttemptAt.Add(time.Second), 100)
	if err != nil {
		t.Fatalf("PromoteDueSteps: %v", err)
	}
	found := false
	for _, candidate := range promoted {
		if candidate.ID == step.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("step %s missing from promoted steps", step.ID)
	}
	ready, err := store.GetStep(ctx, tenant.ID, step.ID)
	if err != nil {
		t.Fatalf("GetStep after promote: %v", err)
	}
	if ready.Status != domain.StepReady || ready.NextAttemptAt != nil {
		t.Fatalf("promoted step = %+v", ready)
	}
}

func TestIntegrationOutboxDelivery(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "outbox-request", "outbox-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	now := time.Now().UTC()
	event, err := domain.NewOutboxEvent(tenant.ID, domain.EventRunQueued, created.ID.String(), 0,
		map[string]any{"run_id": created.ID.String()}, now)
	if err != nil {
		t.Fatalf("NewOutboxEvent: %v", err)
	}
	if err := store.AppendOutbox(ctx, []domain.OutboxEvent{event}); err != nil {
		t.Fatalf("AppendOutbox: %v", err)
	}
	pending, err := store.PendingOutbox(ctx, 1000)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	present := false
	for _, candidate := range pending {
		if candidate.ID == event.ID {
			present = true
		}
	}
	if !present {
		t.Fatalf("event %s missing from pending outbox", event.ID)
	}
	if err := store.RecordOutboxFailure(ctx, domain.OutboxDeliveryFailure{
		EventID: event.ID, Reason: "broker unavailable", At: now,
	}); err != nil {
		t.Fatalf("RecordOutboxFailure: %v", err)
	}
	if err := store.MarkOutboxDelivered(ctx, []domain.EventID{event.ID}, now.Add(time.Second)); err != nil {
		t.Fatalf("MarkOutboxDelivered: %v", err)
	}
	pending, err = store.PendingOutbox(ctx, 1000)
	if err != nil {
		t.Fatalf("PendingOutbox after delivery: %v", err)
	}
	for _, candidate := range pending {
		if candidate.ID == event.ID {
			t.Fatalf("event %s is still pending after delivery", event.ID)
		}
	}
}

func TestIntegrationReportRoundTrip(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "report-request", "report-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	report := domain.Report{
		SchemaVersion:   domain.ReportSchemaV1,
		ReportID:        domain.NewReportID(),
		RunID:           created.ID,
		TenantID:        tenant.ID,
		ProjectID:       project.ID,
		Verdict:         domain.VerdictVerified,
		BaseCommit:      created.BaseCommit,
		HeadCommit:      created.HeadCommit,
		SnapshotDigest:  "sha256:" + strings.Repeat("8", 64),
		PolicyDigest:    created.PolicyDigest,
		RequestedChecks: []string{"compile"},
		CompletedChecks: []string{"compile"},
		Claims: []domain.ClaimAssessment{{
			ClaimID:       "claim-compile",
			Kind:          domain.ClaimKindCompile,
			Title:         "compiles",
			Required:      true,
			MinimumResult: domain.ClaimTested,
			Result:        domain.ClaimTested,
		}},
		Manifest:    domain.NewCASRef(strings.Repeat("9", 64), 256),
		MerkleRoot:  "sha256:" + strings.Repeat("a", 64),
		Attestation: domain.AttestationPending,
		GeneratedAt: time.Now().UTC(),
	}
	if err := store.PutReport(ctx, report); err != nil {
		t.Fatalf("PutReport: %v", err)
	}
	loaded, err := store.GetReport(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if loaded.ReportID != report.ReportID || loaded.Verdict != domain.VerdictVerified {
		t.Fatalf("report mismatch: %+v", loaded)
	}
	if len(loaded.Claims) != 1 || loaded.Claims[0].Result != domain.ClaimTested {
		t.Fatalf("report claims mismatch: %+v", loaded.Claims)
	}
	report.Verdict = domain.VerdictInconclusive
	report.GeneratedAt = report.GeneratedAt.Add(time.Minute)
	if err := store.PutReport(ctx, report); err != nil {
		t.Fatalf("PutReport update: %v", err)
	}
	updated, err := store.GetReport(ctx, tenant.ID, created.ID)
	if err != nil {
		t.Fatalf("GetReport update: %v", err)
	}
	if updated.Verdict != domain.VerdictInconclusive {
		t.Fatalf("latest report not returned: %+v", updated)
	}
}

func TestIntegrationTenantProjectPolicy(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)

	loaded, err := store.GetTenant(ctx, tenant.ID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if loaded.HomeRegion != tenant.HomeRegion || loaded.SecurityTier != tenant.SecurityTier {
		t.Fatalf("tenant mismatch: %+v", loaded)
	}
	updated, err := store.UpdateTenantEpoch(ctx, tenant.ID, loaded.ActiveWriterEpoch, loaded.ActiveWriterEpoch+1, "region-b")
	if err != nil {
		t.Fatalf("UpdateTenantEpoch: %v", err)
	}
	if updated.ActiveWriterEpoch != loaded.ActiveWriterEpoch+1 || updated.HomeRegion != "region-b" {
		t.Fatalf("tenant epoch update mismatch: %+v", updated)
	}
	if _, err := store.UpdateTenantEpoch(ctx, tenant.ID, loaded.ActiveWriterEpoch, loaded.ActiveWriterEpoch+2, "region-c"); err == nil {
		t.Fatal("expected tenant epoch conflict")
	} else {
		var conflict *domain.VersionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("expected VersionConflictError, got %T: %v", err, err)
		}
	}

	loadedProject, err := store.GetProject(ctx, tenant.ID, project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if loadedProject.RepositoryURI != project.RepositoryURI || len(loadedProject.AllowedRegions) != 1 {
		t.Fatalf("project mismatch: %+v", loadedProject)
	}
	if _, err := store.GetProject(ctx, tenant.ID, domain.NewProjectID()); err == nil {
		t.Fatal("expected project not found")
	} else if !domain.IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}

	policy := domain.Policy{
		TenantID:  tenant.ID,
		ProjectID: project.ID,
		Digest:    "sha256:" + strings.Repeat("e", 64),
		Version:   "1",
		Source:    domain.PolicySourceAdmin,
		Trusted:   true,
		Document:  []byte("apiVersion: patchproof.io/v1\nkind: VerificationPolicy\n"),
		CreatedBy: "tester",
		CreatedAt: time.Now().UTC(),
	}
	if err := store.PutPolicy(ctx, policy); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	loadedPolicy, err := store.GetPolicy(ctx, tenant.ID, policy.Digest)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if loadedPolicy.Version != "1" || !loadedPolicy.Trusted || len(loadedPolicy.Document) == 0 {
		t.Fatalf("policy mismatch: %+v", loadedPolicy)
	}
	policy.Version = "2"
	if err := store.PutPolicy(ctx, policy); err != nil {
		t.Fatalf("PutPolicy update: %v", err)
	}
	updatedPolicy, err := store.GetPolicy(ctx, tenant.ID, policy.Digest)
	if err != nil {
		t.Fatalf("GetPolicy update: %v", err)
	}
	if updatedPolicy.Version != "2" {
		t.Fatalf("policy version = %s, want 2", updatedPolicy.Version)
	}
}

func TestIntegrationClustersShardsAndAudit(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	now := time.Now().UTC()

	cluster := domain.Cluster{
		ID:                 "cluster-a",
		Region:             "region-a",
		AvailabilityZone:   "zone-a",
		ExecutorKind:       "kubernetes",
		Endpoint:           "https://cluster-a.example",
		Namespace:          "patchproof",
		Architectures:      []string{"amd64", "arm64"},
		Languages:          []string{"go"},
		Engines:            []string{"go-test"},
		IsolationTiers:     []domain.IsolationTier{domain.IsolationContainer, domain.IsolationGVisor},
		RuntimeClasses:     []string{"standard"},
		AvailableCPUMilli:  8000,
		AvailableMemoryMiB: 16384,
		QueueDepth:         3,
		ActiveAttempts:     2,
		MaxConcurrentSteps: 16,
		CostTier:           1,
		CasRegion:          "region-a",
		Healthy:            true,
		Version:            "v1",
		Labels:             map[string]string{"features": "race,coverage"},
		LastHeartbeat:      now,
	}
	if err := store.UpsertCluster(ctx, cluster); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	loaded, err := store.GetCluster(ctx, cluster.ID)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if loaded.Region != cluster.Region || len(loaded.IsolationTiers) != 2 || loaded.Labels["features"] != "race,coverage" {
		t.Fatalf("cluster mismatch: %+v", loaded)
	}
	if !loaded.LastHeartbeat.Equal(now) {
		t.Fatalf("heartbeat = %v, want %v", loaded.LastHeartbeat, now)
	}
	cluster.LastHeartbeat = now.Add(-time.Hour)
	cluster.QueueDepth = 1
	if err := store.UpsertCluster(ctx, cluster); err != nil {
		t.Fatalf("UpsertCluster stale heartbeat: %v", err)
	}
	loaded, err = store.GetCluster(ctx, cluster.ID)
	if err != nil {
		t.Fatalf("GetCluster after heartbeat: %v", err)
	}
	if !loaded.LastHeartbeat.Equal(now) {
		t.Fatalf("heartbeat regressed to %v, want %v", loaded.LastHeartbeat, now)
	}
	if loaded.QueueDepth != 1 {
		t.Fatalf("queue depth = %d, want 1", loaded.QueueDepth)
	}
	clusters, err := store.ListClusters(ctx, "region-a")
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	found := false
	for _, candidate := range clusters {
		if candidate.ID == cluster.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("cluster missing from region listing: %+v", clusters)
	}
	clusters, err = store.ListClusters(ctx, "region-z")
	if err != nil {
		t.Fatalf("ListClusters empty region: %v", err)
	}
	for _, candidate := range clusters {
		if candidate.ID == cluster.ID {
			t.Fatalf("cluster appeared in wrong region listing: %+v", candidate)
		}
	}
	if err := store.DeleteCluster(ctx, cluster.ID); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if _, err := store.GetCluster(ctx, cluster.ID); err == nil {
		t.Fatal("expected deleted cluster to be gone")
	} else if !domain.IsNotFound(err) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}

	claimed, err := store.AcquireShardLeases(ctx, []int{0, 1, 2}, "owner-a", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireShardLeases: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claimed = %v, want [0 1 2]", claimed)
	}
	claimed, err = store.AcquireShardLeases(ctx, []int{1, 2, 3}, "owner-b", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireShardLeases second owner: %v", err)
	}
	if len(claimed) != 1 || claimed[0] != 3 {
		t.Fatalf("claimed = %v, want [3]", claimed)
	}
	claimed, err = store.AcquireShardLeases(ctx, []int{1, 2}, "owner-a", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireShardLeases own leases: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed own leases = %v, want [1 2]", claimed)
	}
	claimed, err = store.AcquireShardLeases(ctx, []int{4, 4}, "owner-c", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireShardLeases duplicates: %v", err)
	}
	if len(claimed) != 1 || claimed[0] != 4 {
		t.Fatalf("claimed duplicates = %v, want [4]", claimed)
	}
	if err := store.ReleaseShardLeases(ctx, "owner-a"); err != nil {
		t.Fatalf("ReleaseShardLeases: %v", err)
	}
	claimed, err = store.AcquireShardLeases(ctx, []int{1, 2}, "owner-b", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireShardLeases after release: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed after release = %v, want [1 2]", claimed)
	}
	if err := store.ReleaseShardLeases(ctx, "owner-b"); err != nil {
		t.Fatalf("ReleaseShardLeases owner-b: %v", err)
	}
	if err := store.ReleaseShardLeases(ctx, "owner-c"); err != nil {
		t.Fatalf("ReleaseShardLeases owner-c: %v", err)
	}

	if err := store.AppendAudit(ctx, domain.AuditEntry{
		ID:         uuid.NewString(),
		TenantID:   tenant.ID,
		Actor:      "tester",
		Action:     "run.create",
		Resource:   "run",
		ResourceID: project.ID.String(),
		Outcome:    "allow",
		Detail:     "integration test",
		At:         now,
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
}

func TestIntegrationRunListingFreezeAndReady(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	base := time.Now().UTC().Add(-time.Hour)

	created := make([]domain.Run, 0, 3)
	for i := 0; i < 3; i++ {
		record := newRunRecord(t, tenant, project, "listing-"+uuid.NewString(), "fingerprint-"+uuid.NewString())
		record.Run.CreatedAt = base.Add(time.Duration(i) * time.Second)
		record.Run.UpdatedAt = record.Run.CreatedAt
		run, err := store.CreateRun(ctx, record)
		if err != nil {
			t.Fatalf("CreateRun %d: %v", i, err)
		}
		created = append(created, run)
	}
	firstPage, err := store.ListRuns(ctx, domain.RunFilter{TenantID: tenant.ID, ProjectID: project.ID, Limit: 2})
	if err != nil {
		t.Fatalf("ListRuns first page: %v", err)
	}
	if len(firstPage.Runs) != 2 || firstPage.NextCursor == "" {
		t.Fatalf("first page = %+v", firstPage)
	}
	if firstPage.Runs[0].ID != created[2].ID || firstPage.Runs[1].ID != created[1].ID {
		t.Fatalf("first page order wrong: %s %s", firstPage.Runs[0].ID, firstPage.Runs[1].ID)
	}
	secondPage, err := store.ListRuns(ctx, domain.RunFilter{
		TenantID: tenant.ID, ProjectID: project.ID, Limit: 2, Cursor: firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListRuns second page: %v", err)
	}
	if len(secondPage.Runs) != 1 || secondPage.Runs[0].ID != created[0].ID || secondPage.NextCursor != "" {
		t.Fatalf("second page = %+v", secondPage)
	}
	byStatus, err := store.ListRuns(ctx, domain.RunFilter{
		TenantID: tenant.ID, Statuses: []domain.RunStatus{domain.RunCreated}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListRuns status filter: %v", err)
	}
	if len(byStatus.Runs) != 3 {
		t.Fatalf("status filter returned %d runs", len(byStatus.Runs))
	}

	freezeTarget := newRunRecord(t, tenant, project, "freeze-"+uuid.NewString(), "freeze-fingerprint")
	frozenRun, err := store.CreateRun(ctx, freezeTarget)
	if err != nil {
		t.Fatalf("CreateRun freeze target: %v", err)
	}
	frozen, err := store.FreezeRunInputs(ctx, domain.RunFreeze{
		TenantID:        tenant.ID,
		RunID:           frozenRun.ID,
		ExpectedVersion: frozenRun.StateVersion,
		SnapshotDigest:  "sha256:" + strings.Repeat("f", 64),
		BaseSnapshot:    "sha256:" + strings.Repeat("1", 64),
		HeadSnapshot:    "sha256:" + strings.Repeat("2", 64),
		At:              time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("FreezeRunInputs: %v", err)
	}
	if frozen.SnapshotDigest == "" || frozen.BaseSnapshot == "" || frozen.HeadSnapshot == "" {
		t.Fatalf("frozen digests not stored: %+v", frozen)
	}
	if frozen.StateVersion != frozenRun.StateVersion+1 {
		t.Fatalf("freeze state version = %d, want %d", frozen.StateVersion, frozenRun.StateVersion+1)
	}
	if _, err := store.FreezeRunInputs(ctx, domain.RunFreeze{
		TenantID: tenant.ID, RunID: frozenRun.ID, ExpectedVersion: frozenRun.StateVersion,
		SnapshotDigest: "sha256:" + strings.Repeat("e", 64), At: time.Now().UTC(),
	}); !domain.IsVersionConflict(err) {
		t.Fatalf("expected freeze version conflict, got %v", err)
	}
	validating := transitionRun(t, store, frozen, domain.RunCreated, domain.RunValidating)

	readyA := newStep(created[0], "ready:two", domain.StepKindCompile, domain.StepReady, 2)
	readyB := newStep(created[0], "ready:three", domain.StepKindCompile, domain.StepReady, 3)
	planRun(t, store, created[0], []domain.Step{readyA, readyB}, nil, nil)
	modulo, err := store.ListReadySteps(ctx, domain.ReadyQuery{
		TenantID: tenant.ID, Shard: 0, ShardCount: 2, Limit: 10, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ListReadySteps modulo: %v", err)
	}
	if len(modulo) != 1 || modulo[0].ID != readyA.ID {
		t.Fatalf("modulo ready steps = %+v", modulo)
	}
	exact, err := store.ListReadySteps(ctx, domain.ReadyQuery{
		TenantID: tenant.ID, Shard: 3, Limit: 10, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ListReadySteps exact: %v", err)
	}
	if len(exact) != 1 || exact[0].ID != readyB.ID {
		t.Fatalf("exact ready steps = %+v", exact)
	}
	leaseAt := time.Now().UTC()
	lease := acquireLease(t, store, readyA, "renew-owner", 0, leaseAt)
	if err := store.RenewLease(ctx, domain.LeaseRenewal{
		Ref: lease.Ref(), Owner: "renew-owner", Extension: time.Minute, Now: leaseAt.Add(10 * time.Second),
	}); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	renewed, err := store.GetStep(ctx, tenant.ID, readyA.ID)
	if err != nil {
		t.Fatalf("GetStep renewed: %v", err)
	}
	if renewed.LeaseDeadline == nil || !renewed.LeaseDeadline.After(leaseAt.Add(30*time.Second)) {
		t.Fatalf("lease not renewed: %+v", renewed.LeaseDeadline)
	}
	if _, err := store.PlanRun(ctx, domain.PlanTransaction{
		TenantID: tenant.ID, RunID: frozenRun.ID, ExpectedVersion: validating.StateVersion,
		Steps: []domain.Step{newStep(validating, "late", domain.StepKindCompile, domain.StepReady, 0)},
		At:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PlanRun frozen run: %v", err)
	}
	if _, err := store.FreezeRunInputs(ctx, domain.RunFreeze{
		TenantID: tenant.ID, RunID: frozenRun.ID, ExpectedVersion: validating.StateVersion + 1,
		SnapshotDigest: "sha256:" + strings.Repeat("d", 64), At: time.Now().UTC(),
	}); err == nil {
		t.Fatal("expected freeze after planning to be rejected")
	} else {
		var transition *domain.StateTransitionError
		if !errors.As(err, &transition) {
			t.Fatalf("expected StateTransitionError, got %T: %v", err, err)
		}
	}
}

func TestIntegrationReadyDefaultsAndLeaseBoundaries(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tenant, project := seedTenantProject(t, store)
	created, err := store.CreateRun(ctx, newRunRecord(t, tenant, project, "boundary-request", "boundary-fingerprint"))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	low := newStep(created, "ready:low", domain.StepKindCompile, domain.StepReady, 0)
	low.Priority = 1
	low.Requirements.Region = "us-east-1"
	high := newStep(created, "ready:high", domain.StepKindDifferential, domain.StepReady, 1)
	high.Priority = 9
	high.Requirements.Region = "us-east-1"
	high2 := newStep(created, "ready:high2", domain.StepKindRegression, domain.StepReady, 2)
	high2.Priority = 9
	high2.Requirements.Region = "us-east-1"
	planned := planRun(t, store, created, []domain.Step{low, high, high2}, nil, nil)

	all, err := store.ListReadySteps(ctx, domain.ReadyQuery{TenantID: tenant.ID, Limit: 10, Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("ListReadySteps default: %v", err)
	}
	if len(all) != 3 || all[0].Priority != 9 || all[2].Priority != 1 {
		t.Fatalf("default ready listing = %+v", all)
	}
	sharded, err := store.ListReadySteps(ctx, domain.ReadyQuery{
		TenantID: tenant.ID, Shard: 1, ShardCount: 2, Limit: 10, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ListReadySteps sharded: %v", err)
	}
	if len(sharded) != 1 || sharded[0].ID != high.ID {
		t.Fatalf("sharded ready listing = %+v", sharded)
	}
	region, err := store.ListReadySteps(ctx, domain.ReadyQuery{
		TenantID: tenant.ID, Region: "us-east-1", Limit: 10, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ListReadySteps region: %v", err)
	}
	if len(region) != 3 {
		t.Fatalf("region ready listing = %+v", region)
	}

	leaseAt := time.Now().UTC()
	lease := acquireLease(t, store, high, "boundary-owner", 0, leaseAt)
	if err := store.RenewLease(ctx, domain.LeaseRenewal{
		Ref: lease.Ref(), Owner: "boundary-owner", Extension: time.Minute, Now: leaseAt.Add(30 * time.Second),
	}); err == nil {
		t.Fatal("expected renewal at the lease deadline to fail")
	}
	if err := store.RenewLease(ctx, domain.LeaseRenewal{
		Ref: lease.Ref(), Owner: "boundary-owner", Extension: time.Minute, Now: leaseAt.Add(10 * time.Second),
	}); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	renewed, err := store.GetStep(ctx, tenant.ID, high.ID)
	if err != nil {
		t.Fatalf("GetStep renewed: %v", err)
	}
	wantDeadline := leaseAt.Add(10*time.Second + time.Minute)
	if renewed.LeaseDeadline == nil || !renewed.LeaseDeadline.Equal(wantDeadline) {
		t.Fatalf("renewed deadline = %v, want %v", renewed.LeaseDeadline, wantDeadline)
	}

	expiring := low
	expiringLease := acquireLease(t, store, expiring, "expiry-owner", 0, leaseAt)
	expiries, err := store.ExpireLeases(ctx, expiringLease.ExpiresAt, 100)
	if err != nil {
		t.Fatalf("ExpireLeases at deadline: %v", err)
	}
	found := false
	for _, expiry := range expiries {
		if expiry.StepID == expiring.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("step %s was not expired at its deadline", expiring.ID)
	}

	cancelling, err := store.UpdateRun(ctx, domain.RunUpdate{
		TenantID: tenant.ID, RunID: created.ID, ExpectedVersion: planned.StateVersion,
		From: domain.RunPlanned, To: domain.RunCancelling, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("UpdateRun cancelling: %v", err)
	}
	if cancelling.Status != domain.RunCancelling {
		t.Fatalf("run status = %s", cancelling.Status)
	}
	_, err = store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID:           tenant.ID,
		RunID:              created.ID,
		StepID:             expiring.ID,
		AttemptID:          domain.NewAttemptID(),
		ExpectedGeneration: expiringLease.Generation,
		Owner:              "late-owner",
		TTL:                30 * time.Second,
		Now:                time.Now().UTC(),
	})
	var leaseErr *domain.LeaseError
	if !errors.As(err, &leaseErr) {
		t.Fatalf("expected LeaseError for cancelling run, got %T: %v", err, err)
	}
}
