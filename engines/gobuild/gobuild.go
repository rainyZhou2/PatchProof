package gobuild

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// Engine implements the go-build compile and regression-test engine.
type Engine struct{}

// New returns a ready-to-register go-build engine.
func New() *Engine { return &Engine{} }

// Name returns the registry name of the engine.
func (e *Engine) Name() string { return EngineName }

// Version returns the engine implementation version.
func (e *Engine) Version() string { return EngineVersion }

// Kinds returns the step kinds handled by the engine.
func (e *Engine) Kinds() []domain.StepKind {
	return []domain.StepKind{domain.StepKindCompile, domain.StepKindRegression}
}

// Languages returns the languages handled by the engine.
func (e *Engine) Languages() []string { return []string{"go"} }

// Execute runs a compile or regression-test step against a staged snapshot.
func (e *Engine) Execute(ctx context.Context, request engine.Request) (engine.Result, error) {
	started := time.Now()
	if request.Store == nil {
		return failedResult(domain.ErrValidationFailed, "artifact store is required"), nil
	}
	kind := request.Spec.Kind
	if kind != domain.StepKindCompile && kind != domain.StepKindRegression {
		return failedResult(domain.ErrEngineUnsupported, fmt.Sprintf("go-build does not handle step kind %q", kind)), nil
	}
	view, err := parseOptions(request.Spec.Options, kind)
	if err != nil {
		return failedResult(domain.ErrValidationFailed, err.Error()), nil
	}
	workspace, err := prepareBuildWorkspace(request)
	if err != nil {
		return failedResult(domain.ErrSandboxUnavailable, err.Error()), nil
	}
	snapshotName := "head"
	if kind == domain.StepKindCompile {
		snapshotName = string(view.target)
	}
	root, err := stagedSnapshot(request, snapshotName)
	if err != nil {
		return failedResult(domain.ErrValidationFailed, err.Error()), nil
	}
	view, err = applyImpactPatterns(request, view, kind)
	if err != nil {
		return failedResult(domain.ErrValidationFailed, err.Error()), nil
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return failedResult(domain.ErrSandboxUnavailable, "go toolchain is not available in PATH"), nil
	}
	absoluteBinary, err := filepath.Abs(goBinary)
	if err != nil {
		return failedResult(domain.ErrSandboxUnavailable, "go toolchain path could not be resolved"), nil
	}
	if isWithin(root, absoluteBinary) {
		return failedResult(domain.ErrSandboxUnavailable, "go toolchain resolved inside the untrusted snapshot"), nil
	}
	if isWithin(root, workspace.root) {
		return failedResult(domain.ErrValidationFailed, "build workspace must not be inside the staged snapshot"), nil
	}
	environment := buildEnvironment(workspace, view)
	workCtx, cancel := commandContext(ctx, request.Spec.Deadline, commandSafetyMargin)
	defer cancel()

	var result engine.Result
	if kind == domain.StepKindCompile {
		result = executeCompile(ctx, workCtx, request, view, root, absoluteBinary, environment, workspace)
	} else {
		result = executeRegression(ctx, workCtx, request, view, root, absoluteBinary, environment, workspace)
	}
	result.Metrics.ExecutionMS = time.Since(started).Milliseconds()
	var written int64
	for _, artifact := range result.Artifacts {
		written += artifact.SizeBytes
	}
	result.Metrics.CASWrittenBytes = written
	return result, nil
}
