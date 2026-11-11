package domain_test

import (
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	testTenantID = domain.TenantID("11111111-1111-4111-8111-111111111111")
	testTenantB  = domain.TenantID("22222222-2222-4222-8222-222222222222")
	testProject  = domain.ProjectID("33333333-3333-4333-8333-333333333333")
	testRunID    = domain.RunID("44444444-4444-4444-8444-444444444444")
	testRunB     = domain.RunID("55555555-5555-4555-8555-555555555555")
	testStep1    = domain.StepID("66666666-6666-4666-8666-666666666666")
	testStep2    = domain.StepID("77777777-7777-4777-8777-777777777777")
	testStep3    = domain.StepID("88888888-8888-4888-8888-888888888888")
	testStep4    = domain.StepID("99999999-9999-4999-8999-999999999999")
	testAttempt1 = domain.AttemptID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	testAttempt2 = domain.AttemptID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	testEvidence = domain.EvidenceID("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	testReportID = domain.ReportID("dddddddd-dddd-4ddd-8ddd-dddddddddddd")

	digestHexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestHexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestHexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	digestHexD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	digestA = "sha256:" + digestHexA
	digestB = "sha256:" + digestHexB
	digestC = "sha256:" + digestHexC
	digestD = "sha256:" + digestHexD
)

var (
	oldTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	newTime = oldTime.Add(90 * time.Minute)
)

func testRequirements() domain.ResourceRequirements {
	return domain.ResourceRequirements{
		Architecture:    "amd64",
		Isolation:       domain.IsolationContainer,
		CPUMilli:        1000,
		MemoryMiB:       2048,
		Network:         domain.NetworkNone,
		DeadlineSeconds: 300,
	}
}

func makeStep(id domain.StepID, runID domain.RunID, tenantID domain.TenantID, key string) domain.Step {
	return domain.Step{
		ID:                  id,
		RunID:               runID,
		TenantID:            tenantID,
		Key:                 key,
		Kind:                domain.StepKindCompile,
		Status:              domain.StepReady,
		MaxAttempts:         2,
		Requirements:        testRequirements(),
		InputManifestDigest: digestA,
		CreatedAt:           oldTime,
		UpdatedAt:           oldTime,
	}
}

func makeClaim(id domain.ClaimID, runID domain.RunID, tenantID domain.TenantID) domain.Claim {
	return domain.Claim{
		ID:            id,
		RunID:         runID,
		TenantID:      tenantID,
		Kind:          domain.ClaimKindTest,
		Title:         "claim " + string(id),
		Predicate:     "predicate",
		Domain:        "test domain",
		MinimumResult: domain.ClaimTested,
		Required:      true,
		CreatedAt:     oldTime,
	}
}

func makeEvidence(id domain.EvidenceID, runID domain.RunID, tenantID domain.TenantID, claimID domain.ClaimID) domain.Evidence {
	return domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            id,
		TenantID:      tenantID,
		RunID:         runID,
		ClaimID:       claimID,
		Kind:          domain.EvidenceCompile,
		Result:        domain.ClaimTested,
		Scope:         domain.EvidenceScope{InputDomain: "declared domain"},
		Engine:        domain.EngineInfo{Name: "go-build", Version: "1.0.0"},
		Environment:   domain.EnvironmentInfo{Arch: "amd64", SandboxProfile: "isolated-container"},
		CreatedAt:     oldTime,
	}
}

func intPtr(v int) *int { return &v }
