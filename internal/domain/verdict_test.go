package domain_test

import (
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func verdictClaim(id string, kind domain.ClaimKind, minimum domain.ClaimResult, required bool) domain.Claim {
	return domain.Claim{
		ID:            domain.ClaimID(id),
		Kind:          kind,
		Title:         "claim " + id,
		Required:      required,
		MinimumResult: minimum,
	}
}

func verdictEvidence(claimID, result string, at time.Time) domain.Evidence {
	return domain.Evidence{
		ID:        domain.EvidenceID("evidence-" + claimID + "-" + result),
		ClaimID:   domain.ClaimID(claimID),
		Result:    domain.ClaimResult(result),
		Scope:     domain.EvidenceScope{InputDomain: "scope-" + claimID},
		CreatedAt: at,
	}
}

func hasFindingCode(result domain.VerdictResult, code string) bool {
	for _, finding := range result.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func TestComputeVerdictPolicyBlockedDominates(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims:               []domain.Claim{verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true)},
		Evidence:             []domain.Evidence{verdictEvidence("compile", string(domain.ClaimViolated), oldTime)},
		PolicyBlocked:        true,
		PolicyReason:         "protected path modified",
		Cancelled:            true,
		InfrastructureFailed: true,
	})
	if result.Verdict != domain.VerdictPolicyBlocked {
		t.Fatalf("verdict = %s, want POLICY_BLOCKED", result.Verdict)
	}
	if !hasFindingCode(result, "POLICY_BLOCKED") {
		t.Fatalf("missing POLICY_BLOCKED finding: %+v", result.Findings)
	}
}

func TestComputeVerdictCancelled(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims:    []domain.Claim{verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true)},
		Cancelled: true,
	})
	if result.Verdict != domain.VerdictCancelled {
		t.Fatalf("verdict = %s, want CANCELLED", result.Verdict)
	}
	if !hasFindingCode(result, "CANCELLED") {
		t.Fatalf("missing CANCELLED finding: %+v", result.Findings)
	}
}

func TestComputeVerdictInvalidEvidence(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{verdictClaim("regression", domain.ClaimKindTest, domain.ClaimTested, true)},
		Evidence: []domain.Evidence{
			verdictEvidence("regression", string(domain.ClaimInvalidEvidence), oldTime),
		},
	})
	if result.Verdict != domain.VerdictInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE", result.Verdict)
	}
	if !hasFindingCode(result, "INVALID_EVIDENCE") {
		t.Fatalf("missing INVALID_EVIDENCE finding: %+v", result.Findings)
	}
}

func TestComputeVerdictRequiredViolationUsesKindCode(t *testing.T) {
	tests := []struct {
		kind domain.ClaimKind
		code string
	}{
		{domain.ClaimKindCompile, "COMPILATION_FAILED"},
		{domain.ClaimKindTest, "TEST_ASSERTION_FAILED"},
		{domain.ClaimKindSecurity, "SECURITY_REGRESSION"},
		{domain.ClaimKindRegression, "COUNTEREXAMPLE_FOUND"},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			result := domain.ComputeVerdict(domain.VerdictInput{
				Claims:   []domain.Claim{verdictClaim("claim-x", tc.kind, domain.ClaimTested, true)},
				Evidence: []domain.Evidence{verdictEvidence("claim-x", string(domain.ClaimViolated), oldTime)},
			})
			if result.Verdict != domain.VerdictCounterexample {
				t.Fatalf("verdict = %s, want COUNTEREXAMPLE_FOUND", result.Verdict)
			}
			if !hasFindingCode(result, tc.code) {
				t.Fatalf("missing finding %s: %+v", tc.code, result.Findings)
			}
		})
	}
}

func TestComputeVerdictOptionalViolationOnlyFinding(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{
			verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true),
			verdictClaim("optional-fuzz", domain.ClaimKindRegression, domain.ClaimTested, false),
		},
		Evidence: []domain.Evidence{
			verdictEvidence("compile", string(domain.ClaimTested), oldTime),
			verdictEvidence("optional-fuzz", string(domain.ClaimViolated), oldTime),
		},
	})
	if result.Verdict != domain.VerdictVerified {
		t.Fatalf("verdict = %s, want VERIFIED_WITHIN_SCOPE", result.Verdict)
	}
	if !hasFindingCode(result, "OPTIONAL_CLAIM_VIOLATED") {
		t.Fatalf("missing OPTIONAL_CLAIM_VIOLATED finding: %+v", result.Findings)
	}
	if hasFindingCode(result, "COUNTEREXAMPLE_FOUND") {
		t.Fatalf("optional violation must not produce a counterexample finding: %+v", result.Findings)
	}
}

