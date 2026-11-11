package gobuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// regressionAssumptions documents the regression-test evidence boundary.
var regressionAssumptions = []string{
	"go test runs inside the staged head snapshot and, when available, the staged base snapshot with a sanitized environment and no shell",
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
	baseRoot := ""
	if _, ok := request.Staged["base"]; ok {
		resolved, err := stagedSnapshot(request, "base")
		if err != nil {
			return failedResult(domain.ErrValidationFailed, "base snapshot: "+err.Error())
		}
		baseRoot = resolved
	}
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
		class, retryable := startFailureClass(outcome.startErr)
		result := failedResultRetryable(class, fmt.Sprintf("go test could not start: %v", outcome.startErr), retryable)
		result.Artifacts = artifacts
		result.Outputs = outputs
		return result
	}
	if outcome.timedOut {
		return commandAbortedResult(ctx, workCtx, artifacts, logRef, "go test")
	}

	baselineStatus := "not-staged"
	var baseBundle TestResultsBundle
	baseArtifacts := []domain.CASRef{}
	if baseRoot != "" {
		baseOutcome := runGoCommand(workCtx, goBinary, baseRoot, environment, args)
		baseLimits := []string{}
		if baseOutcome.stdoutTruncated || baseOutcome.stderrTruncated {
			baseLimits = append(baseLimits, fmt.Sprintf("base command output capped at %d bytes per stream", maxLogBytes))
		}
		baseBundle, _ = parseTestJSON([]byte(baseOutcome.stdout), &baseLimits)
		baseBundle.Limits = normalizeLimits(baseLimits)
		baseHeader := fmt.Sprintf("target: base\npatterns: %s\ntimeout_seconds: %d", joinPatterns(view.patterns), timeoutSeconds)
		baseLogRef, err := putLog(ctx, request, workspace, "test-base.log", composeLog(baseHeader, args, baseOutcome))
		if err != nil {
			return failedResult(domain.ErrCASUnavailable, err.Error())
		}
		baseResultsRef, err := engine.PutJSON(ctx, request.Store, request.Scope, baseBundle)
		if err != nil {
			return failedResult(domain.ErrCASUnavailable, fmt.Sprintf("write base test-results.json: %v", err))
		}
		baseArtifacts = []domain.CASRef{baseLogRef, baseResultsRef}
		limits = append(limits, baseLimits...)
		switch {
		case baseOutcome.startErr != nil:
			baselineStatus = "infra-unavailable"
		case baseOutcome.timedOut:
			baselineStatus = "timed-out"
		case baseOutcome.exitCode != 0 || bundleFailed(baseBundle):
			baselineStatus = "failed"
		default:
			baseTotal, _, baseSkipped, _ := summarizeTests(baseBundle)
			if baseTotal-baseSkipped == 0 {
				baselineStatus = "empty"
			} else {
				baselineStatus = "passed"
			}
		}
	}
	artifacts = append(artifacts, baseArtifacts...)

	failed := outcome.exitCode != 0 || bundleFailed(bundle)
	buildFailed := bundleBuildFailed(bundle)
	totalTests, failedTests, skippedTests, failedPackages := summarizeTests(bundle)
	details := map[string]string{
		"patterns":        joinPatterns(view.patterns),
		"test_args":       strings.Join(view.testArgs, " "),
		"timeout_seconds": fmt.Sprintf("%d", timeoutSeconds),
		"packages":        fmt.Sprintf("%d", len(bundle.Packages)),
		"failed_packages": fmt.Sprintf("%d", failedPackages),
		"tests":           fmt.Sprintf("%d", totalTests),
		"skipped_tests":   fmt.Sprintf("%d", skippedTests),
		"failed_tests":    fmt.Sprintf("%d", failedTests),
		"build_failed":    boolString(buildFailed),
		"baseline":        baselineStatus,
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
	executedTests := totalTests - skippedTests
	if !failed && executedTests == 0 {
		// A clean `go test` exit with no executed test is not evidence that the
		// patch preserves behavior. Without this guard, deleting tests, targeting
		// a package with no tests, or skipping everything would produce
		// TESTED_WITHIN_SCOPE and ultimately VERIFIED_WITHIN_SCOPE.
		claimResult = domain.ClaimUnknown
		message = fmt.Sprintf("no tests were executed (%d packages, %d skipped)", len(bundle.Packages), skippedTests)
		limits = append(limits, "no tests were executed; an empty or fully skipped test suite cannot support a regression claim")
	}
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
	if baselineStatus != "not-staged" && baselineStatus != "passed" {
		// A base suite that already fails or cannot run means the head result
		// cannot be attributed to the patch. Report UNKNOWN rather than a false
		// counterexample (base failure) or a false pass (head failure hidden).
		claimResult = domain.ClaimUnknown
		stepOutcome = domain.OutcomeSucceeded
		errorClass = ""
		message = "base regression baseline " + baselineStatus + "; head result cannot be attributed to the patch"
		limits = append(limits, "base regression baseline is not a clean pass: "+baselineStatus)
	}
	if baselineStatus == "passed" && !failed {
		if changed, changeErr := changedTestFiles(baseRoot, root); changeErr != nil {
			limits = append(limits, "could not compare test file content: "+changeErr.Error())
		} else if len(changed) > 0 {
			claimResult = domain.ClaimUnknown
			message = fmt.Sprintf("%d base test file(s) changed content", len(changed))
			details["changed_test_files"] = fmt.Sprintf("%d", len(changed))
			limits = append(limits, "test file content changed relative to base; a clean exit cannot prove assertions were not weakened")
		}
		removed := missingTests(baseBundle, bundle)
		if len(removed) > 0 {
			// A patch that removes tests can still produce a clean `go test`
			// exit. Treat a shrunken test inventory as inconclusive rather than
			// positive regression evidence.
			claimResult = domain.ClaimUnknown
			message = fmt.Sprintf("head test inventory is missing %d base test(s)", len(removed))
			details["removed_tests"] = fmt.Sprintf("%d", len(removed))
			limits = append(limits, "head test inventory shrank relative to base; a clean exit cannot support the regression claim")
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

// changedTestFiles hashes every base _test.go file and compares it with the
// same path in the head snapshot. Adding tests is allowed; modifying an
// existing test file is treated as inconclusive because the engine cannot
// prove that its assertions were not weakened.
func changedTestFiles(baseRoot, headRoot string) ([]string, error) {
	if baseRoot == "" || headRoot == "" {
		return nil, nil
	}
	changed := make([]string, 0)
	err := filepath.WalkDir(baseRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(baseRoot, path)
		if err != nil {
			return err
		}
		baseDigest, err := fileDigest(path)
		if err != nil {
			return err
		}
		headPath := filepath.Join(headRoot, relative)
		headDigest, err := fileDigest(headPath)
		if err != nil {
			if os.IsNotExist(err) {
				changed = append(changed, relative)
				return nil
			}
			return err
		}
		if baseDigest != headDigest {
			changed = append(changed, relative)
		}
		return nil
	})
	sort.Strings(changed)
	return changed, err
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// missingTests returns base test identities that are absent from the head
// bundle. Counts alone are insufficient because a patch can delete and add the
// same number of tests.
func missingTests(base, head TestResultsBundle) []string {
	headTests := make(map[string]struct{})
	for _, pkg := range head.Packages {
		for _, test := range pkg.Tests {
			headTests[pkg.Path+"\x00"+test.Name] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, pkg := range base.Packages {
		for _, test := range pkg.Tests {
			key := pkg.Path + "\x00" + test.Name
			if _, ok := headTests[key]; !ok {
				missing = append(missing, pkg.Path+"::"+test.Name)
			}
		}
	}
	return missing
}

// summarizeTests counts packages, tests and failures.
func summarizeTests(bundle TestResultsBundle) (total, failedTests, skippedTests, failedPackages int) {
	for _, pkg := range bundle.Packages {
		if pkg.Status == "fail" {
			failedPackages++
		}
		for _, test := range pkg.Tests {
			total++
			switch test.Status {
			case "fail":
				failedTests++
			case "skip":
				skippedTests++
			}
		}
	}
	return total, failedTests, skippedTests, failedPackages
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
