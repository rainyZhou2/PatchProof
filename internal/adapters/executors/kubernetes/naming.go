package kubernetes

import (
	"strconv"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	maxLabelValueLength = 63
	maxNamePrefixLength = 20
	shortIDLength       = 8
)

// sanitizeLabelValue makes raw safe for use as a Kubernetes label value. The
// result is lowercased, limited to 63 characters and starts/ends with an
// alphanumeric character. Empty input yields an empty label value, which
// Kubernetes accepts.
func sanitizeLabelValue(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	value := strings.Trim(b.String(), "-_.")
	if len(value) > maxLabelValueLength {
		value = value[:maxLabelValueLength]
	}
	return strings.Trim(value, "-_.")
}

// sanitizeName makes raw safe for use inside a DNS-1123 resource or volume
// name. Invalid runs collapse into a single dash.
func sanitizeName(raw string, maxLen int) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > maxLen {
		name = strings.Trim(name[:maxLen], "-")
	}
	if name == "" {
		return "step"
	}
	return name
}

// shortID returns the sanitized first eight characters of an identifier.
func shortID(raw string) string {
	if len(raw) > shortIDLength {
		raw = raw[:shortIDLength]
	}
	return sanitizeName(raw, shortIDLength)
}

// labels returns the identifying labels carried by the job and its pod.
func (e *Executor) labels(spec domain.StepSpec) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       workerAppName,
		"app.kubernetes.io/managed-by": "patchproof",
		"patchproof.io/run-id":         sanitizeLabelValue(string(spec.RunID)),
		"patchproof.io/step-id":        sanitizeLabelValue(string(spec.StepID)),
		"patchproof.io/attempt-id":     sanitizeLabelValue(string(spec.AttemptID)),
		"patchproof.io/generation":     strconv.FormatInt(spec.Generation, 10),
		"patchproof.io/tenant-id":      sanitizeLabelValue(string(spec.TenantID)),
	}
}

// resourceName derives the deterministic job name for an attempt.
func (e *Executor) resourceName(spec domain.StepSpec) string {
	prefix := sanitizeName(e.cfg.SecretNamePrefix, maxNamePrefixLength)
	return prefix + "-" + shortID(string(spec.StepID)) + "-" + shortID(string(spec.AttemptID))
}
