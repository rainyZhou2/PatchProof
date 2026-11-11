package domain_test

import (
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
)

func validEvidence() domain.Evidence {
	return makeEvidence(testEvidence, testRunID, testTenantID, "claim-a")
}

func TestEvidenceValidateHappyPath(t *testing.T) {
	if err := validEvidence().Validate(); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
}

func TestEvidenceValidateFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.Evidence)
	}{
		{"missing schema", func(e *domain.Evidence) { e.SchemaVersion = "" }},
		{"wrong schema", func(e *domain.Evidence) { e.SchemaVersion = "patchproof.evidence/v2" }},
		{"invalid evidence id", func(e *domain.Evidence) { e.ID = "" }},
		{"invalid run id", func(e *domain.Evidence) { e.RunID = "" }},
		{"invalid tenant id", func(e *domain.Evidence) { e.TenantID = "" }},
		{"invalid claim id", func(e *domain.Evidence) { e.ClaimID = "bad claim!" }},
		{"missing claim id", func(e *domain.Evidence) { e.ClaimID = "" }},
		{"invalid kind", func(e *domain.Evidence) { e.Kind = domain.EvidenceKind("BOGUS") }},
		{"invalid result", func(e *domain.Evidence) { e.Result = domain.ClaimResult("BOGUS") }},
		{"missing scope", func(e *domain.Evidence) { e.Scope.InputDomain = "" }},
		{"blank scope", func(e *domain.Evidence) { e.Scope.InputDomain = "   " }},
		{"missing engine name", func(e *domain.Evidence) { e.Engine.Name = "" }},
		{"missing engine version", func(e *domain.Evidence) { e.Engine.Version = "" }},
		{"missing architecture", func(e *domain.Evidence) { e.Environment.Arch = "" }},
		{"missing sandbox profile", func(e *domain.Evidence) { e.Environment.SandboxProfile = "" }},
		{"invalid input CAS ref", func(e *domain.Evidence) {
			e.Inputs = []domain.CASRef{{Algorithm: "sha256", Digest: "short"}}
		}},
		{"invalid artifact CAS ref", func(e *domain.Evidence) {
			e.Artifacts = []domain.CASRef{{Algorithm: "sha1", Digest: digestHexA}}
		}},
		{"violated without artifacts", func(e *domain.Evidence) {
			e.Result = domain.ClaimViolated
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence := validEvidence()
			tc.mutate(&evidence)
			if err := evidence.Validate(); err == nil {
				t.Fatalf("invalid evidence accepted: %+v", evidence)
			}
		})
	}
}

func TestEvidenceViolationRequiresArtifact(t *testing.T) {
	evidence := validEvidence()
	evidence.Result = domain.ClaimViolated
	if err := evidence.Validate(); err == nil {
		t.Fatal("violated evidence without artifact accepted")
	}
	evidence.Artifacts = []domain.CASRef{domain.NewCASRef(digestHexB, 12)}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("violated evidence with artifact rejected: %v", err)
	}
}

func TestEvidenceAcceptsAllValidKindsAndResults(t *testing.T) {
	kinds := []domain.EvidenceKind{
		domain.EvidenceCompile, domain.EvidenceStatic, domain.EvidenceDifferential,
		domain.EvidenceRegression, domain.EvidenceFuzz, domain.EvidenceSMT,
		domain.EvidencePolicy, domain.EvidenceCoverage, domain.EvidenceEngineCrash,
		domain.EvidenceProvenance,
	}
	results := []domain.ClaimResult{
		domain.ClaimProven, domain.ClaimTested, domain.ClaimUnknown, domain.ClaimNotRun,
	}
	for _, kind := range kinds {
		for _, result := range results {
			evidence := validEvidence()
			evidence.Kind = kind
			evidence.Result = result
			if err := evidence.Validate(); err != nil {
				t.Errorf("kind %s result %s rejected: %v", kind, result, err)
			}
		}
	}
}

