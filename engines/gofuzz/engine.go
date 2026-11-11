package gofuzz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

const (
	// EngineName is the registry name of this engine.
	EngineName = "go-native-fuzz"
	// Version is the engine protocol version reported in evidence.
	Version = "0.1.0"
)

// Engine is the Go-native fuzzing engine.
type Engine struct{}

// New constructs the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string { return EngineName }

func (*Engine) Version() string { return Version }

func (*Engine) Kinds() []domain.StepKind {
	return []domain.StepKind{domain.StepKindDifferential}
}

func (*Engine) Languages() []string { return []string{"go"} }

// Execute generates a fuzz target in the head snapshot and runs the Go fuzzing
// engine against it for the configured duration.
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
	if cfg.toolErr != nil {
		return cfg.unknownResult(started, "tool-unavailable", "go toolchain environment is unavailable: "+cfg.toolErr.Error(), nil), nil
	}

	source, err := GenerateTarget(cfg.symbol.PackagePath, cfg.symbol.Name, cfg.contract.Spec.Kind, cfg.seeds)
	if err != nil {
		return *failure(domain.ErrEngineUnsupported, err.Error(), false, nil), nil
	}
	cleanup, err := installTarget(cfg.headDir, source)
	if err != nil {
		return *failure(domain.ErrEngineUnsupported, "install fuzz target: "+err.Error(), false, nil), nil
	}
	defer cleanup()

	fuzzCache := filepath.Join(cfg.workspace, "fuzzcache")
	defer os.RemoveAll(fuzzCache)

	// The public -fuzzcache flag does not exist in current Go releases. Use it
	// when the toolchain advertises it; otherwise pass the equivalent hidden
	// test-binary flag so the cache still lives under the step workspace.
	argv := []string{
		cfg.env.GoBinary,
		"test",
		"-run=^$",
		"-fuzz=FuzzTarget",
		"-fuzztime=" + strconv.Itoa(cfg.seconds) + "s",
		"-parallel=1",
	}
	if supportsFuzzCacheFlag(ctx, cfg.env) {
		argv = append(argv, "-fuzzcache="+fuzzCache, "./.patchproof")
	} else {
		cfg.fuzzCacheFallback = true
		argv = append(argv, "./.patchproof", "-test.fuzzcachedir="+fuzzCache)
	}
	run := harness.Run(ctx, harness.ProcessSpec{
		Dir:       cfg.headDir,
		Argv:      argv,
		Env:       cfg.env.EnvVars(),
		Timeout:   time.Duration(cfg.seconds)*time.Second + fuzzTimeoutGrace,
		MaxStdout: 8 << 20,
		MaxStderr: 8 << 20,
	})
	output := truncateOutput(string(run.Stdout) + string(run.Stderr))

	if err := ctx.Err(); err != nil {
		return canceledResult(err), nil
	}
	if run.StartError != nil {
		return cfg.unknownResult(started, "tool-unavailable", "the go toolchain could not be started: "+run.StartError.Error(), nil), nil
	}
	if run.TimedOut || run.Canceled || run.OutputOverflow {
		reason := "fuzzing was canceled"
		if run.TimedOut {
			reason = "fuzzing exceeded the process timeout"
		}
		if run.OutputOverflow {
			reason = "fuzzing produced output beyond the capture limit"
		}
		return cfg.unknownResult(started, "timeout", reason, []string{reason}), nil
	}

	artifacts := make([]domain.CASRef, 0, maxCorpusFiles+2)
	fuzzLog, logErr := cfg.uploadLog(ctx, "fuzz", output, run.ExitCode)
	if logErr != nil {
		return casFailure(logErr), nil
	}
	artifacts = append(artifacts, fuzzLog)
	limits := make([]string, 0, 8)
	files := corpusFiles(cfg.headDir)
	crashInputs := 0
	for _, path := range files {
		if crashInputs >= maxCorpusFiles {
			limits = append(limits, fmt.Sprintf("only the first %d crash corpus inputs were uploaded", maxCorpusFiles))
			break
		}
		data, readErr := readBounded(path, maxCorpusFileBytes)
		if readErr != nil {
			limits = append(limits, fmt.Sprintf("crash corpus input %s could not be read: %v", filepath.Base(path), readErr))
			continue
		}
		ref, putErr := cfg.putBytes(ctx, data)
		if putErr != nil {
			return casFailure(putErr), nil
		}
		artifacts = append(artifacts, ref)
		crashInputs++
	}
	if crashInputs == 0 && len(files) > 0 {
		limits = append(limits, "crash corpus files were written by the fuzzer but none could be uploaded")
	}

	crash, buildFailure := classifyFuzzOutput(output)
	if run.ExitCode == 0 {
		cacheEntries := countFiles(fuzzCache, 10000)
		details := map[string]string{
			"corpus_size":        strconv.Itoa(int(cfg.seedCount) + crashInputs + cacheEntries),
			"crash_inputs":       "0",
			"fuzz_cache_entries": strconv.Itoa(cacheEntries),
			"exit_code":          "0",
		}
		limits = append(limits, "no crash was observed; this is bounded negative evidence only")
		return cfg.finish(started, domain.ClaimTested, details, artifacts, limits), nil
	}

	if crash {
		reproduction, reproduced := cfg.reproduce(ctx)
		reproductionOutput := truncateOutput(string(reproduction.Stdout) + string(reproduction.Stderr))
		if reproductionOutput != "" {
			if ref, err := cfg.uploadLog(ctx, "reproduction", reproductionOutput, reproduction.ExitCode); err == nil {
				artifacts = append(artifacts, ref)
			}
		}
		details := map[string]string{
			"corpus_size":  strconv.Itoa(int(cfg.seedCount) + crashInputs),
			"crash_inputs": strconv.Itoa(crashInputs),
			"exit_code":    strconv.Itoa(run.ExitCode),
		}
		if crashInputs == 0 {
			limits = append(limits, "the crash originated from the seed corpus; the fuzzer did not write a testdata corpus entry")
		}
		if cfg.contract.Spec.Kind != "safety" {
			limits = append(limits, fmt.Sprintf("contract kind is %s, which does not assert a no-panic invariant; the panic is reported as a head-side crash finding", cfg.contract.Spec.Kind))
		}
		if !reproduced {
			limits = append(limits, "the crash was not reproducible when the saved corpus was replayed; it is not reported as a violation")
			return cfg.finish(started, domain.ClaimUnknown, details, artifacts, limits), nil
		}
		return cfg.finish(started, domain.ClaimViolated, details, artifacts, limits), nil
	}

	if buildFailure {
		return *failure(domain.ErrEngineUnsupported, "fuzz target build failed: "+truncateForMessage(output), false, artifacts), nil
	}
	limits = append(limits, "go test exited non-zero without a recognizable crash or build failure")
	return cfg.finish(started, domain.ClaimUnknown, map[string]string{
		"corpus_size":  strconv.Itoa(int(cfg.seedCount) + crashInputs),
		"crash_inputs": strconv.Itoa(crashInputs),
		"exit_code":    strconv.Itoa(run.ExitCode),
	}, artifacts, limits), nil
}

