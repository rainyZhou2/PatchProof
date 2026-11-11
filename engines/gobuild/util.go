package gobuild

import (
	"errors"
	"os/exec"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// failedResult builds a terminal engine result for an operational failure.
func failedResult(class domain.ErrorClass, message string) engine.Result {
	return failedResultRetryable(class, message, false)
}

func failedResultRetryable(class domain.ErrorClass, message string, retryable bool) engine.Result {
	return engine.Result{
		Outcome:    domain.OutcomeFailed,
		ErrorClass: class,
		Message:    message,
		Retryable:  retryable,
	}
}

// startFailureClass distinguishes a missing toolchain (permanent sandbox
// problem) from a transient process-start failure (retryable executor error).
func startFailureClass(err error) (domain.ErrorClass, bool) {
	if errors.Is(err, exec.ErrNotFound) {
		return domain.ErrSandboxUnavailable, false
	}
	return domain.ErrExecutorError, true
}

// joinPatterns renders patterns deterministically for evidence details.
func joinPatterns(patterns []string) string {
	return strings.Join(patterns, ",")
}

// boolString renders a boolean for evidence details.
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
