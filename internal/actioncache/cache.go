// Package actioncache defines the hermetic action key and safe cloning rules
// for reusing completed step results across runs.
package actioncache

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/pkg/evidence"
)

const SchemaVersion = "patchproof.action/v1"

type KeyInput struct {
	TenantID       domain.TenantID
	CacheEpoch     string
	StepKind       domain.StepKind
	Engine         string
	EngineVersion  string
	SnapshotDigest string
	BaseSnapshot   string
	HeadSnapshot   string
	PolicyDigest   string
	InputManifest  string
	Architecture   string
	Isolation      string
	SubjectNetwork string
	RuntimeClass   string
	WorkerImage    string
	Options        map[string]string
}

func Key(input KeyInput) (string, error) {
	if input.TenantID.Validate() != nil {
		return "", fmt.Errorf("action cache: invalid tenant")
	}
	engine := strings.TrimSpace(input.Engine)
	engineVersion := strings.TrimSpace(input.EngineVersion)
	if !input.StepKind.Valid() || engine == "" {
		return "", fmt.Errorf("action cache: step kind and engine are required")
	}
	if engineVersion == "" {
		return "", fmt.Errorf("action cache: engine version is required")
	}
	options := make(map[string]string, len(input.Options))
	keys := make([]string, 0, len(input.Options))
	for key, value := range input.Options {
		options[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		ordered = append(ordered, key+"="+options[key])
	}
	return evidence.CanonicalDigest(struct {
		Schema         string   `json:"schema"`
		Tenant         string   `json:"tenant"`
		CacheEpoch     string   `json:"cache_epoch"`
		StepKind       string   `json:"step_kind"`
		Engine         string   `json:"engine"`
		EngineVersion  string   `json:"engine_version"`
		SnapshotDigest string   `json:"snapshot_digest"`
		BaseSnapshot   string   `json:"base_snapshot"`
		HeadSnapshot   string   `json:"head_snapshot"`
		PolicyDigest   string   `json:"policy_digest"`
		InputManifest  string   `json:"input_manifest"`
		Architecture   string   `json:"architecture"`
		Isolation      string   `json:"isolation"`
		SubjectNetwork string   `json:"subject_network"`
		RuntimeClass   string   `json:"runtime_class"`
		WorkerImage    string   `json:"worker_image"`
		Options        []string `json:"options"`
	}{
		Schema: SchemaVersion, Tenant: input.TenantID.String(), CacheEpoch: input.CacheEpoch,
		StepKind: string(input.StepKind), Engine: engine, EngineVersion: engineVersion,
		SnapshotDigest: input.SnapshotDigest,
		BaseSnapshot:   input.BaseSnapshot, HeadSnapshot: input.HeadSnapshot, PolicyDigest: input.PolicyDigest,
		InputManifest: input.InputManifest, Architecture: input.Architecture, Isolation: input.Isolation,
		SubjectNetwork: input.SubjectNetwork, RuntimeClass: input.RuntimeClass, WorkerImage: input.WorkerImage,
		Options: ordered,
	})
}

func KeyForStep(step domain.Step, run domain.Run, cacheEpoch, engineVersion, workerImage string) (string, error) {
	return Key(KeyInput{
		TenantID: step.TenantID, CacheEpoch: cacheEpoch, StepKind: step.Kind,
		Engine: step.Requirements.Engine, EngineVersion: engineVersion,
		SnapshotDigest: run.SnapshotDigest,
		BaseSnapshot:   run.BaseSnapshot, HeadSnapshot: run.HeadSnapshot, PolicyDigest: run.PolicyDigest,
		InputManifest: step.InputManifestDigest, Architecture: step.Requirements.Architecture,
		Isolation: string(step.Requirements.Isolation), SubjectNetwork: string(step.Requirements.SubjectNetwork),
		RuntimeClass: step.Requirements.RuntimeClass, WorkerImage: workerImage, Options: step.Options,
	})
}

// Cacheable reports whether a step is declared hermetic and deterministic
// enough to reuse. Only analysis and compilation steps are eligible; test,
// fuzz and security steps always re-execute.
func Cacheable(step domain.Step) bool {
	if step.Options == nil || !strings.EqualFold(step.Options["cache.enabled"], "true") {
		return false
	}
	switch step.Kind {
	case domain.StepKindAnalyze, domain.StepKindImpact, domain.StepKindCompile:
		return true
	default:
		return false
	}
}

// Storable reports whether a completed result can be cached. Discoveries
// (violations, counterexamples, unknown or invalid evidence) are never cached.
func Storable(result domain.StepResult) bool {
	if result.Outcome != domain.OutcomeSucceeded || len(result.Counterexamples) > 0 {
		return false
	}
	if err := result.Validate(); err != nil {
		return false
	}
	for _, item := range result.Evidence {
		if item.Result != domain.ClaimTested && item.Result != domain.ClaimProven {
			return false
		}
	}
	return true
}

func NewEntry(tenantID domain.TenantID, actionKey string, step domain.Step, engineVersion string, result domain.StepResult, ttl time.Duration, now time.Time) (domain.ActionCacheEntry, error) {
	if ttl <= 0 {
		return domain.ActionCacheEntry{}, fmt.Errorf("action cache: ttl must be positive")
	}
	now = now.UTC()
	entry := domain.ActionCacheEntry{
		TenantID: tenantID, ActionKey: actionKey, StepKind: step.Kind,
		Engine: step.Requirements.Engine, EngineVersion: engineVersion,
		Result: cloneResult(result), CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := entry.Validate(); err != nil {
		return domain.ActionCacheEntry{}, err
	}
	return entry, nil
}

// CloneForStep re-attributes a cached result to a new run and step. Evidence
// identifiers and timestamps are regenerated; artifacts remain
// content-addressed and tenant-scoped.
func CloneForStep(entry domain.ActionCacheEntry, runID domain.RunID, stepID domain.StepID, tenantID domain.TenantID, now time.Time) (domain.StepResult, error) {
	result := cloneResult(entry.Result)
	result.Outcome = domain.OutcomeSucceeded
	result.Metrics = domain.StepMetrics{}
	for i := range result.Evidence {
		item := &result.Evidence[i]
		item.ID = domain.NewEvidenceID()
		item.RunID = runID
		item.TenantID = tenantID
		item.StepID = stepID
		item.CreatedAt = now.UTC()
		if err := item.Validate(); err != nil {
			return domain.StepResult{}, fmt.Errorf("action cache: evidence %s: %w", item.ID, err)
		}
	}
	if len(result.Counterexamples) > 0 {
		return domain.StepResult{}, fmt.Errorf("action cache: counterexamples are not cacheable")
	}
	result.FinishedAt = now.UTC()
	digest, err := evidence.CanonicalDigest(struct {
		Evidence        []domain.Evidence       `json:"evidence"`
		Counterexamples []domain.Counterexample `json:"counterexamples"`
		Outcome         domain.StepOutcome      `json:"outcome"`
		Outputs         map[string]string       `json:"outputs"`
	}{
		Evidence: result.Evidence, Counterexamples: result.Counterexamples,
		Outcome: result.Outcome, Outputs: result.Outputs,
	})
	if err != nil {
		return domain.StepResult{}, err
	}
	result.ResultDigest = digest
	if err := result.Validate(); err != nil {
		return domain.StepResult{}, err
	}
	return result, nil
}

func cloneResult(result domain.StepResult) domain.StepResult {
	cloned := result
	cloned.Artifacts = append([]domain.CASRef(nil), result.Artifacts...)
	if result.Outputs != nil {
		cloned.Outputs = make(map[string]string, len(result.Outputs))
		for key, value := range result.Outputs {
			cloned.Outputs[key] = value
		}
	}
	cloned.Evidence = make([]domain.Evidence, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		copyItem := item
		copyItem.Assumptions = append([]string(nil), item.Assumptions...)
		copyItem.Limits = append([]string(nil), item.Limits...)
		copyItem.Inputs = append([]domain.CASRef(nil), item.Inputs...)
		copyItem.Artifacts = append([]domain.CASRef(nil), item.Artifacts...)
		if item.Scope.Details != nil {
			copyItem.Scope.Details = make(map[string]string, len(item.Scope.Details))
			for key, value := range item.Scope.Details {
				copyItem.Scope.Details[key] = value
			}
		}
		if item.Environment.Extra != nil {
			copyItem.Environment.Extra = make(map[string]string, len(item.Environment.Extra))
			for key, value := range item.Environment.Extra {
				copyItem.Environment.Extra[key] = value
			}
		}
		cloned.Evidence = append(cloned.Evidence, copyItem)
	}
	cloned.Counterexamples = make([]domain.Counterexample, 0, len(result.Counterexamples))
	for _, item := range result.Counterexamples {
		copyItem := item
		copyItem.Nondeterminism = append([]string(nil), item.Nondeterminism...)
		cloned.Counterexamples = append(cloned.Counterexamples, copyItem)
	}
	return cloned
}