func TestComputeVerdictMinimumEvidenceLevels(t *testing.T) {
	tests := []struct {
		name    string
		minimum domain.ClaimResult
		result  domain.ClaimResult
		verdict domain.Verdict
	}{
		{"tested evidence insufficient for proven", domain.ClaimProven, domain.ClaimTested, domain.VerdictInconclusive},
		{"proven evidence satisfies tested", domain.ClaimTested, domain.ClaimProven, domain.VerdictVerified},
		{"proven evidence satisfies proven", domain.ClaimProven, domain.ClaimProven, domain.VerdictVerified},
		{"no evidence is insufficient", domain.ClaimTested, "", domain.VerdictInconclusive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := domain.VerdictInput{
				Claims: []domain.Claim{verdictClaim("claim-x", domain.ClaimKindTest, tc.minimum, true)},
			}
			if tc.result != "" {
				input.Evidence = []domain.Evidence{verdictEvidence("claim-x", string(tc.result), oldTime)}
			}
			result := domain.ComputeVerdict(input)
			if result.Verdict != tc.verdict {
				t.Fatalf("verdict = %s, want %s", result.Verdict, tc.verdict)
			}
			if tc.verdict == domain.VerdictInconclusive && !hasFindingCode(result, "INSUFFICIENT_EVIDENCE") {
				t.Fatalf("missing INSUFFICIENT_EVIDENCE finding: %+v", result.Findings)
			}
		})
	}
}

func TestComputeVerdictAllSatisfied(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{
			verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true),
			verdictClaim("regression", domain.ClaimKindTest, domain.ClaimTested, true),
		},
		Evidence: []domain.Evidence{
			verdictEvidence("compile", string(domain.ClaimProven), oldTime),
			verdictEvidence("regression", string(domain.ClaimTested), oldTime.Add(time.Second)),
		},
	})
	if result.Verdict != domain.VerdictVerified {
		t.Fatalf("verdict = %s, want VERIFIED_WITHIN_SCOPE", result.Verdict)
	}
	if len(result.Claims) != 2 {
		t.Fatalf("claims = %d, want 2", len(result.Claims))
	}
	for _, assessment := range result.Claims {
		if !assessment.Result.Satisfies(assessment.MinimumResult) {
			t.Errorf("claim %s not satisfied: %s", assessment.ClaimID, assessment.Result)
		}
	}
}

func TestComputeVerdictInfrastructureFailure(t *testing.T) {
	t.Run("no positive evidence", func(t *testing.T) {
		result := domain.ComputeVerdict(domain.VerdictInput{
			Claims:               []domain.Claim{verdictClaim("regression", domain.ClaimKindTest, domain.ClaimTested, true)},
			InfrastructureFailed: true,
		})
		if result.Verdict != domain.VerdictInfraFailed {
			t.Fatalf("verdict = %s, want INFRA_FAILED", result.Verdict)
		}
		if !hasFindingCode(result, "INFRA_FAILURE") {
			t.Fatalf("missing INFRA_FAILURE finding: %+v", result.Findings)
		}
	})

	t.Run("positive evidence makes it inconclusive", func(t *testing.T) {
		result := domain.ComputeVerdict(domain.VerdictInput{
			Claims: []domain.Claim{
				verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true),
				verdictClaim("regression", domain.ClaimKindTest, domain.ClaimTested, true),
			},
			Evidence:             []domain.Evidence{verdictEvidence("compile", string(domain.ClaimProven), oldTime)},
			InfrastructureFailed: true,
		})
		if result.Verdict != domain.VerdictInconclusive {
			t.Fatalf("verdict = %s, want INCONCLUSIVE", result.Verdict)
		}
	})
}

func TestComputeVerdictNoRequiredClaims(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{
			verdictClaim("optional", domain.ClaimKindTest, domain.ClaimTested, false),
		},
		Evidence: []domain.Evidence{verdictEvidence("optional", string(domain.ClaimProven), oldTime)},
	})
	if result.Verdict != domain.VerdictInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE", result.Verdict)
	}
	if !hasFindingCode(result, "NO_REQUIRED_CLAIMS") {
		t.Fatalf("missing NO_REQUIRED_CLAIMS finding: %+v", result.Findings)
	}
}

func TestComputeVerdictInvalidEvidenceBeatsViolation(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true)},
		Evidence: []domain.Evidence{
			verdictEvidence("compile", string(domain.ClaimViolated), oldTime),
			verdictEvidence("compile", string(domain.ClaimInvalidEvidence), oldTime.Add(time.Second)),
		},
	})
	if result.Verdict != domain.VerdictInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE when invalid evidence is present", result.Verdict)
	}
}

