package markdown

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
)

type Reporter struct{}

func New() *Reporter { return &Reporter{} }

func (*Reporter) Format() string      { return "markdown" }
func (*Reporter) ContentType() string { return "text/markdown; charset=utf-8" }

func (*Reporter) Render(_ context.Context, report domain.Report, evidence []domain.Evidence) ([]byte, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# PatchProof report\n\n")
	fmt.Fprintf(&builder, "**Verdict:** `%s`  \n", report.Verdict)
	fmt.Fprintf(&builder, "**Run:** `%s`  \n", report.RunID)
	fmt.Fprintf(&builder, "**Base:** `%s`  \n**Head:** `%s`  \n", report.BaseCommit, report.HeadCommit)
	fmt.Fprintf(&builder, "**Policy digest:** `%s`  \n", report.PolicyDigest)
	fmt.Fprintf(&builder, "**Snapshot digest:** `%s`  \n", report.SnapshotDigest)
	fmt.Fprintf(&builder, "**Attestation:** %s  \n", report.Attestation)
	fmt.Fprintf(&builder, "**Generated:** %s\n\n", report.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"))

	fmt.Fprintf(&builder, "## Claims\n\n")
	fmt.Fprintf(&builder, "| Claim | Kind | Required | Result | Minimum | Scope |\n")
	fmt.Fprintf(&builder, "|---|---|---|---|---|---|\n")
	claims := append([]domain.ClaimAssessment(nil), report.Claims...)
	sort.Slice(claims, func(i, j int) bool { return claims[i].ClaimID < claims[j].ClaimID })
	for _, claim := range claims {
		fmt.Fprintf(&builder, "| `%s` | %s | %t | **%s** | %s | %s |\n",
			escape(claim.ClaimID.String()), claim.Kind, claim.Required, claim.Result, claim.MinimumResult, escape(claim.Scope))
	}
	builder.WriteString("\n")

	if len(report.Findings) > 0 {
		fmt.Fprintf(&builder, "## Findings\n\n")
		for _, finding := range report.Findings {
			fmt.Fprintf(&builder, "### %s (%s)\n\n%s\n\n", finding.Code, finding.Severity, escape(finding.Summary))
			if finding.Detail != "" {
				fmt.Fprintf(&builder, "%s\n\n", escape(finding.Detail))
			}
			if finding.ClaimID != "" {
				fmt.Fprintf(&builder, "Claim: `%s`\n\n", finding.ClaimID)
			}
		}
	}

	if len(report.Counterexamples) > 0 {
		fmt.Fprintf(&builder, "## Counterexamples\n\n")
		for _, counterexample := range report.Counterexamples {
			fmt.Fprintf(&builder, "- `%s` claim `%s`, reproduced: %t, minimized: %t, input: `%s`\n",
				escape(counterexample.CaseID), counterexample.ClaimID, counterexample.Reproduced, counterexample.Minimized, counterexample.InputRef.URI())
		}
		builder.WriteString("\n")
	}

	if len(evidence) > 0 {
		fmt.Fprintf(&builder, "## Evidence\n\n")
		for _, item := range evidence {
			fmt.Fprintf(&builder, "- `%s` %s/%s → %s\n", item.ID, item.Engine.Name, item.Engine.Version, item.Result)
		}
		builder.WriteString("\n")
	}

	if len(report.Assumptions) > 0 {
		fmt.Fprintf(&builder, "## Assumptions\n\n")
		for _, assumption := range report.Assumptions {
			fmt.Fprintf(&builder, "- %s\n", escape(assumption))
		}
		builder.WriteString("\n")
	}
	if len(report.Limitations) > 0 {
		fmt.Fprintf(&builder, "## Limitations\n\n")
		for _, limitation := range report.Limitations {
			fmt.Fprintf(&builder, "- %s\n", escape(limitation))
		}
		builder.WriteString("\n")
	}

	fmt.Fprintf(&builder, "## Integrity\n\n")
	fmt.Fprintf(&builder, "- Manifest: `%s`\n", report.Manifest.URI())
	fmt.Fprintf(&builder, "- Merkle root: `%s`\n", report.MerkleRoot)
	for _, signature := range report.Signatures {
		fmt.Fprintf(&builder, "- Signature `%s` by key `%s` (%s) at %s\n", signature.ID, signature.KeyID, signature.Algorithm, signature.SignedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	builder.WriteString("\n")
	fmt.Fprintf(&builder, "_Results are bounded by the declared claim domains and recorded engine versions; TESTED_WITHIN_SCOPE is not a formal proof of program correctness._\n")
	return []byte(builder.String()), nil
}

func escape(value string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(value)
}
