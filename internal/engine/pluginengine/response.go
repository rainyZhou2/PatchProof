package pluginengine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/plugin"
)

// mapResponse turns a plugin response into an engine result. All identity,
// engine and environment metadata is filled in by the host; plugin-supplied
// drafts cannot spoof it.
func (e *Engine) mapResponse(ctx context.Context, request engine.Request, response plugin.StepResponse, outputDir, version string) (engine.Result, error) {
	if len(response.Evidence) > 0 || len(response.Counterexamples) > 0 {
		return engine.Result{}, errors.New("raw evidence/counterexample payloads are not supported; return evidence_drafts/counterexample_drafts")
	}
	if len(response.EvidenceDrafts) > maxEvidenceDrafts {
		return engine.Result{}, fmt.Errorf("plugin returned %d evidence drafts, limit is %d", len(response.EvidenceDrafts), maxEvidenceDrafts)
	}
	if len(response.CounterexampleDrafts) > maxCounterexampleDrafts {
		return engine.Result{}, fmt.Errorf("plugin returned %d counterexample drafts, limit is %d", len(response.CounterexampleDrafts), maxCounterexampleDrafts)
	}

	outcome := domain.StepOutcome(strings.ToUpper(strings.TrimSpace(response.Outcome)))
	if outcome == "" {
		outcome = domain.OutcomeFailed
	}
	if !outcome.Valid() {
		return engine.Result{}, fmt.Errorf("response outcome %q is not a valid step outcome", response.Outcome)
	}
	var class domain.ErrorClass
	if raw := strings.ToUpper(strings.TrimSpace(response.ErrorClass)); raw != "" {
		class = domain.ErrorClass(raw)
		if !class.Valid() {
			return engine.Result{}, fmt.Errorf("response error class %q is not a valid error class", response.ErrorClass)
		}
	}

	metrics := domain.StepMetrics{}
	if response.Metrics != nil {
		metrics = domain.StepMetrics{
			CPUSeconds:              response.Metrics.CPUSeconds,
			MemoryGiBSeconds:        response.Metrics.MemoryGiBSeconds,
			EphemeralDiskGiBSeconds: response.Metrics.EphemeralDiskGiBSeconds,
			EgressBytes:             response.Metrics.EgressBytes,
			CASWrittenBytes:         response.Metrics.CASWrittenBytes,
			QueueWaitMS:             response.Metrics.QueueWaitMS,
			ExecutionMS:             response.Metrics.ExecutionMS,
			UploadMS:                response.Metrics.UploadMS,
		}
	}
	if err := metrics.Validate(); err != nil {
		return engine.Result{}, fmt.Errorf("invalid metrics: %w", err)
	}

	artifacts := make([]domain.CASRef, 0, len(response.Artifacts)+len(response.Files))
	seen := make(map[string]struct{}, len(response.Artifacts)+len(response.Files))
	for index, raw := range response.Artifacts {
		ref, err := toDomainCASRef(raw, request.Spec.TenantID)
		if err != nil {
			return engine.Result{}, fmt.Errorf("artifact %d: %w", index, err)
		}
		if _, duplicate := seen[ref.URI()]; duplicate {
			continue
		}
		seen[ref.URI()] = struct{}{}
		artifacts = append(artifacts, ref)
	}

	files, uploadedRefs, err := uploadFiles(ctx, request.Store, request.Scope, outputDir, response.Files, request.Logger)
	if err != nil {
		return engine.Result{}, err
	}
	for _, ref := range uploadedRefs {
		if _, duplicate := seen[ref.URI()]; duplicate {
			continue
		}
		seen[ref.URI()] = struct{}{}
		artifacts = append(artifacts, ref)
	}

	now := time.Now().UTC()
	if request.Clock != nil {
		now = request.Clock.Now()
	}

	evidence := make([]domain.Evidence, 0, len(response.EvidenceDrafts))
	for index, draft := range response.EvidenceDrafts {
		entry, err := e.buildEvidence(request, draft, files, version, now)
		if err != nil {
			return engine.Result{}, fmt.Errorf("evidence draft %d: %w", index, err)
		}
		evidence = append(evidence, entry)
	}

	counterexamples := make([]domain.Counterexample, 0, len(response.CounterexampleDrafts))
	for index, draft := range response.CounterexampleDrafts {
		entry, err := buildCounterexample(request, draft, files, now)
		if err != nil {
			return engine.Result{}, fmt.Errorf("counterexample draft %d: %w", index, err)
		}
		counterexamples = append(counterexamples, entry)
	}

	return engine.Result{
		Outcome:         outcome,
		ErrorClass:      class,
		Message:         response.Message,
		Retryable:       response.Retryable,
		Evidence:        evidence,
		Counterexamples: counterexamples,
		Artifacts:       artifacts,
		Metrics:         metrics,
		Outputs:         cloneStringMap(response.Outputs),
	}, nil
}

