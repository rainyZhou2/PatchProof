// Package godifferential implements the counterexample-driven differential
// verification engine. It builds a reflection harness into both the base and
// head snapshots, executes shared JSON inputs against both binaries, minimizes
// the first reproducible differences and records them as CAS-backed evidence.
package godifferential

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

const (
	// EngineName is the registry name of this engine.
	EngineName = "go-differential"
	// Version is the engine protocol version reported in evidence.
	Version = "0.1.0"

	buildTimeout    = 3 * time.Minute
	describeTimeout = 30 * time.Second
	maxInfraRetries = 3
)

// Engine is the go-differential differential fuzzing engine.
type Engine struct{}

// New constructs the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string { return EngineName }

func (*Engine) Version() string { return Version }

func (*Engine) Kinds() []domain.StepKind {
	return []domain.StepKind{domain.StepKindDifferential}
}

func (*Engine) Languages() []string { return []string{"go"} }

// Execute runs the differential workload for one staged base/head pair.
func (e *Engine) Execute(ctx context.Context, request engine.Request) (engine.Result, error) {
	started := time.Now()
	cfg, failed := newConfig(ctx, request)
	if failed != nil {
		return *failed, nil
	}
	defer cfg.runCleanup()
	if !request.Spec.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, request.Spec.Deadline)
		defer cancel()
	}

	if err := ctx.Err(); err != nil {
		return canceledResult(err), nil
	}

	for side, snapshot := range map[string]string{"base": cfg.baseDir, "head": cfg.headDir} {
		if _, err := harness.PackageDir(snapshot, cfg.baseModule, cfg.symbol.PackagePath); err != nil {
			return *failure(domain.ErrEngineUnsupported, fmt.Sprintf("%s snapshot: %s", side, err.Error()), false, nil), nil
		}
	}

	baseSource, err := harness.GenerateSource(cfg.symbol.PackagePath, cfg.symbol.Name)
	if err != nil {
		return *failure(domain.ErrEngineUnsupported, err.Error(), false, nil), nil
	}
	headSource := baseSource
	baseCleanup, err := harness.WriteSource(cfg.baseDir, baseSource)
	if err != nil {
		return *failure(domain.ErrEngineUnsupported, "install base harness: "+err.Error(), false, nil), nil
	}
	cfg.cleanup = append(cfg.cleanup, baseCleanup)
	headCleanup, err := harness.WriteSource(cfg.headDir, headSource)
	if err != nil {
		return *failure(domain.ErrEngineUnsupported, "install head harness: "+err.Error(), false, nil), nil
	}
	cfg.cleanup = append(cfg.cleanup, headCleanup)

	baseBuild := cfg.env.Build(ctx, cfg.baseDir, cfg.baseBinary, "./.patchproof/harness", buildTimeout)
	if !baseBuild.Successful() {
		return cfg.buildFailure(ctx, "base", baseBuild), nil
	}
	headBuild := cfg.env.Build(ctx, cfg.headDir, cfg.headBinary, "./.patchproof/harness", buildTimeout)
	if !headBuild.Successful() {
		return cfg.buildFailure(ctx, "head", headBuild), nil
	}

	baseSignature, _, err := cfg.env.Describe(ctx, cfg.baseBinary, cfg.baseDir, describeTimeout)
	if err != nil {
		return cfg.describeFailure(ctx, "base", err), nil
	}
	headSignature, _, err := cfg.env.Describe(ctx, cfg.headBinary, cfg.headDir, describeTimeout)
	if err != nil {
		return cfg.describeFailure(ctx, "head", err), nil
	}

	initialLimits := make([]string, 0, 8)
	if !signaturesEqual(baseSignature, headSignature) {
		initialLimits = append(initialLimits, "base and head signatures differ; cases are generated from the base signature and the head may reject them")
	}

	stats := &statistics{
		seedCount: 0,
		stepID:    request.Spec.StepID,
		limits:    initialLimits,
	}
	stats.generator = newGenerator(baseSignature, cfg.contract.Spec.Seeds, cfg.contract.Spec.Domain.Generators, cfg.opts.maxCases, cfg.opts.seed)
	stats.seedCount = stats.generator.seedCount

	runner := &executor{
		env:          cfg.env,
		baseBinary:   cfg.baseBinary,
		headBinary:   cfg.headBinary,
		baseDir:      cfg.baseDir,
		headDir:      cfg.headDir,
		signature:    baseSignature,
		contractKind: cfg.contract.Spec.Kind,
		oracle:       cfg.oracle,
		batchTimeout: cfg.opts.batchTimeout,
	}

	if cfg.replay != nil {
		if failed := cfg.executeReplay(ctx, runner, stats); failed != nil {
			return *failed, nil
		}
		return cfg.finishRun(ctx, started, stats, baseSource, headSource)
	}

	var pending []caseInput
	reported := make(map[string]struct{})
	duplicateMinimizations := 0
	duplicateLimit := maxInt(cfg.opts.maxCounterexamples*2, 8)
	batchNumber := 0
	for {
		if err := ctx.Err(); err != nil {
			return canceledResult(err), nil
		}
		if time.Since(started) >= cfg.opts.duration {
			stats.limits = append(stats.limits, fmt.Sprintf("time budget of %s exhausted before the case budget", cfg.opts.duration))
			break
		}
		for len(pending) < cfg.opts.batchSize {
			item, ok := stats.generator.Next()
			if !ok {
				break
			}
			pending = append(pending, item)
		}
		if len(pending) == 0 {
			break
		}
		batch := pending
		pending = nil
		batchNumber++
		baseObservations, headObservations, infra := runner.runCases(ctx, batch)
		if infra != "" {
			stats.infraFailures++
			if strings.Contains(infra, "TIMEOUT") {
				stats.timeouts++
			}
			stats.limits = append(stats.limits, fmt.Sprintf("batch %d: %s", batchNumber, infra))
			if stats.infraFailures >= maxInfraRetries {
				stats.limits = append(stats.limits, fmt.Sprintf("aborted after %d infrastructure failures", stats.infraFailures))
				break
			}
			continue
		}

		stop := false
		for index := range batch {
			stats.executed++
			baseObservation := baseObservations[index]
			headObservation := headObservations[index]
			if baseObservation.Status == harness.StatusInvalid || headObservation.Status == harness.StatusInvalid {
				stats.invalidCases++
				continue
			}
			stats.validCases++
			finding := isFinding(baseObservation, headObservation, cfg.contract.Spec.Kind, cfg.oracle)
			stats.caseRecords = append(stats.caseRecords, caseRecord{
				CaseID:     batch[index].id(),
				Input:      batch[index].raw,
				BaseStatus: baseObservation.Status,
				HeadStatus: headObservation.Status,
				Diff:       finding,
			})
			if !finding {
				continue
			}
			stats.diffCases++

			minimized, shrinkSteps, minimizedOK := runner.minimize(ctx, batch[index])
			stats.shrinkSteps += shrinkSteps
			baseRepro, headRepro, reproduced, note := runner.reproduce(ctx, minimized)
			if !reproduced {
				stats.nondeterministic++
				message := fmt.Sprintf("case %s differed during the run but not on fresh processes: %s", minimized.id(), note)
				stats.nondeterminism = append(stats.nondeterminism, message)
				continue
			}
			counterexample, record, err := cfg.buildCounterexample(ctx, minimized, baseRepro, headRepro, minimizedOK, shrinkSteps)
			if err != nil {
				return casFailure(err), nil
			}
			if _, duplicate := reported[counterexample.CaseID]; duplicate {
				duplicateMinimizations++
				if duplicateMinimizations >= duplicateLimit {
					stats.limits = append(stats.limits, fmt.Sprintf("stopped after %d differing cases minimized to already-reported inputs", duplicateMinimizations))
					stop = true
					break
				}
				continue
			}
			reported[counterexample.CaseID] = struct{}{}
			stats.counterexamples = append(stats.counterexamples, counterexample)
			stats.ceRecords = append(stats.ceRecords, record)
			if len(stats.counterexamples) >= cfg.opts.maxCounterexamples {
				stats.limits = append(stats.limits, fmt.Sprintf("stopped after %d distinct counterexamples", len(stats.counterexamples)))
				stop = true
				break
			}
		}
		if stop {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledResult(err), nil
	}
	return cfg.finishRun(ctx, started, stats, baseSource, headSource)
}

