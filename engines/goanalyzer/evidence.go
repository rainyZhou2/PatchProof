package goanalyzer

import (
	"fmt"
	"runtime"
	"sort"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

// analysisAssumption documents the conservative resolution model.
const analysisAssumption = "type resolution uses go/packages; unresolved dynamic calls are treated conservatively"

// buildEvidence creates STATIC_ANALYSIS evidence scoped to the changed symbols
// and their bounded reverse call graph.
func buildEvidence(request engine.Request, claimID string, artifacts []domain.CASRef, limits []string) (domain.Evidence, error) {
	tenantID := request.TenantID
	if tenantID == "" {
		tenantID = request.Spec.TenantID
	}
	createdAt := time.Now().UTC()
	if request.Clock != nil {
		createdAt = request.Clock.Now().UTC()
	}
	sandboxProfile := string(request.Spec.Requirements.Isolation)
	if sandboxProfile == "" {
		sandboxProfile = "unspecified"
	}
	dedupedLimits := dedupeStrings(limits)
	if len(dedupedLimits) > 32 {
		dedupedLimits = dedupedLimits[:32]
		dedupedLimits = append(dedupedLimits, "additional limits suppressed")
	}
	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      tenantID,
		RunID:         request.Spec.RunID,
		StepID:        request.Spec.StepID,
		ClaimID:       domain.ClaimID(claimID),
		Kind:          domain.EvidenceStatic,
		Result:        domain.ClaimTested,
		Scope: domain.EvidenceScope{
			InputDomain: "changed symbols + bounded reverse call graph",
			Details: map[string]string{
				"analysis": "go/packages with go/parser fallback",
				"language": language,
			},
		},
		Assumptions: []string{analysisAssumption},
		Engine: domain.EngineInfo{
			Name:    EngineName,
			Version: EngineVersion,
		},
		Environment: domain.EnvironmentInfo{
			Arch:           runtime.GOARCH,
			SandboxProfile: sandboxProfile,
			OS:             runtime.GOOS,
			Extra:          map[string]string{"module_mode": "-mod=mod"},
		},
		Artifacts: append([]domain.CASRef(nil), artifacts...),
		Limits:    dedupedLimits,
		CreatedAt: createdAt,
	}
	sort.Slice(evidence.Artifacts, func(i, j int) bool {
		return evidence.Artifacts[i].Digest < evidence.Artifacts[j].Digest
	})
	if err := evidence.Validate(); err != nil {
		return domain.Evidence{}, fmt.Errorf("build static-analysis evidence: %w", err)
	}
	return evidence, nil
}

// claimOption returns the claim identifier configured for the step.
func claimOption(options map[string]string) string {
	return stepopts.Get(options, stepopts.ClaimID)
}
