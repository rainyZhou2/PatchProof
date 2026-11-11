package scheduler

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/actioncache"
	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	memorystore "github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/app/tokens"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type cacheFixture struct {
	store      *memorystore.Store
	tenantID   domain.TenantID
	runID      domain.RunID
	stepID     domain.StepID
	run        domain.Run
	step       domain.Step
	executor   *fakeExecutor
	scheduler  *Service
	cacheEpoch string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	store := memorystore.New()
	artifacts := casmemory.New()
	tenantID := domain.TenantID("11111111-1111-4111-8111-111111111111")
	now := time.Now().UTC()
	scope := ports.SecurityScope{TenantID: tenantID, TrustDomain: "run", Region: "local"}
	baseArchive, err := artifacts.Put(context.Background(), scope, strings.NewReader("base snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	headArchive, err := artifacts.Put(context.Background(), scope, strings.NewReader("head snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	tenant := domain.Tenant{
		ID: tenantID, HomeRegion: "local", SecurityTier: domain.SecurityTierStandard, Status: domain.TenantActive,
		ActiveWriterEpoch: 1, Quota: domain.TenantQuota{MaxConcurrentSteps: 8}, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateTenant(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "22222222-2222-4222-8222-222222222222", TenantID: tenantID, ProjectID: "33333333-3333-4333-8333-333333333333",
		RequestKey: "cache-test", Fingerprint: "fp", BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40),
		SnapshotDigest: "sha256:" + strings.Repeat("c", 64), BaseSnapshot: baseArchive.URI(),
		HeadSnapshot: headArchive.URI(), PolicyDigest: "sha256:" + strings.Repeat("f", 64),
		Status: domain.RunValidating, Region: "local", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.CreateRun(context.Background(), domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatal(err)
	}
	step := domain.Step{
		ID: "44444444-4444-4444-8444-444444444444", RunID: run.ID, TenantID: tenantID, Key: "compile-head",
		Kind: domain.StepKindCompile, Status: domain.StepReady, MaxAttempts: 2, Shard: 0, Priority: 10,
		Options: map[string]string{"cache.enabled": "true", "target": "head", "claim.id": "compile"},
		Requirements: domain.ResourceRequirements{
			Region: "local", Architecture: "amd64", Engine: "go-build", Language: "go",
			Isolation: domain.IsolationTrustedLocal, CPUMilli: 100, MemoryMiB: 128, EphemeralDiskGiB: 1,
			Network: domain.NetworkNone, SubjectNetwork: domain.NetworkNone, DeadlineSeconds: 60,
			ArtifactTransport: domain.TransportDirect,
		},
		InputManifestDigest: "sha256:" + strings.Repeat("1", 64),
		CreatedAt:           now, UpdatedAt: now,
	}
	if _, err := store.PlanRun(context.Background(), domain.PlanTransaction{
		TenantID: tenantID, RunID: run.ID, ExpectedVersion: 0, Steps: []domain.Step{step},
		Dependencies: map[domain.StepID][]domain.StepID{}, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRun(context.Background(), tenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	run = current
	if _, err := store.UpdateRun(context.Background(), domain.RunUpdate{
		TenantID: tenantID, RunID: run.ID, ExpectedVersion: current.StateVersion,
		From: domain.RunPlanned, To: domain.RunQueued, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	cluster := domain.Cluster{
		ID: "fake", Region: "local", ExecutorKind: "fake", Architectures: []string{"amd64"}, Languages: []string{"go"},
		Engines: []string{"go-build"}, IsolationTiers: []domain.IsolationTier{domain.IsolationTrustedLocal},
		AvailableCPUMilli: 10000, AvailableMemoryMiB: 10000, MaxConcurrentSteps: 10, Healthy: true, LastHeartbeat: now,
	}
	if err := store.UpsertCluster(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	issuer, err := tokens.NewIssuer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &cacheFixture{
		store: store, tenantID: tenantID, runID: run.ID, stepID: step.ID, run: run, step: step,
		executor: &fakeExecutor{}, cacheEpoch: "cache-epoch-1",
	}
	service, err := New(store, fakeFactory{executor: fixture.executor}, ScorePlacement{}, issuer, artifacts, Config{
		Region: "local", OwnerID: "cache-test", ShardCount: 1, BatchSize: 4, LeaseTTL: 30 * time.Second,
		ShardLeaseTTL: 10 * time.Second, PollInterval: 50 * time.Millisecond, MaxStepsPerTenant: 4,
		MaxStepsPerCluster: 4, MaxRunWallSeconds: 600, AttemptTokenTTL: 5 * time.Minute,
		ActionCacheEnabled: true, ActionCacheTTL: time.Hour, CacheEpoch: fixture.cacheEpoch, WorkerImage: "worker@sha256:test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.scheduler = service
	return fixture
}

func (f *cacheFixture) key(t *testing.T) string {
	t.Helper()
	key, err := actioncache.KeyForStep(f.step, f.run, f.cacheEpoch, "worker@sha256:test")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (f *cacheFixture) waitSucceeded(t *testing.T, ctx context.Context) domain.Step {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		step, err := f.store.GetStep(ctx, f.tenantID, f.stepID)
		if err != nil {
			t.Fatal(err)
		}
		if step.Status == domain.StepSucceeded {
			return step
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("step did not succeed")
	return domain.Step{}
}

func TestSchedulerServesActionCacheHitWithoutExecutor(t *testing.T) {
	fixture := newCacheFixture(t)
	now := time.Now().UTC()
	entry, err := actioncache.NewEntry(
		fixture.tenantID, fixture.key(t), fixture.step, "go-build@1",
		domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("9", 64)},
		time.Hour, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutActionCache(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = fixture.scheduler.Run(ctx) }()
	fixture.waitSucceeded(t, ctx)
	fixture.executor.mu.Lock()
	starts := fixture.executor.starts
	fixture.executor.mu.Unlock()
	if starts != 0 {
		t.Fatalf("executor started %d times on a cache hit", starts)
	}
	step, err := fixture.store.GetStep(ctx, fixture.tenantID, fixture.stepID)
	if err != nil {
		t.Fatal(err)
	}
	if step.AttemptCount != 1 {
		t.Fatalf("expected one cache attempt, got %d", step.AttemptCount)
	}
}

func TestSchedulerStoresActionResultAfterExecution(t *testing.T) {
	fixture := newCacheFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = fixture.scheduler.Run(ctx) }()
	fixture.waitSucceeded(t, ctx)
	key := fixture.key(t)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entry, err := fixture.store.GetActionCache(ctx, fixture.tenantID, key, time.Now().UTC())
		if err == nil {
			if entry.Result.Outcome != domain.OutcomeSucceeded {
				t.Fatalf("unexpected cached outcome %s", entry.Result.Outcome)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("result was not written to the action cache")
}
