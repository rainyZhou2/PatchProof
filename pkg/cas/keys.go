package cas

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	digestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

func NormalizeDigest(digest string) (string, error) {
	normalized := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	if !digestPattern.MatchString(normalized) {
		return "", fmt.Errorf("digest must be a lowercase sha256 hex value")
	}
	return normalized, nil
}

func BlobKey(tenantID, trustDomain, digest string) (string, error) {
	normalized, err := NormalizeDigest(digest)
	if err != nil {
		return "", err
	}
	if !namespacePattern.MatchString(tenantID) {
		return "", fmt.Errorf("invalid tenant namespace %q", tenantID)
	}
	if !namespacePattern.MatchString(trustDomain) {
		return "", fmt.Errorf("invalid trust domain %q", trustDomain)
	}
	return fmt.Sprintf("cas/%s/%s/sha256/%s/%s/%s", tenantID, trustDomain, normalized[:2], normalized[2:4], normalized), nil
}

func ReportKey(tenantID, runID, name string) (string, error) {
	if !namespacePattern.MatchString(tenantID) || !namespacePattern.MatchString(runID) {
		return "", fmt.Errorf("invalid report namespace")
	}
	if !namespacePattern.MatchString(name) {
		return "", fmt.Errorf("invalid report object name")
	}
	return fmt.Sprintf("reports/%s/%s/%s", tenantID, runID, name), nil
}

func ReproducerKey(tenantID, runID, caseID string) (string, error) {
	if !namespacePattern.MatchString(tenantID) || !namespacePattern.MatchString(runID) || !namespacePattern.MatchString(caseID) {
		return "", fmt.Errorf("invalid reproducer namespace")
	}
	return fmt.Sprintf("reproducers/%s/%s/%s/bundle.tar.zst", tenantID, runID, caseID), nil
}

func DigestFromBlobKey(key string) (string, error) {
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("key %q is not a blob key", key)
	}
	digest := parts[len(parts)-1]
	if _, err := NormalizeDigest(digest); err != nil {
		return "", err
	}
	return digest, nil
}
