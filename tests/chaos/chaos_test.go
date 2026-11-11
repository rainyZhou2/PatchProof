package chaos

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memorystore "github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
)

const (
	tenantID = domain.TenantID("11111111-1111-4111-8111-111111111111")
	runID    = domain.RunID("22222222-2222-4222-8222-222222222222")
	stepID   = domain.StepID("33333333-3333-4333-8333-333333333333")
)

func setup(t *testing.T) *memorystore.Store {
	t.Helper()
	ctx := context.Background()
	store := memorystore.New()
	now := time.Now().UTC()
	tenant := domain.Tenant{
		ID: tenantID, HomeRegion: "local", SecurityTier: domain.SecurityTierStandard, Status: domain.TenantActive,
		ActiveWriterEpoch: 1, Quota: domain.TenantQuota{MaxConcurrentSteps: 16}, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, TenantID: tenantID, ProjectID: "44444444-4444-4444-8444-444444444444",
		RequestKey: "chaos", Fingerprint: "fp", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PolicyDigest: "sha256:abc",
		Status: domain.RunValidating, Region: "local", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.CreateRun(ctx, domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatal(err)
	}
	step := domain.Step{
		ID: stepID, RunID: runID, TenantID: tenantID, Key: "compile", Kind: domain.StepKindCompile,
		Status: domain.StepReady, MaxAttempts: 3, Shard: 0,
		Requirements: domain.ResourceRequirements{
			Region: "local", Architecture: "amd64", Isolation: domain.IsolationTrustedLocal, CPUMilli: 100,
			MemoryMiB: 128, EphemeralDiskGiB: 1, Network: domain.NetworkNone, DeadlineSeconds: 60,
			ArtifactTransport: domain.TransportDirect,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PlanRun(ctx, domain.PlanTransaction{
		TenantID: tenantID, RunID: runID, ExpectedVersion: 0, Steps: []domain.Step{step},
		Dependencies: map[domain.StepID][]domain.StepID{}, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRun(ctx, domain.RunUpdate{
		TenantID: tenantID, RunID: runID, ExpectedVersion: current.StateVersion,
		From: domain.RunPlanned, To: domain.RunQueued, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func lease(store *memorystore.Store, attemptID domain.AttemptID, expected int64) (domain.Lease, error) {
	return store.AcquireLease(context.Background(), domain.LeaseRequest{
		TenantID: tenantID, RunID: runID, StepID: stepID, AttemptID: attemptID,
		ExpectedGeneration: expected, Owner: "chaos", TTL: time.Minute, ClusterID: "local", Now: time.Now().UTC(),
	})
}

func TestConcurrentLeaseHasSingleWinner(t *testing.T) {
	store := setup(t)
	var winners atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := lease(store, domain.NewAttemptID(), 0); err != nil {
				var leaseErr *domain.LeaseError
				if errors.As(err, &leaseErr) {
					failures.Add(1)
					return
				}
				t.Errorf("unexpected error: %v", err)
				return
			}
			winners.Add(1)
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("expected exactly one lease winner, got %d", winners.Load())
	}
	if failures.Load() != 31 {
		t.Fatalf("expected 31 lease rejections, got %d", failures.Load())
	}
	step, err := store.GetStep(context.Background(), tenantID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Generation != 1 || step.AttemptCount != 1 || step.Status != domain.StepLeased {
		t.Fatalf("unexpected step state: %+v", step)
	}
}

func TestDuplicateNotificationDoesNotDoubleLease(t *testing.T) {
	store := setup(t)
	first, err := lease(store, domain.NewAttemptID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease(store, domain.NewAttemptID(), 0); err == nil {
		t.Fatal("second lease for the same generation succeeded")
	}
	step, err := store.GetStep(context.Background(), tenantID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	if step.CurrentAttemptID != first.AttemptID {
		t.Fatal("current attempt was replaced")
	}
}

func TestExpiredWorkerResultIsFenced(t *testing.T) {
	store := setup(t)
	now := time.Now().UTC()
	first, err := store.AcquireLease(context.Background(), domain.LeaseRequest{
		TenantID: tenantID, RunID: runID, StepID: stepID, AttemptID: domain.NewAttemptID(),
		ExpectedGeneration: 0, Owner: "chaos", TTL: 10 * time.Second, ClusterID: "local", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpireLeases(context.Background(), now.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].Final {
		t.Fatalf("expected one retryable expiry, got %#v", expired)
	}
	if _, err := store.PromoteDueSteps(context.Background(), now.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	second, err := lease(store, domain.NewAttemptID(), first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 {
		t.Fatalf("expected generation %d, got %d", first.Generation+1, second.Generation)
	}
	result := domain.StepResult{
		Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("a", 64), FinishedAt: now,
	}
	_, err = store.CommitAttempt(context.Background(), domain.AttemptCommit{Ref: first.Ref(), Result: result, At: now.Add(2 * time.Minute)})
	var fencing *domain.FencingError
	if !errors.As(err, &fencing) {
		t.Fatalf("expected a fencing error, got %v", err)
	}
	if _, err := store.CommitAttempt(context.Background(), domain.AttemptCommit{Ref: second.Ref(), Result: result, At: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	step, err := store.GetStep(context.Background(), tenantID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Status != domain.StepSucceeded {
		t.Fatalf("expected SUCCEEDED, got %s", step.Status)
	}
}
