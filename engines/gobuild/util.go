package gobuild

import (
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// failedResult builds a terminal engine result for an operational failure.
func failedResult(class domain.ErrorClass, message string) engine.Result {
	return engine.Result{
		Outcome:    domain.OutcomeFailed,
		ErrorClass: class,
		Message:    message,
		Retryable:  false,
	}
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
