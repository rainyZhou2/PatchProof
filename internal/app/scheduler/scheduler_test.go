package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	memorystore "github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/app/tokens"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type fakeExecutor struct {
	mu      sync.Mutex
	starts  int
	spec    domain.StepSpec
	started bool
}

func (f *fakeExecutor) Capabilities(context.Context) (ports.ExecutorCapabilities, error) {
	return ports.ExecutorCapabilities{Name: "fake"}, nil
}
func (f *fakeExecutor) Start(_ context.Context, request ports.ExecutionRequest) (ports.ExecutionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.spec = request.Spec
	f.started = true
	return ports.ExecutionRef{Executor: "fake", ClusterID: "fake", ExternalID: "exec-1"}, nil
}
func (f *fakeExecutor) Observe(context.Context, ports.ExecutionRef) (ports.ExecutionObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return ports.ExecutionObservation{State: ports.ExecutionPending}, nil
	}
	result := domain.StepResult{
		Outcome:      domain.OutcomeSucceeded,
		ResultDigest: "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FinishedAt:   time.Now().UTC(),
	}
	return ports.ExecutionObservation{State: ports.ExecutionCompleted, Result: &result}, nil
}
func (f *fakeExecutor) Cancel(context.Context, ports.ExecutionRef) error { return nil }

type fakeFactory struct{ executor *fakeExecutor }

func (f fakeFactory) ForCluster(domain.Cluster) (ports.Executor, error) { return f.executor, nil }

func TestSchedulerLeasesAndCommitsLocalExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := memorystore.New()
	tenant := domain.Tenant{ID: "11111111-1111-4111-8111-111111111111", HomeRegion: "local", SecurityTier: domain.SecurityTierStandard, Status: domain.TenantActive, ActiveWriterEpoch: 1, Quota: domain.TenantQuota{MaxConcurrentSteps: 8}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID: "22222222-2222-4222-8222-222222222222", TenantID: tenant.ID, ProjectID: "33333333-3333-4333-8333-333333333333",
		RequestKey: "scheduler-test", Fingerprint: "fp", BaseCommit: string(make([]byte, 0)), HeadCommit: "",
		PolicyDigest: "sha256:abc", Status: domain.RunValidating, Region: "local", CreatedAt: now, UpdatedAt: now,
	}
	run.BaseCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run.HeadCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.CreateRun(ctx, domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatal(err)
	}
	step := domain.Step{
		ID: "44444444-4444-4444-8444-444444444444", RunID: run.ID, TenantID: tenant.ID, Key: "policy-check",
		Kind: domain.StepKindPolicy, Status: domain.StepReady, MaxAttempts: 2, Priority: 10, Shard: 0,
		Requirements: domain.ResourceRequirements{
			Region: "local", Architecture: "amd64", Isolation: domain.IsolationTrustedLocal,
			CPUMilli: 100, MemoryMiB: 128, EphemeralDiskGiB: 1, Network: domain.NetworkNone,
			DeadlineSeconds: 60, ArtifactTransport: domain.TransportDirect,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PlanRun(ctx, domain.PlanTransaction{TenantID: tenant.ID, RunID: run.ID, ExpectedVersion: 0, Steps: []domain.Step{step}, Dependencies: map[domain.StepID][]domain.StepID{}, At: now}); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRun(ctx, tenant.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRun(ctx, domain.RunUpdate{TenantID: tenant.ID, RunID: run.ID, ExpectedVersion: current.StateVersion, From: domain.RunPlanned, To: domain.RunQueued, At: now}); err != nil {
		t.Fatal(err)
	}
	cluster := domain.Cluster{
		ID: "fake", Region: "local", ExecutorKind: "fake", Architectures: []string{"amd64"}, Languages: []string{"go"},
		Engines: []string{"go-analyzer"}, IsolationTiers: []domain.IsolationTier{domain.IsolationTrustedLocal},
		AvailableCPUMilli: 10000, AvailableMemoryMiB: 10000, MaxConcurrentSteps: 10, Healthy: true, LastHeartbeat: now,
	}
	if err := store.UpsertCluster(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	issuer, err := tokens.NewIssuer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{}
	scheduler, err := New(store, fakeFactory{executor: executor}, ScorePlacement{}, issuer, casmemory.New(), Config{
		Region: "local", OwnerID: "test", ShardCount: 1, BatchSize: 4, LeaseTTL: 30 * time.Second,
		ShardLeaseTTL: 10 * time.Second, PollInterval: 50 * time.Millisecond,
		MaxStepsPerTenant: 4, MaxStepsPerCluster: 4, MaxRunWallSeconds: 600,
		AttemptTokenTTL: 5 * time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = scheduler.Run(ctx) }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.GetStep(ctx, tenant.ID, step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == domain.StepSucceeded {
			executor.mu.Lock()
			defer executor.mu.Unlock()
			if executor.starts != 1 {
				t.Fatalf("expected one executor start, got %d", executor.starts)
			}
			if executor.spec.AttemptToken == "" {
				t.Fatal("attempt token was not issued")
			}
			if executor.spec.Generation != current.Generation {
				t.Fatalf("spec generation %d does not match step %d", executor.spec.Generation, current.Generation)
			}
			if current.AttemptCount != 1 {
				t.Fatalf("expected one attempt, got %d", current.AttemptCount)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("step was not committed by the scheduler")
}
