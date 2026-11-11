package gobuild

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TestCaseResult is the outcome of one test function.
type TestCaseResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
}

// TestPackageResult is the aggregated outcome of one package.
type TestPackageResult struct {
	Path        string           `json:"path"`
	Status      string           `json:"status"`
	DurationMS  int64            `json:"duration_ms"`
	BuildFailed bool             `json:"build_failed,omitempty"`
	Output      string           `json:"output,omitempty"`
	Tests       []TestCaseResult `json:"tests"`
}

// TestResultsBundle is the test-results.json artifact schema.
type TestResultsBundle struct {
	SchemaVersion string              `json:"schema_version"`
	Packages      []TestPackageResult `json:"packages"`
	Truncated     bool                `json:"truncated"`
	Limits        []string            `json:"limits"`
}

// testEvent is one go test -json event.
type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// testAgg accumulates test state while streaming events.
type testAgg struct {
	status    string
	elapsed   float64
	output    string
	truncated bool
}

// packageAgg accumulates package state while streaming events.
type packageAgg struct {
	path        string
	status      string
	elapsed     float64
	output      string
	truncated   bool
	buildFailed bool
	tests       map[string]*testAgg
}

// parseTestJSON aggregates newline-delimited go test -json events.
func parseTestJSON(data []byte, limits *[]string) (TestResultsBundle, bool) {
	bundle := TestResultsBundle{
		SchemaVersion: TestResultsSchemaVersion,
		Packages:      []TestPackageResult{},
		Limits:        []string{},
	}
	aggregates := make(map[string]*packageAgg)
	sawEvents := false
	invalidLines := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event testEvent
		if err := json.Unmarshal(line, &event); err != nil {
			invalidLines++
			continue
		}
		sawEvents = true
		if event.Package == "" {
			continue
		}
		aggregate, ok := aggregates[event.Package]
		if !ok {
			aggregate = &packageAgg{path: event.Package, tests: make(map[string]*testAgg)}
			aggregates[event.Package] = aggregate
		}
		if event.Test == "" {
			switch event.Action {
			case "output":
				aggregate.output = appendCapped(aggregate.output, event.Output, maxPackageFailureBytes, &aggregate.truncated)
				if buildFailureText(event.Output) {
					aggregate.buildFailed = true
				}
			case "pass", "fail", "skip":
				aggregate.status = mergeStatus(aggregate.status, event.Action)
				aggregate.elapsed = event.Elapsed
			}
			continue
		}
		test, ok := aggregate.tests[event.Test]
		if !ok {
			test = &testAgg{}
			aggregate.tests[event.Test] = test
		}
		switch event.Action {
		case "output":
			test.output = appendCapped(test.output, event.Output, maxTestFailureBytes, &test.truncated)
		case "run":
			if test.status == "" {
				test.status = "run"
			}
		case "pass", "fail", "skip":
			test.status = mergeStatus(test.status, event.Action)
			test.elapsed = event.Elapsed
			if event.Action == "fail" {
				aggregate.status = mergeStatus(aggregate.status, "fail")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		bundle.Truncated = true
		*limits = append(*limits, "go test -json output was truncated or unreadable; results may be incomplete")
	}
	if invalidLines > 0 {
		*limits = append(*limits, fmt.Sprintf("skipped %d non-JSON lines in go test output", invalidLines))
	}

	paths := make([]string, 0, len(aggregates))
	for path := range aggregates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	embeddedBytes := 0
	for _, path := range paths {
		aggregate := aggregates[path]
		status := aggregate.status
		if status == "" || status == "run" {
			status = derivePackageStatus(aggregate)
		}
		result := TestPackageResult{
			Path:        path,
			Status:      status,
			DurationMS:  int64(aggregate.elapsed * 1000),
			BuildFailed: aggregate.buildFailed || (status == "fail" && len(aggregate.tests) == 0 && aggregate.output != ""),
			Tests:       []TestCaseResult{},
		}
		testNames := make([]string, 0, len(aggregate.tests))
		for name := range aggregate.tests {
			testNames = append(testNames, name)
		}
		sort.Strings(testNames)
		var totalTestDuration float64
		for _, name := range testNames {
			test := aggregate.tests[name]
			testStatus := test.status
			if testStatus == "" || testStatus == "run" {
				testStatus = "unknown"
			}
			testResult := TestCaseResult{
				Name:       name,
				Status:     testStatus,
				DurationMS: int64(test.elapsed * 1000),
			}
			totalTestDuration += test.elapsed
			if testStatus == "fail" {
				if embeddedBytes < maxResultsTruncationBytes {
					output := truncateString(test.output, maxTestFailureBytes)
					testResult.Output = output
					embeddedBytes += len(output)
				} else {
					bundle.Truncated = true
				}
			}
			result.Tests = append(result.Tests, testResult)
		}
		if result.DurationMS == 0 && totalTestDuration > 0 {
			result.DurationMS = int64(totalTestDuration * 1000)
		}
		if result.Status == "fail" {
			if embeddedBytes < maxResultsTruncationBytes {
				output := truncateString(aggregate.output, maxPackageFailureBytes)
				result.Output = output
				embeddedBytes += len(output)
			} else {
				bundle.Truncated = true
			}
		}
		bundle.Packages = append(bundle.Packages, result)
		if result.BuildFailed {
			*limits = append(*limits, fmt.Sprintf("package %s failed to build during test execution", path))
		}
	}
	if embeddedBytes >= maxResultsTruncationBytes {
		*limits = append(*limits, fmt.Sprintf("embedded failure output capped at %d bytes", maxResultsTruncationBytes))
	}
	if aggregateTruncated(aggregates) {
		bundle.Truncated = true
		*limits = append(*limits, fmt.Sprintf("per-stream command output capped at %d bytes", maxLogBytes))
	}
	if len(bundle.Packages) == 0 {
		*limits = append(*limits, "no package events were observed in go test output")
	}
	bundle.Limits = normalizeLimits(*limits)
	return bundle, sawEvents
}

// aggregateTruncated reports whether any captured stream hit its cap.
func aggregateTruncated(aggregates map[string]*packageAgg) bool {
	for _, aggregate := range aggregates {
		if aggregate.truncated {
			return true
		}
		for _, test := range aggregate.tests {
			if test.truncated {
				return true
			}
		}
	}
	return false
}

// mergeStatus combines two statuses, with failure dominating.
func mergeStatus(current, next string) string {
	if current == "fail" || next == "fail" {
		return "fail"
	}
	if current == "" || current == "run" {
		return next
	}
	return current
}

// derivePackageStatus infers a package status when no package-level event
// carried one.
func derivePackageStatus(aggregate *packageAgg) string {
	status := ""
	for _, test := range aggregate.tests {
		switch test.status {
		case "fail":
			return "fail"
		case "pass":
			status = "pass"
		case "skip":
			if status == "" {
				status = "skip"
			}
		}
	}
	if status == "" {
		if len(aggregate.tests) > 0 {
			return "fail"
		}
		return "unknown"
	}
	return status
}

// appendCapped appends text up to limit bytes and records truncation.
func appendCapped(current, addition string, limit int, truncated *bool) string {
	if len(current) >= limit {
		if addition != "" {
			*truncated = true
		}
		return current
	}
	remaining := limit - len(current)
	if len(addition) > remaining {
		current += addition[:remaining]
		*truncated = true
		return current
	}
	return current + addition
}

// truncateString bounds a string to limit bytes.
func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

// buildFailureText reports whether an output line indicates a build failure.
func buildFailureText(line string) bool {
	lowered := strings.ToLower(line)
	return strings.Contains(lowered, "[build failed]") ||
		strings.Contains(lowered, "build failed") ||
		strings.Contains(lowered, "setup failed") ||
		strings.Contains(lowered, "no required module provides")
}

// bundleFailed reports whether any package in the bundle failed.
func bundleFailed(bundle TestResultsBundle) bool {
	for _, pkg := range bundle.Packages {
		if pkg.Status == "fail" {
			return true
		}
	}
	return false
}

// bundleBuildFailed reports whether the bundle contains a build failure.
func bundleBuildFailed(bundle TestResultsBundle) bool {
	for _, pkg := range bundle.Packages {
		if pkg.BuildFailed {
			return true
		}
	}
	return false
}
