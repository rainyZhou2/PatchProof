package gobuild

import (
	"fmt"
	"runtime"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// buildEvidence constructs validated evidence for a compile or regression step.
func buildEvidence(
	request engine.Request,
	claimID string,
	kind domain.EvidenceKind,
	result domain.ClaimResult,
	scope string,
	assumptions []string,
	artifacts []domain.CASRef,
	limits []string,
	details map[string]string,
	extra map[string]string,
) (domain.Evidence, error) {
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
	normalizedLimits := normalizeLimits(limits)
	if len(normalizedLimits) > 32 {
		normalizedLimits = normalizedLimits[:32]
		normalizedLimits = append(normalizedLimits, "additional limits suppressed")
	}
	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      tenantID,
		RunID:         request.Spec.RunID,
		StepID:        request.Spec.StepID,
		ClaimID:       domain.ClaimID(claimID),
		Kind:          kind,
		Result:        result,
		Scope: domain.EvidenceScope{
			InputDomain: scope,
			Details:     details,
		},
		Assumptions: append([]string(nil), assumptions...),
		Engine: domain.EngineInfo{
			Name:    EngineName,
			Version: EngineVersion,
		},
		Environment: domain.EnvironmentInfo{
			Arch:           runtime.GOARCH,
			SandboxProfile: sandboxProfile,
			OS:             runtime.GOOS,
			Extra:          extra,
		},
		Artifacts: append([]domain.CASRef(nil), artifacts...),
		Limits:    normalizedLimits,
		CreatedAt: createdAt,
	}
	if err := evidence.Validate(); err != nil {
		return domain.Evidence{}, fmt.Errorf("build evidence: %w", err)
	}
	return evidence, nil
}