// supportsFuzzCacheFlag reports whether the Go toolchain documents a
// -fuzzcache test flag. Current releases do not, so the engine falls back to
// -test.fuzzcachedir (and to the workspace-local GOCACHE).
func supportsFuzzCacheFlag(ctx context.Context, env harness.Environment) bool {
	result := harness.Run(ctx, harness.ProcessSpec{
		Argv:      []string{env.GoBinary, "help", "testflag"},
		Env:       env.EnvVars(),
		Timeout:   10 * time.Second,
		MaxStdout: 1 << 20,
		MaxStderr: 1 << 20,
	})
	if !result.Successful() {
		return false
	}
	return strings.Contains(string(result.Stdout), "-fuzzcache")
}

func (c *config) runCleanup() {
	for index := len(c.cleanup) - 1; index >= 0; index-- {
		c.cleanup[index]()
	}
}

func (c *config) uploadLog(ctx context.Context, label, output string, exitCode int) (domain.CASRef, error) {
	payload, err := json.Marshal(map[string]any{
		"label":     label,
		"exit_code": exitCode,
		"output":    output,
	})
	if err != nil {
		return domain.CASRef{}, err
	}
	return c.putBytes(ctx, payload)
}

// reproduce replays the saved corpus (including the seed corpus) without
// fuzzing. A crash is only reported when this replay fails the same way.
func (c *config) reproduce(ctx context.Context) (harness.ProcessResult, bool) {
	timeout := time.Duration(c.seconds)*time.Second + 60*time.Second
	if timeout > 3*time.Minute {
		timeout = 3 * time.Minute
	}
	result := harness.Run(ctx, harness.ProcessSpec{
		Dir:       c.headDir,
		Argv:      []string{c.env.GoBinary, "test", "-run=FuzzTarget", "-count=1", "./.patchproof"},
		Env:       c.env.EnvVars(),
		Timeout:   timeout,
		MaxStdout: 4 << 20,
		MaxStderr: 4 << 20,
	})
	if result.StartError != nil || result.TimedOut || result.Canceled || result.OutputOverflow {
		return result, false
	}
	if !result.Successful() {
		output := string(result.Stdout) + string(result.Stderr)
		crash, _ := classifyFuzzOutput(output)
		return result, crash
	}
	return result, false
}

func (c *config) finish(started time.Time, claimResult domain.ClaimResult, details map[string]string, artifacts []domain.CASRef, limits []string) engine.Result {
	elapsed := time.Since(started)
	evidence := c.buildEvidence(claimResult, elapsed.Milliseconds(), details, artifacts, limits)
	corpusSize := atoiOrZero(details["corpus_size"])
	crashInputs := atoiOrZero(details["crash_inputs"])
	digest := resultDigest(claimResult, corpusSize, crashInputs, artifacts)
	outputs := map[string]string{
		"corpus_size":   strconv.Itoa(corpusSize),
		"crash_inputs":  strconv.Itoa(crashInputs),
		"evidence":      string(evidence.ID),
		"result_digest": digest,
	}
	if len(artifacts) > 0 {
		outputs["fuzz_log_artifact"] = artifacts[0].URI()
	}
	return engine.Result{
		Outcome:   domain.OutcomeSucceeded,
		Evidence:  []domain.Evidence{evidence},
		Artifacts: artifacts,
		Metrics:   domain.StepMetrics{ExecutionMS: elapsed.Milliseconds()},
		Outputs:   outputs,
	}
}

func (c *config) unknownResult(started time.Time, reason, message string, extraLimits []string) engine.Result {
	limits := append([]string{reason + ": " + message}, extraLimits...)
	return c.finish(started, domain.ClaimUnknown, map[string]string{
		"corpus_size":  strconv.Itoa(int(c.seedCount)),
		"crash_inputs": "0",
		"reason":       reason,
	}, nil, limits)
}

func countFiles(root string, limit int) int {
	count := 0
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		count++
		if count >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return count
}

func atoiOrZero(text string) int {
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0
	}
	return value
}

func truncateForMessage(output string) string {
	if len(output) > 2048 {
		return output[:2048] + "...(truncated)"
	}
	return output
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