func (cfg *config) finishRun(ctx context.Context, started time.Time, stats *statistics, baseSource, headSource []byte) (engine.Result, error) {
	if stats.generator != nil {
		if stats.generator.invalidSeeds > 0 {
			stats.limits = append(stats.limits, fmt.Sprintf("%d contract seeds had the wrong arity and were ignored", stats.generator.invalidSeeds))
		}
		if stats.generator.oversize > 0 {
			stats.limits = append(stats.limits, fmt.Sprintf("%d generated cases exceeded the 1 MiB input cap and were skipped", stats.generator.oversize))
		}
		if stats.generator.generationErr != nil {
			stats.limits = append(stats.limits, stats.generator.generationErr.Error())
		}
	}
	if len(stats.nondeterminism) > 0 {
		for _, message := range stats.nondeterminism {
			stats.limits = append(stats.limits, message)
		}
	}

	elapsed := time.Since(started)
	claimResult := claimResultFor(stats, cfg.opts.minCases)
	if cfg.replay != nil && len(stats.counterexamples) > 0 {
		claimResult = domain.ClaimViolated
	}

	casesJSONL, omitted := buildCasesJSONL(stats.caseRecords)
	if omitted > 0 {
		stats.limits = append(stats.limits, fmt.Sprintf("%d cases omitted from cases.jsonl to stay under 8 MiB", omitted))
	}
	counterexamplesJSON, truncated := buildCounterexamplesJSON(stats.ceRecords)
	if truncated > 0 {
		stats.limits = append(stats.limits, fmt.Sprintf("%d counterexample records truncated from counterexamples.json to stay under 8 MiB", truncated))
	}
	digestText := cfg.harnessDigestText(baseSource, headSource)

	artifactData := [][]byte{casesJSONL, counterexamplesJSON, digestText}
	artifacts := make([]domain.CASRef, 0, len(artifactData))
	for index, data := range artifactData {
		ref, err := cfg.putBytes(ctx, data)
		if err != nil {
			return casFailure(fmt.Errorf("store artifact %d: %w", index, err)), nil
		}
		artifacts = append(artifacts, ref)
	}

	evidence := cfg.buildEvidence(stats, elapsed.Milliseconds(), claimResult, artifacts)
	if len(artifacts) == 3 {
		if evidence.Scope.Details == nil {
			evidence.Scope.Details = map[string]string{}
		}
		evidence.Scope.Details["cases_artifact"] = artifacts[0].URI()
		evidence.Scope.Details["counterexamples_artifact"] = artifacts[1].URI()
		evidence.Scope.Details["harness_digest_artifact"] = artifacts[2].URI()
	}
	digest := resultDigest(stats.counterexamples, stats.validCases, artifacts)
	generated := 0
	if stats.generator != nil {
		generated = stats.generator.produced
	}
	return engine.Result{
		Outcome:         domain.OutcomeSucceeded,
		Evidence:        []domain.Evidence{evidence},
		Counterexamples: stats.counterexamples,
		Artifacts:       artifacts,
		Metrics:         domain.StepMetrics{ExecutionMS: elapsed.Milliseconds()},
		Outputs: map[string]string{
			"cases":                    fmt.Sprintf("%d", stats.validCases),
			"counterexamples":          fmt.Sprintf("%d", len(stats.counterexamples)),
			"evidence":                 string(evidence.ID),
			"result_digest":            digest,
			"generated_cases":          fmt.Sprintf("%d", generated),
			"cases_artifact":           artifacts[0].URI(),
			"counterexamples_artifact": artifacts[1].URI(),
			"harness_digest_artifact":  artifacts[2].URI(),
		},
	}, nil
}

