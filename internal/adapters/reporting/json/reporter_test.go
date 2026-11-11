package json

import (
	"bytes"
	stdjson "encoding/json"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func testReport(t *testing.T) domain.Report {
	t.Helper()
	runID := domain.NewRunID()
	tenantID := domain.NewTenantID()
	projectID := domain.NewProjectID()
	claimID := domain.ClaimID("claim:behavior")
	secondClaimID := domain.ClaimID("claim:security")
	generatedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	signatureID := domain.NewSignatureID()
	digest := func(seed byte) string {
		out := make([]byte, 64)
		for i := range out {
			out[i] = "0123456789abcdef"[seed%16]
		}
		return string(out)
	}
	return domain.Report{
		SchemaVersion:  domain.ReportSchemaV1,
		ReportID:       domain.NewReportID(),
		RunID:          runID,
		TenantID:       tenantID,
		ProjectID:      projectID,
		Verdict:        domain.VerdictCounterexample,
		BaseCommit:     "aaaaaaa",
		HeadCommit:     "bbbbbbb",
		SnapshotDigest: "sha256:" + digest(1),
		PolicyDigest:   "sha256:" + digest(2),
		Claims: []domain.ClaimAssessment{
			{
				ClaimID: claimID, Kind: domain.ClaimKindRegression, Title: "behavior", Required: true,
				MinimumResult: domain.ClaimTested, Result: domain.ClaimViolated, Scope: "inputs | edge", Reason: "counterexample in scope",
			},
			{
				ClaimID: secondClaimID, Kind: domain.ClaimKindSecurity, Title: "security", Required: true,
				MinimumResult: domain.ClaimTested, Result: domain.ClaimTested,
			},
		},
		Findings: []domain.Finding{
			{Code: "COUNTEREXAMPLE_FOUND", Severity: domain.SeverityHigh, ClaimID: claimID, Summary: "claim violated | in scope", Detail: "diff | mismatch"},
			{Code: "INSUFFICIENT_EVIDENCE", Severity: domain.SeverityMedium, ClaimID: secondClaimID, Summary: "insufficient evidence"},
		},
		Counterexamples: []domain.Counterexample{{
			CaseID: "case-1", TenantID: tenantID, RunID: runID, ClaimID: claimID, InputCodec: "json",
			InputRef: domain.NewCASRef(digest(3), 10), BaseObservationRef: domain.NewCASRef(digest(4), 11),
			HeadObservationRef: domain.NewCASRef(digest(5), 12), Reproduced: true, Scope: "inputs", Minimized: true,
			CreatedAt: generatedAt,
		}},
		Assumptions: []string{"assumes canonical JSON"},
		Limitations: []string{"scope | bounded"},
		Metrics:     domain.RunMetrics{CPUSeconds: 1.5, WallSeconds: 10, StepCount: 3},
		Provenance: domain.Provenance{
			Region: "region-a", OrchestratorVersion: "v1.2.3", SourceRepository: "https://example.com/repo.git",
			EngineVersions: map[string]string{"go-test": "1.0.0"},
		},
		Manifest:   domain.NewCASRef(digest(6), 2048),
		MerkleRoot: "sha256:" + digest(7),
		Signatures: []domain.Signature{{
			ID: signatureID, KeyID: "key-1", Algorithm: "ed25519", Subject: "report", Signature: []byte{1, 2, 3, 4},
			SignedAt: generatedAt,
		}},
		Attestation: domain.AttestationSigned,
		GeneratedAt: generatedAt,
	}
}

func TestReporterMetadata(t *testing.T) {
	t.Parallel()
	reporter := New()
	if got := reporter.Format(); got != "json" {
		t.Fatalf("Format = %q, want json", got)
	}
	if got := reporter.ContentType(); got != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", got)
	}
}

func TestRenderRoundTripsReport(t *testing.T) {
	t.Parallel()
	report := testReport(t)
	if err := report.Validate(); err != nil {
		t.Fatalf("fixture report is not valid: %v", err)
	}
	rendered, err := New().Render(t.Context(), report, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !stdjson.Valid(rendered) {
		t.Fatalf("rendered report is not valid JSON:\n%s", rendered)
	}

	var decoded domain.Report
	if err := stdjson.Unmarshal(rendered, &decoded); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded report is not valid: %v", err)
	}
	if decoded.RunID != report.RunID || decoded.ReportID != report.ReportID || decoded.TenantID != report.TenantID {
		t.Fatalf("identity fields changed: %+v", decoded)
	}
	if decoded.Verdict != report.Verdict || decoded.Attestation != report.Attestation {
		t.Fatalf("verdict/attestation changed: %q/%q", decoded.Verdict, decoded.Attestation)
	}
	if !decoded.GeneratedAt.Equal(report.GeneratedAt) {
		t.Fatalf("generated_at = %v, want %v", decoded.GeneratedAt, report.GeneratedAt)
	}
	if len(decoded.Claims) != 2 || decoded.Claims[0].ClaimID != report.Claims[0].ClaimID || decoded.Claims[0].Result != domain.ClaimViolated {
		t.Fatalf("claims changed: %+v", decoded.Claims)
	}
	if len(decoded.Findings) != 2 || decoded.Findings[0].Code != "COUNTEREXAMPLE_FOUND" {
		t.Fatalf("findings changed: %+v", decoded.Findings)
	}
	if len(decoded.Counterexamples) != 1 {
		t.Fatalf("counterexamples changed: %+v", decoded.Counterexamples)
	}
	ce := decoded.Counterexamples[0]
	if ce.InputRef != report.Counterexamples[0].InputRef || !ce.Reproduced || !ce.Minimized {
		t.Fatalf("counterexample changed: %+v", ce)
	}
	if len(decoded.Signatures) != 1 || !bytes.Equal(decoded.Signatures[0].Signature, []byte{1, 2, 3, 4}) {
		t.Fatalf("signatures changed: %+v", decoded.Signatures)
	}
	if decoded.Manifest != report.Manifest || decoded.MerkleRoot != report.MerkleRoot {
		t.Fatalf("integrity fields changed: %+v / %q", decoded.Manifest, decoded.MerkleRoot)
	}
	if len(decoded.Limitations) != 1 || decoded.Limitations[0] != "scope | bounded" {
		t.Fatalf("limitations changed: %#v", decoded.Limitations)
	}
}

func TestRenderEmptyOptionalSections(t *testing.T) {
	t.Parallel()
	report := testReport(t)
	report.Findings = nil
	report.Counterexamples = nil
	report.Signatures = nil
	report.Limitations = nil
	report.Assumptions = nil
	rendered, err := New().Render(t.Context(), report, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var decoded domain.Report
	if err := stdjson.Unmarshal(rendered, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Findings != nil || decoded.Counterexamples != nil || decoded.Signatures != nil {
		t.Fatalf("omitted sections decoded as non-nil: %+v", decoded)
	}
	if decoded.RunID != report.RunID {
		t.Fatalf("run ID changed: %q", decoded.RunID)
	}
}
