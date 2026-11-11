// Package gobuild implements the go-build compile and regression-test engines.
//
// Commands are executed directly (never through a shell) inside the staged
// snapshot with a sanitized environment, bounded logs and an explicit deadline.
package gobuild

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

const (
	// EngineName is the stable registry name of the engine.
	EngineName = "go-build"
	// EngineVersion is the semantic version of the engine.
	EngineVersion = "0.1.0"

	// TestResultsSchemaVersion is the schema identifier of test-results.json.
	TestResultsSchemaVersion = "patchproof.test-results/v1"

	// maxLogBytes bounds each captured command stream.
	maxLogBytes = 4 << 20
	// maxPackageFailureBytes bounds failure output embedded per package.
	maxPackageFailureBytes = 64 << 10
	// maxTestFailureBytes bounds failure output embedded per test.
	maxTestFailureBytes = 8 << 10
	// maxResultsTruncationBytes bounds all embedded failure output.
	maxResultsTruncationBytes = 256 << 10
	// maxPatterns bounds the number of package patterns.
	maxPatterns = 64
	// defaultTestTimeoutSeconds is used when the step has no deadline.
	defaultTestTimeoutSeconds = 600
	// commandSafetyMargin is reserved for artifact upload after the deadline.
	commandSafetyMargin = 2 * time.Second
)

// target is the snapshot revision compiled by the engine.
type target string

const (
	targetBase target = "base"
	targetHead target = "head"
)

// optionsView is the validated set of step options used by the engine.
type optionsView struct {
	patterns []string
	testArgs []string
	proxy    string
	goFlags  string
	cgo      bool
	target   target
	claimID  string
}

// parseOptions validates all engine options. Errors are user-facing validation
// failures and must not be retried.
func parseOptions(options map[string]string, kind domain.StepKind) (optionsView, error) {
	view := optionsView{
		proxy:   "off",
		target:  targetHead,
		claimID: "compile",
	}
	if kind == domain.StepKindRegression {
		view.claimID = "regression"
	}
	patterns, err := parsePatterns(options, kind)
	if err != nil {
		return optionsView{}, err
	}
	view.patterns = patterns
	if raw := strings.TrimSpace(stepopts.Get(options, stepopts.Goproxy)); raw != "" {
		if !validProxy(raw) {
			return optionsView{}, fmt.Errorf("go.proxy contains unsupported characters")
		}
		view.proxy = raw
	}
	if raw := stepopts.Get(options, stepopts.GoFlags); raw != "" {
		if strings.ContainsAny(raw, "\x00\n\r") {
			return optionsView{}, fmt.Errorf("go.flags contains control characters")
		}
		if len(raw) > 4096 {
			return optionsView{}, fmt.Errorf("go.flags exceeds 4096 bytes")
		}
		for _, field := range strings.Fields(raw) {
			for _, banned := range []string{"-toolexec", "-exec", "-modfile", "-overlay"} {
				if field == banned || strings.HasPrefix(field, banned+"=") {
					return optionsView{}, fmt.Errorf("go.flags must not set %s", banned)
				}
			}
		}
		view.goFlags = strings.TrimSpace(raw)
	}
	view.cgo = stepopts.GetBool(options, "cgo", false)
	if raw := strings.ToLower(strings.TrimSpace(stepopts.Get(options, "target"))); raw != "" {
		switch raw {
		case string(targetBase), string(targetHead):
			view.target = target(raw)
		default:
			return optionsView{}, fmt.Errorf("target must be base or head")
		}
	}
	if claim := strings.TrimSpace(stepopts.Get(options, stepopts.ClaimID)); claim != "" {
		view.claimID = claim
	}
	if kind == domain.StepKindRegression {
		if view.target == targetBase {
			return optionsView{}, fmt.Errorf("regression tests run against head; target=base is not supported")
		}
		view.target = targetHead
		args, err := parseTestArgs(stepopts.Get(options, "test.args"))
		if err != nil {
			return optionsView{}, err
		}
		view.testArgs = args
	}
	return view, nil
}