func (e *Engine) buildEvidence(request engine.Request, draft plugin.EvidenceDraft, files map[string]uploadedFile, version string, now time.Time) (domain.Evidence, error) {
	claimID := strings.TrimSpace(draft.ClaimID)
	if claimID == "" {
		return domain.Evidence{}, errors.New("claim_id is required")
	}
	kind := domain.EvidenceKind(strings.TrimSpace(draft.Kind))
	if !kind.Valid() {
		return domain.Evidence{}, fmt.Errorf("unknown evidence kind %q", draft.Kind)
	}
	result := domain.ClaimResult(strings.TrimSpace(draft.Result))
	if !result.Valid() {
		return domain.Evidence{}, fmt.Errorf("unknown claim result %q", draft.Result)
	}
	artifacts, err := resolveArtifactNames(draft.ArtifactNames, files)
	if err != nil {
		return domain.Evidence{}, err
	}
	inputs, err := resolveInputNames(draft.InputNames, request.Staged)
	if err != nil {
		return domain.Evidence{}, err
	}

	entry := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      request.Spec.TenantID,
		RunID:         request.Spec.RunID,
		StepID:        request.Spec.StepID,
		ClaimID:       domain.ClaimID(claimID),
		Kind:          kind,
		Result:        result,
		Scope: domain.EvidenceScope{
			InputDomain:    draft.Scope.InputDomain,
			SeedCount:      draft.Scope.SeedCount,
			GeneratedCases: draft.Scope.GeneratedCases,
			ElapsedMS:      draft.Scope.ElapsedMS,
			Budget:         draft.Scope.Budget,
			Details:        cloneStringMap(draft.Scope.Details),
		},
		Assumptions: append([]string(nil), draft.Assumptions...),
		Limits:      append([]string(nil), draft.Limits...),
		Engine: domain.EngineInfo{
			Name:     e.cfg.Name,
			Version:  version,
			Protocol: Protocol,
		},
		Environment: domain.EnvironmentInfo{
			Arch:           runtime.GOARCH,
			SandboxProfile: string(request.Spec.Requirements.Isolation),
			Region:         request.Spec.Requirements.Region,
		},
		Inputs:    inputs,
		Artifacts: artifacts,
		CreatedAt: now,
	}
	if err := entry.Validate(); err != nil {
		return domain.Evidence{}, err
	}
	return entry, nil
}

func buildCounterexample(request engine.Request, draft plugin.CounterexampleDraft, files map[string]uploadedFile, now time.Time) (domain.Counterexample, error) {
	caseID := strings.TrimSpace(draft.CaseID)
	if caseID == "" {
		return domain.Counterexample{}, errors.New("case_id is required")
	}
	claimID := strings.TrimSpace(draft.ClaimID)
	if claimID == "" {
		return domain.Counterexample{}, errors.New("claim_id is required")
	}
	inputRef, err := resolveFile("input_file", draft.InputFile, files)
	if err != nil {
		return domain.Counterexample{}, err
	}
	baseRef, err := resolveFile("base_observation_file", draft.BaseObservationFile, files)
	if err != nil {
		return domain.Counterexample{}, err
	}
	headRef, err := resolveFile("head_observation_file", draft.HeadObservationFile, files)
	if err != nil {
		return domain.Counterexample{}, err
	}

	entry := domain.Counterexample{
		CaseID:             caseID,
		TenantID:           request.Spec.TenantID,
		RunID:              request.Spec.RunID,
		ClaimID:            domain.ClaimID(claimID),
		InputCodec:         strings.TrimSpace(draft.InputCodec),
		InputRef:           inputRef,
		BaseObservationRef: baseRef,
		HeadObservationRef: headRef,
		Reproduced:         draft.Reproduced,
		Minimized:          draft.Minimized,
		Scope:              draft.Scope,
		Nondeterminism:     append([]string(nil), draft.Nondeterminism...),
		CreatedAt:          now,
	}
	if err := entry.Validate(); err != nil {
		return domain.Counterexample{}, err
	}
	return entry, nil
}

func resolveArtifactNames(names []string, files map[string]uploadedFile) ([]domain.CASRef, error) {
	refs := make([]domain.CASRef, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, errors.New("artifact name is empty")
		}
		file, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("references unknown file artifact %q", name)
		}
		if _, duplicate := seen[file.Ref.URI()]; duplicate {
			continue
		}
		seen[file.Ref.URI()] = struct{}{}
		refs = append(refs, file.Ref)
	}
	return refs, nil
}

func resolveInputNames(names []string, staged map[string]engine.StagedInput) ([]domain.CASRef, error) {
	refs := make([]domain.CASRef, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, errors.New("input name is empty")
		}
		input, ok := staged[name]
		if !ok {
			return nil, fmt.Errorf("references unknown staged input %q", name)
		}
		ref := input.Ref
		if err := ref.Validate(); err != nil {
			return nil, fmt.Errorf("staged input %q: %w", name, err)
		}
		if _, duplicate := seen[ref.URI()]; duplicate {
			continue
		}
		seen[ref.URI()] = struct{}{}
		refs = append(refs, ref)
	}
	return refs, nil
}

func resolveFile(field, raw string, files map[string]uploadedFile) (domain.CASRef, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return domain.CASRef{}, fmt.Errorf("%s is required", field)
	}
	file, ok := files[name]
	if !ok {
		return domain.CASRef{}, fmt.Errorf("%s references unknown file artifact %q", field, name)
	}
	return file.Ref, nil
}

func toDomainCASRef(raw plugin.CASRef, tenant domain.TenantID) (domain.CASRef, error) {
	ref := domain.CASRef{
		Algorithm:   strings.TrimSpace(raw.Algorithm),
		Digest:      strings.TrimSpace(raw.Digest),
		SizeBytes:   raw.SizeBytes,
		TenantScope: strings.TrimSpace(raw.TenantScope),
	}
	if ref.Algorithm == "" {
		ref.Algorithm = "sha256"
	}
	if ref.TenantScope != "" && ref.TenantScope != string(tenant) {
		return domain.CASRef{}, fmt.Errorf("artifact tenant scope %q does not match the request tenant", ref.TenantScope)
	}
	if err := ref.Validate(); err != nil {
		return domain.CASRef{}, err
	}
	return ref, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