func TestComputeVerdictEvidencePrecedence(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{verdictClaim("claim-x", domain.ClaimKindTest, domain.ClaimTested, true)},
		Evidence: []domain.Evidence{
			verdictEvidence("claim-x", string(domain.ClaimUnknown), oldTime),
			verdictEvidence("claim-x", string(domain.ClaimTested), oldTime.Add(time.Second)),
			verdictEvidence("claim-x", string(domain.ClaimProven), oldTime.Add(2*time.Second)),
		},
	})
	if result.Verdict != domain.VerdictVerified {
		t.Fatalf("verdict = %s, want VERIFIED_WITHIN_SCOPE", result.Verdict)
	}
	if len(result.Claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(result.Claims))
	}
	if result.Claims[0].Result != domain.ClaimProven {
		t.Fatalf("best evidence = %s, want PROVEN_WITHIN_MODEL", result.Claims[0].Result)
	}
	if len(result.Claims[0].EvidenceIDs) != 3 {
		t.Fatalf("evidence ids = %v, want all three", result.Claims[0].EvidenceIDs)
	}
}

func TestComputeVerdictClaimsSortedDeterministically(t *testing.T) {
	claims := []domain.Claim{
		verdictClaim("zulu", domain.ClaimKindTest, domain.ClaimTested, true),
		verdictClaim("alpha", domain.ClaimKindCompile, domain.ClaimTested, true),
		verdictClaim("mike", domain.ClaimKindSecurity, domain.ClaimTested, true),
	}
	evidence := []domain.Evidence{
		verdictEvidence("zulu", string(domain.ClaimProven), oldTime),
		verdictEvidence("alpha", string(domain.ClaimProven), oldTime),
		verdictEvidence("mike", string(domain.ClaimProven), oldTime),
	}
	result := domain.ComputeVerdict(domain.VerdictInput{Claims: claims, Evidence: evidence})
	if result.Verdict != domain.VerdictVerified {
		t.Fatalf("verdict = %s, want VERIFIED_WITHIN_SCOPE", result.Verdict)
	}
	for i := 1; i < len(result.Claims); i++ {
		if result.Claims[i-1].ClaimID >= result.Claims[i].ClaimID {
			t.Fatalf("claims not sorted: %s >= %s", result.Claims[i-1].ClaimID, result.Claims[i].ClaimID)
		}
	}
}

func TestComputeVerdictFindingsReferenceEvidence(t *testing.T) {
	evidence := verdictEvidence("compile", string(domain.ClaimViolated), oldTime)
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims:   []domain.Claim{verdictClaim("compile", domain.ClaimKindCompile, domain.ClaimTested, true)},
		Evidence: []domain.Evidence{evidence},
	})
	if len(result.Claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(result.Claims))
	}
	if len(result.Claims[0].EvidenceIDs) != 1 || result.Claims[0].EvidenceIDs[0] != evidence.ID {
		t.Fatalf("assessment evidence ids = %v, want [%s]", result.Claims[0].EvidenceIDs, evidence.ID)
	}
	for _, finding := range result.Findings {
		if finding.Code != "COMPILATION_FAILED" {
			continue
		}
		if len(finding.EvidenceIDs) != 1 || finding.EvidenceIDs[0] != evidence.ID {
			t.Fatalf("finding evidence ids = %v, want [%s]", finding.EvidenceIDs, evidence.ID)
		}
		return
	}
	t.Fatalf("missing COMPILATION_FAILED finding: %+v", result.Findings)
}

func TestComputeVerdictViolatedEvidenceDominatesProven(t *testing.T) {
	result := domain.ComputeVerdict(domain.VerdictInput{
		Claims: []domain.Claim{verdictClaim("differential", domain.ClaimKindRegression, domain.ClaimTested, true)},
		Evidence: []domain.Evidence{
			verdictEvidence("differential", string(domain.ClaimProven), oldTime),
			verdictEvidence("differential", string(domain.ClaimViolated), oldTime.Add(time.Second)),
		},
		Counterexamples: []domain.Counterexample{
			{ClaimID: "differential", CaseID: "ce-1", Reproduced: true},
		},
	})
	if result.Verdict != domain.VerdictCounterexample {
		t.Fatalf("verdict = %s, want COUNTEREXAMPLE_FOUND", result.Verdict)
	}
	if hasFindingCode(result, "INVALID_EVIDENCE") {
		t.Fatalf("unexpected invalid evidence finding: %+v", result.Findings)
	}
}
