package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	memorystore "github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/app/tokens"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

func TestDeclaredInputsResolveDependencyOutput(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	artifacts := casmemory.New()
	tenantID := domain.NewTenantID()
	projectID := domain.NewProjectID()
	now := time.Now().UTC()
	if err := store.CreateTenant(ctx, domain.Tenant{
		ID: tenantID, HomeRegion: "local", SecurityTier: domain.SecurityTierStandard,
		Status: domain.TenantActive, ActiveWriterEpoch: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: domain.NewRunID(), TenantID: tenantID, ProjectID: projectID,
		RequestKey: "inputs", Fingerprint: "fp", BaseCommit: strings.Repeat("a", 7),
		HeadCommit: strings.Repeat("b", 7), PolicyDigest: "sha256:" + strings.Repeat("c", 64),
		Status: domain.RunValidating, Region: "local", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.CreateRun(ctx, domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatal(err)
	}
	impactRef, err := artifacts.Put(ctx, ports.SecurityScope{TenantID: tenantID, TrustDomain: "run", Region: "local"}, strings.NewReader(`{"affected_packages":["example.com/mod/pkg"]}`))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := domain.EncodeOutputManifest(map[string]string{"impact.digest": impactRef.Hex()}, []domain.CASRef{impactRef})
	if err != nil {
		t.Fatal(err)
	}
	impact := domain.Step{
		ID: domain.NewStepID(), RunID: run.ID, TenantID: tenantID, Key: "impact",
		Kind: domain.StepKindImpact, Status: domain.StepSucceeded, MaxAttempts: 1,
		OutputManifest: manifest, CreatedAt: now, UpdatedAt: now,
		Requirements: inputRequirements(),
	}
	regression := domain.Step{
		ID: domain.NewStepID(), RunID: run.ID, TenantID: tenantID, Key: "regression",
		Kind: domain.StepKindRegression, Status: domain.StepBlocked, MaxAttempts: 1,
		Options:   map[string]string{stepopts.InputPrefix + "impact": "impact/impact.digest"},
		CreatedAt: now, UpdatedAt: now,
		Requirements: inputRequirements(),
	}
	if _, err := store.PlanRun(ctx, domain.PlanTransaction{
		TenantID: tenantID, RunID: run.ID, ExpectedVersion: 0,
		Steps: []domain.Step{impact, regression},
		Dependencies: map[domain.StepID][]domain.StepID{
			regression.ID: {impact.ID},
		},
		At: now,
	}); err != nil {
		t.Fatal(err)
	}
	issuer, err := tokens.NewIssuer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(store, fakeFactory{executor: &fakeExecutor{}}, ScorePlacement{}, issuer, artifacts, Config{
		Region: "local", OwnerID: "inputs", ShardCount: 1, LeaseTTL: time.Minute,
		ShardLeaseTTL: time.Minute, PollInterval: time.Second, Logger: nil,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := service.declaredInputs(ctx, run, regression)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := inputs["impact"]
	if !ok {
		t.Fatalf("impact input was not resolved: %+v", inputs)
	}
	if binding.Kind != "file" || binding.Ref.Digest != impactRef.Digest {
		t.Fatalf("binding = %+v, want %+v", binding, impactRef)
	}
}

func inputRequirements() domain.ResourceRequirements {
	return domain.ResourceRequirements{
		Architecture: "amd64", Isolation: domain.IsolationContainer, CPUMilli: 100, MemoryMiB: 128,
		Network: domain.NetworkNone, SubjectNetwork: domain.NetworkNone,
		DeadlineSeconds: 60, ArtifactTransport: domain.TransportDirect,
	}
}