// parsePatterns validates the package pattern list. Patterns are package
// import paths, never filesystem paths, which keeps the command surface free of
// shell or path-escape injections.
func parsePatterns(options map[string]string, kind domain.StepKind) ([]string, error) {
	raw := stepopts.SplitList(options, "go.patterns")
	if len(raw) == 0 && kind == domain.StepKindRegression {
		if single := strings.TrimSpace(stepopts.Get(options, stepopts.TestPattern)); single != "" {
			raw = []string{single}
		}
	}
	if len(raw) == 0 {
		return []string{"./..."}, nil
	}
	if len(raw) > maxPatterns {
		return nil, fmt.Errorf("go.patterns must contain at most %d entries", maxPatterns)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, pattern := range raw {
		if err := validatePattern(pattern); err != nil {
			return nil, err
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	return out, nil
}

// validatePattern rejects patterns that could escape the snapshot or be
// misinterpreted by the toolchain.
func validatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("go.patterns entries must not be empty")
	}
	if strings.HasPrefix(pattern, "-") {
		return fmt.Errorf("go.patterns entry %q must not start with '-'", pattern)
	}
	if filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("go.patterns entry %q must be relative", pattern)
	}
	for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
		if segment == ".." {
			return fmt.Errorf("go.patterns entry %q must not contain '..'", pattern)
		}
	}
	for _, r := range pattern {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("go.patterns entry %q must not contain whitespace", pattern)
		}
		switch r {
		case ';', '|', '&', '$', '`', '>', '<', '\\', '"', '\'':
			return fmt.Errorf("go.patterns entry %q contains a disallowed character", pattern)
		}
	}
	return nil
}

// validProxy validates a module proxy setting.
func validProxy(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return value == "off" || value == "direct" || strings.Contains(value, "://") || strings.Contains(value, ",")
}

// parseTestArgs validates the allowlisted extra go test arguments.
func parseTestArgs(raw string) ([]string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(fields))
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch field {
		case "-run":
			if index+1 >= len(fields) {
				return nil, fmt.Errorf("-run requires a pattern argument")
			}
			value := fields[index+1]
			if err := validateTestArgValue(value); err != nil {
				return nil, err
			}
			out = append(out, field, value)
			index++
		case "-short", "-v", "-failfast":
			out = append(out, field)
		case "-race":
			return nil, fmt.Errorf("-race is not allowed under the sandbox profile")
		case "-count", "-timeout", "-cover", "-coverprofile", "-cpuprofile", "-memprofile", "-blockprofile", "-mutexprofile", "-trace", "-exec", "-toolexec":
			return nil, fmt.Errorf("%s is managed by the engine and is not allowed", field)
		default:
			return nil, fmt.Errorf("test.args entry %q is not allowed", field)
		}
	}
	return out, nil
}

// validateTestArgValue rejects shell and control characters in allowlisted
// argument values. Regular-expression metacharacters remain valid.
func validateTestArgValue(value string) error {
	if value == "" {
		return fmt.Errorf("-run pattern must not be empty")
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("-run pattern must not start with '-'")
	}
	if len(value) > 1024 {
		return fmt.Errorf("-run pattern exceeds 1024 bytes")
	}
	for _, r := range value {
		if r == 0 || r == '\n' || r == '\r' || r == '\t' {
			return fmt.Errorf("-run pattern contains control characters")
		}
		switch r {
		case ';', '|', '&', '$', '`', '>', '<', '\\', '"', '\'':
			return fmt.Errorf("-run pattern contains a disallowed character")
		}
	}
	return nil
}

// testTimeoutSeconds derives the go test -timeout budget from the step
// deadline, leaving room for the harness to collect the final events.
func testTimeoutSeconds(deadline time.Time) int {
	if deadline.IsZero() {
		return defaultTestTimeoutSeconds
	}
	remaining := time.Until(deadline) - 3*time.Second
	if remaining < time.Second {
		return 1
	}
	seconds := int(remaining / time.Second)
	if seconds > 3600 {
		seconds = 3600
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
