package godifferential

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/pkg/contract"
	evidenceutil "github.com/patchproof/patchproof/pkg/evidence"
)

const maxArtifactBytes = 8 << 20

type caseRecord struct {
	CaseID     string          `json:"case_id"`
	Input      json.RawMessage `json:"input"`
	BaseStatus string          `json:"base_status"`
	HeadStatus string          `json:"head_status"`
	Diff       bool            `json:"diff"`
}

type counterexampleRecord struct {
	CaseID          string          `json:"case_id"`
	Input           json.RawMessage `json:"input"`
	BaseObservation json.RawMessage `json:"base_observation"`
	HeadObservation json.RawMessage `json:"head_observation"`
	BaseStatus      string          `json:"base_status"`
	HeadStatus      string          `json:"head_status"`
	Reproduced      bool            `json:"reproduced"`
	Minimized       bool            `json:"minimized"`
	ShrinkSteps     int             `json:"shrink_steps"`
	Scope           string          `json:"scope"`
	Note            string          `json:"note,omitempty"`
}

func (c *config) putBytes(ctx context.Context, data []byte) (domain.CASRef, error) {
	return c.store.Put(ctx, c.scope, bytes.NewReader(data))
}

func (c *config) putLog(ctx context.Context, payload map[string]any) (*domain.CASRef, error) {
	ref, err := c.putBytes(ctx, mustJSON(payload))
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// buildCounterexample persists the canonical input and both observations and
// returns the domain record together with its JSON artifact form.
func (c *config) buildCounterexample(ctx context.Context, item caseInput, base, head harness.Observation, minimized bool, shrinkSteps int) (domain.Counterexample, counterexampleRecord, error) {
	inputRef, err := c.putBytes(ctx, item.raw)
	if err != nil {
		return domain.Counterexample{}, counterexampleRecord{}, fmt.Errorf("store counterexample input: %w", err)
	}
	baseCanonical, err := base.Canonical()
	if err != nil {
		return domain.Counterexample{}, counterexampleRecord{}, fmt.Errorf("canonicalize base observation: %w", err)
	}
	headCanonical, err := head.Canonical()
	if err != nil {
		return domain.Counterexample{}, counterexampleRecord{}, fmt.Errorf("canonicalize head observation: %w", err)
	}
	baseRef, err := c.putBytes(ctx, baseCanonical)
	if err != nil {
		return domain.Counterexample{}, counterexampleRecord{}, fmt.Errorf("store base observation: %w", err)
	}
	headRef, err := c.putBytes(ctx, headCanonical)
	if err != nil {
		return domain.Counterexample{}, counterexampleRecord{}, fmt.Errorf("store head observation: %w", err)
	}
	counterexample := domain.Counterexample{
		CaseID:             item.id(),
		TenantID:           c.tenant,
		RunID:              c.runID,
		ClaimID:            c.opts.claimID,
		InputCodec:         contractInputCodec(),
		InputRef:           inputRef,
		BaseObservationRef: baseRef,
		HeadObservationRef: headRef,
		Reproduced:         true,
		Scope:              c.opts.inputDomain,
		Minimized:          minimized,
		CreatedAt:          c.now,
	}
	record := counterexampleRecord{
		CaseID:          counterexample.CaseID,
		Input:           item.raw,
		BaseObservation: baseCanonical,
		HeadObservation: headCanonical,
		BaseStatus:      base.Status,
		HeadStatus:      head.Status,
		Reproduced:      true,
		Minimized:       minimized,
		ShrinkSteps:     shrinkSteps,
		Scope:           c.opts.inputDomain,
	}
	if c.contract.Spec.Kind == "safety" && base.Status == harness.StatusPanic && head.Status == harness.StatusPanic {
		record.Note = "safety contract violated: the head panicked even though the base panicked as well"
	}
	return counterexample, record, nil
}

func contractInputCodec() string {
	return contract.InputJSONArgs
}

func buildCasesJSONL(records []caseRecord) ([]byte, int) {
	var buffer bytes.Buffer
	omitted := 0
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			continue
		}
		if buffer.Len()+len(line)+1 > maxArtifactBytes {
			omitted++
			continue
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	return buffer.Bytes(), omitted
}

func buildCounterexamplesJSON(records []counterexampleRecord) ([]byte, int) {
	truncated := 0
	for len(records) > 0 {
		encoded, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return []byte("[]"), len(records)
		}
		if len(encoded) <= maxArtifactBytes {
			return encoded, truncated
		}
		records = records[:len(records)-1]
		truncated++
	}
	return []byte("[]"), truncated
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 64<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sourceDigest(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

func (c *config) harnessDigestText(baseSource, headSource []byte) []byte {
	baseBinaryDigest, err := fileDigest(c.baseBinary)
	if err != nil {
		baseBinaryDigest = "unavailable"
	}
	headBinaryDigest, err := fileDigest(c.headBinary)
	if err != nil {
		headBinaryDigest = "unavailable"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "engine=go-differential/%s\n", Version)
	fmt.Fprintf(&builder, "protocol=%s\n", harness.ProtocolVersion)
	fmt.Fprintf(&builder, "go_version=%s\n", harness.GoVersion())
	fmt.Fprintf(&builder, "os=%s\narch=%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&builder, "target=%s\n", c.symbol.PackagePath+"."+c.symbol.Name)
	fmt.Fprintf(&builder, "package=%s\n", c.symbol.PackagePath)
	fmt.Fprintf(&builder, "base_source_sha256=%s\n", sourceDigest(baseSource))
	fmt.Fprintf(&builder, "head_source_sha256=%s\n", sourceDigest(headSource))
	fmt.Fprintf(&builder, "base_binary_sha256=%s\n", baseBinaryDigest)
	fmt.Fprintf(&builder, "head_binary_sha256=%s\n", headBinaryDigest)
	return []byte(builder.String())
}

func (c *config) buildEvidence(stats *statistics, elapsedMS int64, result domain.ClaimResult, artifacts []domain.CASRef) domain.Evidence {
	limits := append([]string(nil), stats.limits...)
	limits = append(limits,
		"seed generation is type-directed over JSON-encodable values; invalid UTF-8 strings and negative zero are not representable and are not covered",
		"no fix-invariant expression is evaluated; differences are compared directly on return values and panic observations",
		"concurrency is one: each batch is executed sequentially against base and head",
	)
	if stats.invalidCases > 0 {
		limits = append(limits, fmt.Sprintf("%d cases were invalid (arguments could not be decoded into the target signature) and are not evidence", stats.invalidCases))
	}
	if stats.nondeterministic > 0 {
		limits = append(limits, fmt.Sprintf("%d differing cases were not reproducible in fresh processes and were not reported", stats.nondeterministic))
	}
	if stats.validCases == 0 {
		limits = append(limits, "no valid case was executed against both snapshots")
	}
	if c.contractFallback {
		limits = append(limits, "contract was re-decoded from YAML through JSON so that native YAML seed arguments could be represented; contract.Document validation still applied")
	}
	limits = dedupeStrings(limits)

	assumptions := append([]string(nil), c.contract.Spec.Assumptions...)
	assumptions = append(assumptions,
		"the target function is deterministic for identical inputs",
		"observations are compared after applying the contract's normalize rules; timing metadata is excluded",
	)
	assumptions = dedupeStrings(assumptions)

	details := map[string]string{
		"seed":                     fmt.Sprintf("%d", c.opts.seed),
		"cases":                    fmt.Sprintf("%d", stats.validCases),
		"executed_cases":           fmt.Sprintf("%d", stats.executed),
		"invalid_cases":            fmt.Sprintf("%d", stats.invalidCases),
		"distinct_counterexamples": fmt.Sprintf("%d", len(stats.counterexamples)),
		"diff_ratio":               fmt.Sprintf("%.6f", stats.diffRatio()),
		"strategy":                 "boundary-seeds,corpus-replay,mutations,delta-debugging,reproduction",
		"shrink_steps":             fmt.Sprintf("%d", stats.shrinkSteps),
		"nondeterministic_cases":   fmt.Sprintf("%d", stats.nondeterministic),
		"infrastructure_failures":  fmt.Sprintf("%d", stats.infraFailures),
		"timeouts":                 fmt.Sprintf("%d", stats.timeouts),
	}
	if len(stats.nondeterminism) > 0 {
		details["nondeterminism"] = strings.Join(stats.nondeterminism, "; ")
	}
	for key, value := range c.generatorSummary(stats) {
		details[key] = value
	}

	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      c.tenant,
		RunID:         c.runID,
		StepID:        stats.stepID,
		ClaimID:       c.opts.claimID,
		Kind:          domain.EvidenceDifferential,
		Result:        result,
		Scope: domain.EvidenceScope{
			InputDomain:    c.opts.inputDomain,
			SeedCount:      stats.seedCount,
			GeneratedCases: stats.validCases,
			ElapsedMS:      elapsedMS,
			Budget:         c.budgetSummary(),
			Details:        details,
		},
		Assumptions: assumptions,
		Engine: domain.EngineInfo{
			Name:     EngineName,
			Version:  Version,
			Protocol: harness.ProtocolVersion,
		},
		Environment: domain.EnvironmentInfo{
			Arch:            runtime.GOARCH,
			OS:              runtime.GOOS,
			SandboxProfile:  "process-isolated",
			ToolchainDigest: harness.GoVersion(),
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

func (c *config) generatorSummary(stats *statistics) map[string]string {
	if stats.generator == nil {
		return nil
	}
	return stats.generator.summary()
}

func (c *config) budgetSummary() string {
	return fmt.Sprintf("max_cases=%d;seconds=%d;batch=%d;min_cases=%d",
		c.opts.maxCases, int(c.opts.duration.Seconds()), c.opts.batchSize, c.opts.minCases)
}

// resultDigest is the stable digest the executor records for successful steps.
func resultDigest(counterexamples []domain.Counterexample, validCases int64, artifacts []domain.CASRef) string {
	ids := make([]string, 0, len(counterexamples))
	for _, counterexample := range counterexamples {
		ids = append(ids, counterexample.CaseID)
	}
	sort.Strings(ids)
	artifactDigests := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifactDigests = append(artifactDigests, artifact.Digest)
	}
	digest, err := evidenceutil.CanonicalDigest(map[string]any{
		"engine":           EngineName,
		"version":          Version,
		"counterexamples":  ids,
		"cases":            validCases,
		"artifact_digests": artifactDigests,
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

// statistics accumulates execution accounting for evidence and limits.
type statistics struct {
	executed         int64
	validCases       int64
	invalidCases     int64
	diffCases        int64
	nondeterministic int
	infraFailures    int
	timeouts         int
	shrinkSteps      int
	seedCount        int64
	stepID           domain.StepID
	generator        *generator
	counterexamples  []domain.Counterexample
	ceRecords        []counterexampleRecord
	caseRecords      []caseRecord
	limits           []string
	nondeterminism   []string
}

func (s *statistics) diffRatio() float64 {
	if s.validCases == 0 {
		return 0
	}
	return float64(s.diffCases) / float64(s.validCases)
}
