package domain

import (
	"fmt"
	"sort"
)

type VerdictInput struct {
	Claims               []Claim
	Evidence             []Evidence
	Counterexamples      []Counterexample
	PolicyBlocked        bool
	PolicyReason         string
	InfrastructureFailed bool
	Cancelled            bool
}

type VerdictResult struct {
	Verdict     Verdict           `json:"verdict"`
	Claims      []ClaimAssessment `json:"claims"`
	Findings    []Finding         `json:"findings"`
	Limitations []string          `json:"limitations,omitempty"`
}

func ComputeVerdict(in VerdictInput) VerdictResult {
	byClaim := make(map[ClaimID][]Evidence, len(in.Claims))
	for _, ev := range in.Evidence {
		byClaim[ev.ClaimID] = append(byClaim[ev.ClaimID], ev)
	}
	counterByClaim := make(map[ClaimID]int, len(in.Counterexamples))
	for _, ce := range in.Counterexamples {
		if ce.Reproduced {
			counterByClaim[ce.ClaimID]++
		}
	}

	out := VerdictResult{
		Claims:   make([]ClaimAssessment, 0, len(in.Claims)),
		Findings: make([]Finding, 0, 4),
	}
	for _, claim := range in.Claims {
		assessment := assessClaim(claim, byClaim[claim.ID])
		out.Claims = append(out.Claims, assessment)
	}
	sort.Slice(out.Claims, func(i, j int) bool { return out.Claims[i].ClaimID < out.Claims[j].ClaimID })

	if in.PolicyBlocked {
		out.Verdict = VerdictPolicyBlocked
		out.Findings = append(out.Findings, Finding{
			Code:     "POLICY_BLOCKED",
			Severity: SeverityHigh,
			Summary:  "trusted verification policy rejected this patch",
			Detail:   in.PolicyReason,
		})
		return out
	}
	if in.Cancelled {
		out.Verdict = VerdictCancelled
		out.Findings = append(out.Findings, Finding{Code: "CANCELLED", Severity: SeverityInfo, Summary: "verification was cancelled before completion"})
		return out
	}

	required := 0
	invalid := make([]ClaimAssessment, 0, 1)
	violated := make([]ClaimAssessment, 0, 1)
	unsatisfied := make([]ClaimAssessment, 0, 2)
	for _, assessment := range out.Claims {
		if !assessment.Required {
			if assessment.Result == ClaimViolated {
				out.Findings = append(out.Findings, Finding{
					Code: "OPTIONAL_CLAIM_VIOLATED", Severity: SeverityMedium, ClaimID: assessment.ClaimID,
					Summary: "optional claim was violated", EvidenceIDs: assessment.EvidenceIDs,
				})
			}
			continue
		}
		required++
		switch {
		case assessment.Result == ClaimInvalidEvidence:
			invalid = append(invalid, assessment)
		case assessment.Result == ClaimViolated:
			violated = append(violated, assessment)
		case !assessment.Result.Satisfies(assessment.MinimumResult):
			unsatisfied = append(unsatisfied, assessment)
		}
	}

	for _, assessment := range violated {
		out.Findings = append(out.Findings, Finding{
			Code:        violationCode(assessment.Kind),
			Severity:    SeverityHigh,
			ClaimID:     assessment.ClaimID,
			Summary:     fmt.Sprintf("claim %s was violated", assessment.ClaimID),
			Detail:      assessment.Reason,
			EvidenceIDs: assessment.EvidenceIDs,
		})
	}
	for _, assessment := range invalid {
		out.Findings = append(out.Findings, Finding{
			Code:        "INVALID_EVIDENCE",
			Severity:    SeverityHigh,
			ClaimID:     assessment.ClaimID,
			Summary:     "evidence failed integrity or schema validation and cannot support a verdict",
			Detail:      assessment.Reason,
			EvidenceIDs: assessment.EvidenceIDs,
		})
	}
	for _, assessment := range unsatisfied {
		out.Findings = append(out.Findings, Finding{
			Code:        "INSUFFICIENT_EVIDENCE",
			Severity:    SeverityMedium,
			ClaimID:     assessment.ClaimID,
			Summary:     fmt.Sprintf("claim %s requires %s evidence but reached %s", assessment.ClaimID, assessment.MinimumResult, assessment.Result),
			Detail:      assessment.Reason,
			EvidenceIDs: assessment.EvidenceIDs,
		})
	}

	if required == 0 {
		out.Verdict = VerdictInconclusive
		out.Findings = append(out.Findings, Finding{Code: "NO_REQUIRED_CLAIMS", Severity: SeverityHigh, Summary: "policy did not define any required claim"})
		return out
	}
	if len(invalid) > 0 {
		out.Verdict = VerdictInconclusive
		return out
	}
	if len(violated) > 0 {
		out.Verdict = VerdictCounterexample
		return out
	}
	if len(unsatisfied) == 0 {
		out.Verdict = VerdictVerified
		return out
	}
	if in.InfrastructureFailed && !anyPositiveEvidence(out.Claims) {
		out.Verdict = VerdictInfraFailed
		out.Findings = append(out.Findings, Finding{Code: "INFRA_FAILURE", Severity: SeverityHigh, Summary: "verification could not execute required claims"})
		return out
	}
	out.Verdict = VerdictInconclusive
	return out
}

