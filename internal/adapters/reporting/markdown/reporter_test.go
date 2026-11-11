package markdown

import (
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func testReport(t *testing.T) domain.Report {
	t.Helper()
	runID := domain.NewRunID()
	tenantID := domain.NewTenantID()
	claimID := domain.ClaimID("claim:behavior")
	secondClaimID := domain.ClaimID("claim:security")
	digest := strings.Repeat("a", 64)
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return domain.Report{
		SchemaVersion:  domain.ReportSchemaV1,
		ReportID:       domain.NewReportID(),
		RunID:          runID,
		TenantID:       tenantID,
		ProjectID:      domain.NewProjectID(),
		Verdict:        domain.VerdictCounterexample,
		BaseCommit:     "aaaaaaa",
		HeadCommit:     "bbbbbbb",
		SnapshotDigest: "sha256:" + digest,
		PolicyDigest:   "sha256:" + digest,
		Claims: []domain.ClaimAssessment{
			{
				ClaimID: claimID, Kind: domain.ClaimKindRegression, Title: "behavior", Required: true,
				MinimumResult: domain.ClaimTested, Result: domain.ClaimViolated, Scope: "inputs | edge",
			},
			{
				ClaimID: secondClaimID, Kind: domain.ClaimKindSecurity, Title: "security", Required: true,
				MinimumResult: domain.ClaimTested, Result: domain.ClaimTested, Scope: "security",
			},
		},
		Findings: []domain.Finding{
			{
				Code: "COUNTEREXAMPLE_FOUND", Severity: domain.SeverityHigh, ClaimID: claimID,
				Summary: "claim violated | in scope", Detail: "diff | mismatch\nwith newline",
			},
		},
		Counterexamples: []domain.Counterexample{{
			CaseID: "case-1", TenantID: tenantID, RunID: runID, ClaimID: claimID, InputCodec: "json",
			InputRef: domain.NewCASRef(digest, 10), BaseObservationRef: domain.NewCASRef(digest, 11),
			HeadObservationRef: domain.NewCASRef(digest, 12), Reproduced: true, Minimized: true, Scope: "inputs",
		}},
		Assumptions: []string{"assumes canonical JSON"},
		Limitations: []string{"scope | bounded", "no network"},
		Manifest:    domain.NewCASRef(digest, 2048),
		MerkleRoot:  "sha256:" + digest,
		Signatures: []domain.Signature{{
			ID: domain.NewSignatureID(), KeyID: "key-1", Algorithm: "ed25519", Subject: "report",
			Signature: []byte{1, 2}, SignedAt: fixed,
		}},
		Attestation: domain.AttestationSigned,
		GeneratedAt: fixed,
	}
}

func TestReporterMetadata(t *testing.T) {
	t.Parallel()
	reporter := New()
	if got := reporter.Format(); got != "markdown" {
		t.Fatalf("Format = %q, want markdown", got)
	}
	if got := reporter.ContentType(); got != "text/markdown; charset=utf-8" {
		t.Fatalf("ContentType = %q", got)
	}
}

func TestRenderContainsVerdictClaimsAndLimitations(t *testing.T) {
	t.Parallel()
	report := testReport(t)
	evidence := []domain.Evidence{{
		ID: domain.NewEvidenceID(), Result: domain.ClaimTested,
		Engine: domain.EngineInfo{Name: "go-test", Version: "1.0.0"},
	}}
	rendered, err := New().Render(t.Context(), report, evidence)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	output := string(rendered)

	for _, want := range []string{
		"# PatchProof report",
		"**Verdict:** `COUNTEREXAMPLE_FOUND`",
		"**Run:** `" + report.RunID.String() + "`",
		"**Base:** `aaaaaaa`",
		"**Head:** `bbbbbbb`",
		"**Attestation:** SIGNED",
		"**Generated:** 2026-03-04T05:06:07Z",
		"## Claims",
		"| Claim | Kind | Required | Result | Minimum | Scope |",
		"| `claim:behavior` | REGRESSION | true | **VIOLATED** | TESTED_WITHIN_SCOPE | inputs \\| edge |",
		"| `claim:security` | SECURITY | true | **TESTED_WITHIN_SCOPE** | TESTED_WITHIN_SCOPE | security |",
		"## Findings",
		"### COUNTEREXAMPLE_FOUND (HIGH)",
		"claim violated \\| in scope",
		"diff \\| mismatch with newline",
		"Claim: `claim:behavior`",
		"## Counterexamples",
		"- `case-1` claim `claim:behavior`, reproduced: true, minimized: true, input: `" + report.Counterexamples[0].InputRef.URI() + "`",
		"## Evidence",
		"- `" + evidence[0].ID.String() + "` go-test/1.0.0 → TESTED_WITHIN_SCOPE",
		"## Assumptions",
		"- assumes canonical JSON",
		"## Limitations",
		"- scope \\| bounded",
		"- no network",
		"## Integrity",
		"- Manifest: `" + report.Manifest.URI() + "`",
		"- Merkle root: `" + report.MerkleRoot + "`",
		"by key `key-1` (ed25519)",
		"_Results are bounded by the declared claim domains",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("markdown output does not contain %q:\n%s", want, output)
		}
	}

	// Claims are rendered in sorted claim-ID order regardless of input order.
	if first, second := strings.Index(output, "`claim:behavior`"), strings.Index(output, "`claim:security`"); first < 0 || second < 0 || first > second {
		t.Fatalf("claims are not rendered in sorted order: %d, %d", first, second)
	}

	// The table must stay well formed: each pipe belonging to the claim scope is escaped.
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "claim:behavior` | REGRESSION") {
			structural := strings.Count(line, "|") - strings.Count(line, `\|`)
			if structural != 7 {
				t.Fatalf("claim table row has %d structural pipes, want 7: %q", structural, line)
			}
		}
	}
}

func TestRenderOmitsOptionalSections(t *testing.T) {
	t.Parallel()
	report := testReport(t)
	report.Findings = nil
	report.Counterexamples = nil
	report.Assumptions = nil
	report.Limitations = nil
	report.Signatures = nil
	rendered, err := New().Render(t.Context(), report, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	output := string(rendered)
	for _, unwanted := range []string{"## Findings", "## Counterexamples", "## Evidence", "## Assumptions", "## Limitations"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("output contains %q with empty section:\n%s", unwanted, output)
		}
	}
	if !strings.Contains(output, "## Claims") || !strings.Contains(output, "## Integrity") {
		t.Fatalf("required sections missing:\n%s", output)
	}
}
