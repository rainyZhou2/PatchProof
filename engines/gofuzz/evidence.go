package gofuzz

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	evidenceutil "github.com/patchproof/patchproof/pkg/evidence"
)

func (c *config) putBytes(ctx context.Context, data []byte) (domain.CASRef, error) {
	return c.store.Put(ctx, c.scope, bytes.NewReader(data))
}

func (c *config) buildEvidence(result domain.ClaimResult, elapsedMS int64, details map[string]string, artifacts []domain.CASRef, extraLimits []string) domain.Evidence {
	limits := append([]string(nil), extraLimits...)
	limits = append(limits,
		"go-native-fuzz detects head-side panics and crashes only; it does not prove differential equivalence",
		"no fix-invariant expression is evaluated; a panic is treated as a crash finding",
		"concurrency is one fuzz worker and mutational coverage is determined by the Go fuzzing engine",
	)
	if c.contractFallback {
		limits = append(limits, "contract was re-decoded from YAML through JSON so that native YAML seed arguments could be represented; contract.Document validation still applied")
	}
	if c.fuzzCacheFallback {
		limits = append(limits, "the Go toolchain does not implement -fuzzcache; the equivalent -test.fuzzcachedir flag and the workspace-local GOCACHE hold the fuzz cache")
	}
	limits = dedupeStrings(limits)

	assumptions := append([]string(nil), c.contract.Spec.Assumptions...)
	assumptions = append(assumptions,
		"the target function is deterministic for identical inputs",
		"native fuzzing exercises the head snapshot only",
	)
	assumptions = dedupeStrings(assumptions)

	if details == nil {
		details = map[string]string{}
	}
	details["fuzz_seconds"] = fmt.Sprintf("%d", c.seconds)
	details["strategy"] = "native-go-fuzz"
	details["contract_kind"] = c.contract.Spec.Kind

	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      c.tenant,
		RunID:         c.runID,
		ClaimID:       c.claimID,
		Kind:          domain.EvidenceFuzz,
		Result:        result,
		Scope: domain.EvidenceScope{
			InputDomain: c.contract.Spec.Domain.Description,
			SeedCount:   c.seedCount,
			ElapsedMS:   elapsedMS,
			Budget:      fmt.Sprintf("fuzztime=%ds;runs=1", c.seconds),
			Details:     details,
		},
		Assumptions: assumptions,
		Engine: domain.EngineInfo{
			Name:     EngineName,
			Version:  Version,
			Protocol: "go-test-fuzz/v1",
		},
		Environment: domain.EnvironmentInfo{
			Arch:            runtime.GOARCH,
			OS:              runtime.GOOS,
			SandboxProfile:  "process-isolated",
			ToolchainDigest: harnessVersion(),
		},
		Artifacts: artifacts,
		Limits:    limits,
		CreatedAt: c.now,
	}
	if c.contractRef != nil {
		evidence.Inputs = []domain.CASRef{*c.contractRef}
	}
	return evidence
}

func harnessVersion() string {
	return runtime.Version()
}

// resultDigest is the stable summary digest recorded for successful steps.
func resultDigest(evidenceResult domain.ClaimResult, corpusSize int, crashInputs int, artifacts []domain.CASRef) string {
	digests := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		digests = append(digests, artifact.Digest)
	}
	digest, err := evidenceutil.CanonicalDigest(map[string]any{
		"engine":       EngineName,
		"version":      Version,
		"result":       string(evidenceResult),
		"corpus_size":  corpusSize,
		"crash_inputs": crashInputs,
		"artifacts":    digests,
	})
	if err != nil {
		return ""
	}
	return digest
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}