func (c *config) runCleanup() {
	for index := len(c.cleanup) - 1; index >= 0; index-- {
		c.cleanup[index]()
	}
}

func (c *config) buildFailure(ctx context.Context, side string, result harness.ProcessResult) engine.Result {
	logText := truncateText(string(result.Stdout)+string(result.Stderr), maxArtifactBytes)
	artifact, err := c.putLog(ctx, map[string]any{
		"stage":     "build",
		"side":      side,
		"exit_code": result.ExitCode,
		"timed_out": result.TimedOut,
		"log":       logText,
	})
	artifacts := make([]domain.CASRef, 0, 1)
	if err == nil && artifact != nil {
		artifacts = append(artifacts, *artifact)
	}
	if result.TimedOut || result.Canceled {
		outcome := domain.OutcomeTimedOut
		message := fmt.Sprintf("%s harness build timed out", side)
		if result.Canceled {
			outcome = domain.OutcomeCancelled
			message = fmt.Sprintf("%s harness build was canceled", side)
		}
		return engine.Result{Outcome: outcome, Message: message, Retryable: false, Artifacts: artifacts}
	}
	message := fmt.Sprintf("%s target build failed: %s", side, truncateText(logText, 2048))
	return *failure(domain.ErrEngineUnsupported, message, false, artifacts)
}

func (c *config) describeFailure(ctx context.Context, side string, describeErr error) engine.Result {
	artifact, err := c.putLog(ctx, map[string]any{
		"stage": "describe",
		"side":  side,
		"error": describeErr.Error(),
	})
	artifacts := make([]domain.CASRef, 0, 1)
	if err == nil && artifact != nil {
		artifacts = append(artifacts, *artifact)
	}
	return *failure(domain.ErrEngineUnsupported, fmt.Sprintf("%s harness describe failed: %s", side, describeErr.Error()), false, artifacts)
}

func signaturesEqual(base, head harness.Signature) bool {
	if base.Arity != head.Arity || base.Variadic != head.Variadic {
		return false
	}
	if !reflect.DeepEqual(base.Params, head.Params) {
		return false
	}
	baseJSON, baseErr := json.Marshal(base.Params)
	headJSON, headErr := json.Marshal(head.Params)
	if baseErr != nil || headErr != nil {
		return false
	}
	return string(baseJSON) == string(headJSON)
}

func claimResultFor(stats *statistics, minCases int64) domain.ClaimResult {
	if len(stats.counterexamples) > 0 {
		return domain.ClaimViolated
	}
	if stats.validCases >= minCases {
		return domain.ClaimTested
	}
	return domain.ClaimUnknown
}

func canceledResult(err error) engine.Result {
	return engine.Result{
		Outcome:   domain.OutcomeCancelled,
		Message:   "context canceled: " + err.Error(),
		Retryable: false,
	}
}

func casFailure(err error) engine.Result {
	return engine.Result{
		Outcome:    domain.OutcomeRetryable,
		ErrorClass: domain.ErrCASUnavailable,
		Message:    err.Error(),
		Retryable:  true,
	}
}

func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "...(truncated)"
}
