package gobuild

import (
	"context"
	"fmt"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// regressionAssumptions documents the regression-test evidence boundary.
var regressionAssumptions = []string{
	"go test runs inside the staged head snapshot with a sanitized environment and no shell",
	"the engine manages -count and -timeout; only -run, -short, -v and -failfast pass through",
	"module proxy access is disabled unless the step explicitly sets go.proxy",
	"Go workspace mode is disabled (GOWORK=off); go.work files are ignored",
}

// executeRegression runs go test -json and emits REGRESSION_TEST evidence.
func executeRegression(
	ctx context.Context,
	workCtx context.Context,
	request engine.Request,
	view optionsView,
	root string,
	goBinary string,
	environment []string,
	workspace buildWorkspace,
) engine.Result {
	timeoutSeconds := testTimeoutSeconds(request.Spec.Deadline)
	args := []string{"test", "-json", "-count=1", "-timeout", fmt.Sprintf("%ds", timeoutSeconds)}
	args = append(args, view.testArgs...)
	args = append(args, view.patterns...)
	outcome := runGoCommand(workCtx, goBinary, root, environment, args)

	limits := []string{}
	if outcome.stdoutTruncated || outcome.stderrTruncated {
		limits = append(limits, fmt.Sprintf("command output capped at %d bytes per stream", maxLogBytes))
	}
	bundle, sawEvents := parseTestJSON([]byte(outcome.stdout), &limits)
	if !sawEvents && outcome.exitCode != 0 {
		limits = append(limits, "go test produced no JSON events; the exit code determines the result")
	}

	header := fmt.Sprintf("target: head\npatterns: %s\ntimeout_seconds: %d", joinPatterns(view.patterns), timeoutSeconds)
	logContent := composeLog(header, args, outcome)
	logRef, err := putLog(ctx, request, workspace, "test.log", logContent)
	if err != nil {
		return failedResult(domain.ErrCASUnavailable, err.Error())
	}
	bundle.Limits = normalizeLimits(limits)
	resultsRef, err := engine.PutJSON(ctx, request.Store, request.Scope, bundle)
	if err != nil {
		result := failedResult(domain.ErrCASUnavailable, fmt.Sprintf("write test-results.json: %v", err))
		result.Artifacts = []domain.CASRef{logRef}
		return result
	}
	artifacts := []domain.CASRef{logRef, resultsRef}
	outputs := map[string]string{
		"log.digest":     logRef.Hex(),
		"results.digest": resultsRef.Hex(),
		"exit_code":      fmt.Sprintf("%d", outcome.exitCode),
	}

	if outcome.startErr != nil {
		result := failedResult(domain.ErrSandboxUnavailable, fmt.Sprintf("go test could not start: %v", outcome.startErr))
		result.Artifacts = artifacts
		result.Outputs = outputs
		return result
	}
	if outcome.timedOut {
		return commandAbortedResult(ctx, workCtx, artifacts, logRef, "go test")
	}

	failed := outcome.exitCode != 0 || bundleFailed(bundle)
	buildFailed := bundleBuildFailed(bundle)
	totalTests, failedTests, failedPackages := summarizeTests(bundle)
	details := map[string]string{
		"patterns":        joinPatterns(view.patterns),
		"test_args":       strings.Join(view.testArgs, " "),
		"timeout_seconds": fmt.Sprintf("%d", timeoutSeconds),
		"packages":        fmt.Sprintf("%d", len(bundle.Packages)),
		"failed_packages": fmt.Sprintf("%d", failedPackages),
		"tests":           fmt.Sprintf("%d", totalTests),
		"failed_tests":    fmt.Sprintf("%d", failedTests),
		"build_failed":    boolString(buildFailed),
	}
	extra := map[string]string{
		"target":  "head",
		"cgo":     boolString(view.cgo),
		"goproxy": view.proxy,
	}
	if view.impactScoped {
		extra["input_scope"] = "impact"
	} else {
		extra["input_scope"] = "patterns"
	}
	claimResult := domain.ClaimTested
	stepOutcome := domain.OutcomeSucceeded
	var errorClass domain.ErrorClass
	message := fmt.Sprintf("tests passed (%d packages, %d tests)", len(bundle.Packages), totalTests)
	if failed {
		claimResult = domain.ClaimViolated
		stepOutcome = domain.OutcomeFailed
		errorClass = domain.ErrTestAssertionFailed
		if buildFailed {
			errorClass = domain.ErrCompilationFailed
		}
		if buildFailed {
			message = fmt.Sprintf("go test build failed (%d packages)", failedPackages)
		} else {
			message = fmt.Sprintf("go test failed (%d failing packages, %d failing tests)", failedPackages, failedTests)
		}
		if detail := regressionFailureDetail(bundle, outcome); detail != "" {
			message += ": " + detail
			details["failure"] = detail
		}
		if outcome.exitCode != 0 && !bundleFailed(bundle) {
			details["exit_code_only"] = "true"
		}
	}
	evidence, evidenceErr := buildEvidence(
		request, view.claimID, domain.EvidenceRegression, claimResult,
		"packages matched by patterns", regressionAssumptions,
		artifacts, bundle.Limits, details, extra,
	)
	if evidenceErr != nil {
		result := failedResult(domain.ErrValidationFailed, evidenceErr.Error())
		result.Artifacts = artifacts
		result.Outputs = outputs
		return result
	}
	return engine.Result{
		Outcome:    stepOutcome,
		ErrorClass: errorClass,
		Message:    message,
		Retryable:  false,
		Evidence:   []domain.Evidence{evidence},
		Artifacts:  artifacts,
		Outputs:    outputs,
	}
}

// summarizeTests counts packages, tests and failures.
func summarizeTests(bundle TestResultsBundle) (total, failedTests, failedPackages int) {
	for _, pkg := range bundle.Packages {
		if pkg.Status == "fail" {
			failedPackages++
		}
		for _, test := range pkg.Tests {
			total++
			if test.Status == "fail" {
				failedTests++
			}
		}
	}
	return total, failedTests, failedPackages
}

// regressionFailureDetail returns a short human-readable failure summary.
func regressionFailureDetail(bundle TestResultsBundle, outcome commandOutcome) string {
	for _, pkg := range bundle.Packages {
		if pkg.Status != "fail" {
			continue
		}
		for _, test := range pkg.Tests {
			if test.Status == "fail" {
				if line := firstNonEmptyLine(test.Output); line != "" {
					return pkg.Path + ": " + test.Name + ": " + line
				}
				return pkg.Path + ": " + test.Name
			}
		}
		if pkg.BuildFailed {
			if line := firstNonEmptyLine(pkg.Output); line != "" {
				return pkg.Path + " build: " + line
			}
			return pkg.Path + " build failed"
		}
		return pkg.Path + " failed"
	}
	if outcome.exitCode != 0 {
		if line := firstNonEmptyLine(outcome.stderr); line != "" {
			return line
		}
		return firstNonEmptyLine(outcome.stdout)
	}
	return ""
}