func assessClaim(claim Claim, evidence []Evidence) ClaimAssessment {
	assessment := ClaimAssessment{
		ClaimID:       claim.ID,
		Kind:          claim.Kind,
		Title:         claim.Title,
		Required:      claim.Required,
		MinimumResult: minimumFor(claim.MinimumResult),
		Result:        ClaimNotRun,
	}
	if claim.MinimumResult != "" {
		assessment.Reason = "minimum evidence level: " + string(claim.MinimumResult)
	}
	if len(evidence) == 0 {
		return assessment
	}
	sorted := append([]Evidence(nil), evidence...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	best := ClaimNotRun
	bestReason := ""
	for _, ev := range sorted {
		assessment.EvidenceIDs = append(assessment.EvidenceIDs, ev.ID)
		if assessment.Scope == "" {
			assessment.Scope = ev.Scope.InputDomain
		}
		switch ev.Result {
		case ClaimInvalidEvidence:
			assessment.Result = ClaimInvalidEvidence
			assessment.Reason = "invalid evidence"
			return assessment
		case ClaimViolated:
			if best != ClaimInvalidEvidence {
				best = ClaimViolated
				bestReason = ev.Scope.InputDomain
			}
		case ClaimProven:
			if best != ClaimViolated {
				best = ClaimProven
			}
		case ClaimTested:
			if best == ClaimNotRun || best == ClaimUnknown {
				best = ClaimTested
			}
		case ClaimUnknown:
			if best == ClaimNotRun {
				best = ClaimUnknown
			}
		case ClaimNotRun:
		}
	}
	assessment.Result = best
	if bestReason != "" {
		assessment.Reason = "counterexample in scope: " + bestReason
	}
	return assessment
}

func minimumFor(minimum ClaimResult) ClaimResult {
	if minimum == ClaimProven {
		return ClaimProven
	}
	return ClaimTested
}

func anyPositiveEvidence(claims []ClaimAssessment) bool {
	for _, c := range claims {
		if c.Result == ClaimProven || c.Result == ClaimTested {
			return true
		}
	}
	return false
}

func violationCode(kind ClaimKind) string {
	switch kind {
	case ClaimKindCompile:
		return "COMPILATION_FAILED"
	case ClaimKindTest:
		return "TEST_ASSERTION_FAILED"
	case ClaimKindSecurity:
		return "SECURITY_REGRESSION"
	default:
		return "COUNTEREXAMPLE_FOUND"
	}
}