func TestClaimValidate(t *testing.T) {
	claim := makeClaim("claim-a", testRunID, testTenantID)
	if err := claim.Validate(); err != nil {
		t.Fatalf("valid claim rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*domain.Claim)
	}{
		{"invalid id", func(c *domain.Claim) { c.ID = "bad claim!" }},
		{"empty run", func(c *domain.Claim) { c.RunID = "" }},
		{"empty tenant", func(c *domain.Claim) { c.TenantID = "" }},
		{"invalid kind", func(c *domain.Claim) { c.Kind = domain.ClaimKind("BOGUS") }},
		{"empty title", func(c *domain.Claim) { c.Title = "" }},
		{"empty domain", func(c *domain.Claim) { c.Domain = "" }},
		{"invalid minimum", func(c *domain.Claim) { c.MinimumResult = domain.ClaimResult("BOGUS") }},
		{"violated minimum", func(c *domain.Claim) { c.MinimumResult = domain.ClaimViolated }},
		{"invalid evidence minimum", func(c *domain.Claim) { c.MinimumResult = domain.ClaimInvalidEvidence }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claim := makeClaim("claim-a", testRunID, testTenantID)
			tc.mutate(&claim)
			if err := claim.Validate(); err == nil {
				t.Fatalf("invalid claim accepted: %+v", claim)
			}
		})
	}
}

func TestCounterexampleValidate(t *testing.T) {
	ref := domain.NewCASRef(digestHexA, 5)
	counterexample := domain.Counterexample{
		CaseID:             "ce-1",
		TenantID:           testTenantID,
		RunID:              testRunID,
		ClaimID:            "claim-a",
		InputCodec:         "json",
		InputRef:           ref,
		BaseObservationRef: ref,
		HeadObservationRef: ref,
		Reproduced:         true,
		Scope:              "input domain",
	}
	if err := counterexample.Validate(); err != nil {
		t.Fatalf("valid counterexample rejected: %v", err)
	}
	counterexample.CaseID = ""
	if err := counterexample.Validate(); err == nil {
		t.Fatal("counterexample without case id accepted")
	}
	counterexample.CaseID = "ce-1"
	counterexample.ClaimID = "bad claim!"
	if err := counterexample.Validate(); err == nil {
		t.Fatal("counterexample with invalid claim id accepted")
	}
	counterexample.ClaimID = "claim-a"
	counterexample.InputCodec = ""
	if err := counterexample.Validate(); err == nil {
		t.Fatal("counterexample without input codec accepted")
	}
}

func TestReportValidate(t *testing.T) {
	base := domain.Report{
		SchemaVersion: domain.ReportSchemaV1,
		ReportID:      testReportID,
		RunID:         testRunID,
		TenantID:      testTenantID,
		ProjectID:     testProject,
		Verdict:       domain.VerdictVerified,
		Manifest:      domain.NewCASRef(digestHexA, 10),
		MerkleRoot:    digestB,
		Claims: []domain.ClaimAssessment{
			{ClaimID: "compile", Result: domain.ClaimProven, Required: true},
		},
		Attestation: domain.AttestationPending,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.Report)
	}{
		{"wrong schema", func(r *domain.Report) { r.SchemaVersion = "v2" }},
		{"empty run", func(r *domain.Report) { r.RunID = "" }},
		{"empty tenant", func(r *domain.Report) { r.TenantID = "" }},
		{"unknown verdict", func(r *domain.Report) { r.Verdict = "BOGUS" }},
		{"unknown attestation", func(r *domain.Report) { r.Attestation = "BOGUS" }},
		{"invalid manifest ref", func(r *domain.Report) { r.Manifest = domain.CASRef{Algorithm: "md5", Digest: digestHexA} }},
		{"empty merkle root", func(r *domain.Report) { r.MerkleRoot = "" }},
		{"no claims", func(r *domain.Report) { r.Claims = nil }},
		{"invalid claim result", func(r *domain.Report) { r.Claims[0].Result = "BOGUS" }},
		{"signed without signatures", func(r *domain.Report) { r.Attestation = domain.AttestationSigned }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := base
			report.Claims = append([]domain.ClaimAssessment(nil), base.Claims...)
			tc.mutate(&report)
			if err := report.Validate(); err == nil {
				t.Fatalf("invalid report accepted: %+v", report)
			}
		})
	}
}
