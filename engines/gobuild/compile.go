package gobuild

import (
	"context"
	"errors"
	"fmt"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// compileAssumptions documents the compile evidence boundary.
var compileAssumptions = []string{
	"go build runs inside the staged snapshot with a sanitized environment and no shell",
	"module proxy access is disabled unless the step explicitly sets go.proxy",
	"CGO is disabled unless the step sets cgo=1",
	"Go workspace mode is disabled (GOWORK=off); go.work files are ignored",
}

// executeCompile runs go build and emits COMPILE evidence.
func executeCompile(
	ctx context.Context,
	workCtx context.Context,
	request engine.Request,
	view optionsView,
	root string,
	goBinary string,
	environment []string,
	workspace buildWorkspace,
) engine.Result {
	args := append([]string{"build"}, view.patterns...)
	outcome := runGoCommand(workCtx, goBinary, root, environment, args)
	header := fmt.Sprintf("target: %s\npatterns: %s", view.target, joinPatterns(view.patterns))
	content := composeLog(header, args, outcome)
	logName := "compile-" + string(view.target) + ".log"
	logRef, err := putLog(ctx, request, workspace, logName, content)
	if err != nil {
		return failedResult(domain.ErrCASUnavailable, err.Error())
	}
	artifacts := []domain.CASRef{logRef}
	limits := []string{}
	if outcome.stdoutTruncated || outcome.stderrTruncated {
		limits = append(limits, fmt.Sprintf("command output capped at %d bytes per stream", maxLogBytes))
	}
	details := map[string]string{
		"target":    string(view.target),
		"patterns":  joinPatterns(view.patterns),
		"exit_code": fmt.Sprintf("%d", outcome.exitCode),
	}
	extra := map[string]string{
		"target":  string(view.target),
		"cgo":     boolString(view.cgo),
		"goproxy": view.proxy,
	}
	claimID := view.claimID

	if outcome.startErr != nil {
		result := failedResult(domain.ErrSandboxUnavailable, fmt.Sprintf("go build could not start: %v", outcome.startErr))
		result.Artifacts = artifacts
		result.Outputs = map[string]string{"log.digest": logRef.Hex(), "target": string(view.target)}
		return result
	}
	if outcome.timedOut {
		return commandAbortedResult(ctx, workCtx, artifacts, logRef, "go build")
	}

	claimResult := domain.ClaimTested
	stepOutcome := domain.OutcomeSucceeded
	var errorClass domain.ErrorClass
	message := "go build succeeded"
	if outcome.exitCode != 0 {
		claimResult = domain.ClaimViolated
		stepOutcome = domain.OutcomeFailed
		errorClass = domain.ErrCompilationFailed
		message = fmt.Sprintf("go build failed with exit code %d", outcome.exitCode)
		if detail := firstNonEmptyLine(outcome.stderr); detail != "" {
			message += ": " + detail
		}
		details["failure"] = firstNonEmptyLine(outcome.stderr)
	}
	evidence, evidenceErr := buildEvidence(
		request, claimID, domain.EvidenceCompile, claimResult,
		"all packages matched by patterns", compileAssumptions,
		artifacts, limits, details, extra,
	)
	if evidenceErr != nil {
		result := failedResult(domain.ErrValidationFailed, evidenceErr.Error())
		result.Artifacts = artifacts
		return result
	}
	return engine.Result{
		Outcome:    stepOutcome,
		ErrorClass: errorClass,
		Message:    message,
		Retryable:  false,
		Evidence:   []domain.Evidence{evidence},
		Artifacts:  artifacts,
		Outputs: map[string]string{
			"log.digest": logRef.Hex(),
			"target":     string(view.target),
			"exit_code":  fmt.Sprintf("%d", outcome.exitCode),
		},
	}
}

// commandAbortedResult builds a timeout or cancellation result with logs.
func commandAbortedResult(ctx, workCtx context.Context, artifacts []domain.CASRef, logRef domain.CASRef, command string) engine.Result {
	outputs := map[string]string{"log.digest": logRef.Hex()}
	if errors.Is(workCtx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return engine.Result{
			Outcome:    domain.OutcomeTimedOut,
			ErrorClass: domain.ErrTimeout,
			Message:    command + " exceeded the step deadline",
			Retryable:  false,
			Artifacts:  artifacts,
			Outputs:    outputs,
		}
	}
	return engine.Result{
		Outcome:    domain.OutcomeCancelled,
		ErrorClass: domain.ErrCancelled,
		Message:    command + " was cancelled",
		Retryable:  false,
		Artifacts:  artifacts,
		Outputs:    outputs,
	}
}
