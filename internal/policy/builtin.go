package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type HardPolicy struct {
	MinIsolation            domain.IsolationTier
	RequireNoSubjectNetwork bool
	RequiredChecks          []string
	AllowedLanguages        []string
	AllowedRegions          []string
	MaxRepositoryBytes      int64
	MaxChangedFiles         int
	MaxChangedFileBytes     int64
	ProtectedPaths          []string
	RequireTrustedPolicy    bool
	DisallowSubmodules      bool
	EnforceTenantFloor      bool
}

type Engine struct {
	hard      HardPolicy
	protected *PathMatcher
}

func NewEngine(hard HardPolicy) (*Engine, error) {
	if hard.MinIsolation == "" {
		hard.MinIsolation = domain.IsolationContainer
	}
	if !hard.MinIsolation.Valid() {
		return nil, fmt.Errorf("hard policy: invalid minimum isolation %q", hard.MinIsolation)
	}
	matcher, err := NewPathMatcher(hard.ProtectedPaths)
	if err != nil {
		return nil, fmt.Errorf("hard policy: %w", err)
	}
	return &Engine{hard: hard, protected: matcher}, nil
}

func (e *Engine) Evaluate(ctx context.Context, input ports.PolicyInput) (ports.PolicyDecision, error) {
	if err := ctx.Err(); err != nil {
		return ports.PolicyDecision{}, err
	}
	if input.Tenant.ID != input.Project.TenantID {
		return ports.PolicyDecision{}, &domain.ValidationError{Field: "project", Reason: "does not belong to the tenant"}
	}
	if err := input.Policy.Validate(); err != nil {
		return ports.PolicyDecision{}, err
	}
	decision := ports.PolicyDecision{
		Allowed:           true,
		Digest:            input.PolicyDigest,
		AllowInconclusive: append([]string(nil), input.Policy.Spec.Verification.AllowInconclusiveOn...),
		SubjectNetwork:    input.Policy.Spec.Execution.SubjectNetwork,
		ArtifactTransport: input.Policy.Spec.Execution.ArtifactTransport,
	}
	if decision.SubjectNetwork == "" {
		decision.SubjectNetwork = domain.NetworkNone
	}
	if decision.ArtifactTransport == "" {
		decision.ArtifactTransport = domain.TransportDirect
	}

	required := newStringSet()
	for _, check := range e.hard.RequiredChecks {
		required.add(domain.NormalizeCheckName(check))
	}
	for _, check := range input.Policy.RequiredChecks() {
		required.add(check)
	}
	optional := newStringSet()
	for _, check := range input.Policy.Spec.Verification.Optional {
		optional.add(domain.NormalizeCheckName(check))
	}
	for _, check := range input.RequestedChecks {
		normalized := domain.NormalizeCheckName(check)
		if _, known := domain.CheckStepKind(normalized); !known {
			decision.Violations = append(decision.Violations, ports.PolicyViolation{
				Code: "UNKNOWN_CHECK", Severity: domain.SeverityHigh, Message: "requested check is not supported: " + normalized,
			})
			continue
		}
		required.add(normalized)
		optional.remove(normalized)
	}
	decision.RequiredChecks = required.sorted()
	decision.OptionalChecks = optional.sorted()
	for _, check := range decision.AllowInconclusive {
		if required.has(domain.NormalizeCheckName(check)) {
			decision.Violations = append(decision.Violations, ports.PolicyViolation{
				Code: "INCONCLUSIVE_ON_REQUIRED", Severity: domain.SeverityHigh,
				Message: "policy allows an inconclusive verdict for required check " + check,
			})
		}
	}

	isolation := input.Policy.Spec.Execution.IsolationTier
	if input.Isolation.AtLeast(isolation) {
		isolation = input.Isolation
	}
	if e.hard.EnforceTenantFloor {
		if tenantFloor := isolationFloor(input.Tenant.SecurityTier); tenantFloor.Rank() > isolation.Rank() {
			isolation = tenantFloor
			decision.Reasons = append(decision.Reasons, "isolation raised to tenant security floor "+string(tenantFloor))
		}
	}
	if e.hard.MinIsolation.Rank() > isolation.Rank() {
		isolation = e.hard.MinIsolation
		decision.Reasons = append(decision.Reasons, "isolation raised to platform minimum "+string(e.hard.MinIsolation))
	}
	decision.Isolation = isolation
	if e.hard.RequireNoSubjectNetwork && decision.SubjectNetwork != domain.NetworkNone {
		decision.SubjectNetwork = domain.NetworkNone
		decision.Reasons = append(decision.Reasons, "subject network forced to none by platform policy")
	}
	if decision.Isolation.Rank() >= domain.IsolationGVisor.Rank() && decision.SubjectNetwork != domain.NetworkNone {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "NETWORK_POLICY_WEAKER_THAN_ISOLATION", Severity: domain.SeverityHigh,
			Message: "untrusted isolation tiers require subjectNetwork=none",
		})
	}

	if e.hard.RequireTrustedPolicy && !input.PolicyTrusted {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "UNTRUSTED_POLICY", Severity: domain.SeverityCritical, Message: "verification policy is not from a trusted source",
		})
	}
	if input.Tenant.Status != domain.TenantActive {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "TENANT_NOT_ACTIVE", Severity: domain.SeverityHigh, Message: "tenant status is " + string(input.Tenant.Status),
		})
	}

	maxFiles := input.Policy.Spec.Sources.MaxChangedFiles
	if e.hard.MaxChangedFiles > 0 && (maxFiles == 0 || e.hard.MaxChangedFiles < maxFiles) {
		maxFiles = e.hard.MaxChangedFiles
	}
	if maxFiles > 0 && input.ChangedFiles > maxFiles {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "SOURCE_LIMITS_EXCEEDED", Severity: domain.SeverityHigh,
			Message: fmt.Sprintf("changed files %d exceed limit %d", input.ChangedFiles, maxFiles),
		})
	}
	maxRepoGiB := input.Policy.Spec.Sources.MaxRepositorySizeGiB
	hardRepoGiB := float64(e.hard.MaxRepositoryBytes) / (1 << 30)
	if hardRepoGiB > 0 && (maxRepoGiB == 0 || hardRepoGiB < maxRepoGiB) {
		maxRepoGiB = hardRepoGiB
	}
	if maxRepoGiB > 0 && input.RepositorySizeGiB > maxRepoGiB {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "REPOSITORY_TOO_LARGE", Severity: domain.SeverityHigh,
			Message: fmt.Sprintf("repository size %.2f GiB exceeds limit %.2f GiB", input.RepositorySizeGiB, maxRepoGiB),
		})
	}
	if input.Submodules && (input.Policy.Spec.Sources.DisallowSubmodulesUnlessApproved || e.hard.DisallowSubmodules) {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "SUBMODULES_NOT_ALLOWED", Severity: domain.SeverityHigh, Message: "repository declares submodules without approval",
		})
	}

	policyMatcher, err := NewPathMatcher(input.Policy.Spec.ProtectedPaths)
	if err != nil {
		return ports.PolicyDecision{}, err
	}
	if changed := policyMatcher.Matches(input.ChangedPaths); len(changed) > 0 {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "PROTECTED_PATH_MODIFIED", Severity: domain.SeverityHigh,
			Message: "patch modifies protected paths: " + strings.Join(changed, ", "),
		})
	}
	if changed := e.protected.Matches(input.ChangedPaths); len(changed) > 0 {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "PROTECTED_PATH_MODIFIED", Severity: domain.SeverityCritical,
			Message: "patch modifies platform-protected paths: " + strings.Join(changed, ", "),
		})
	}

	allowedRegions := intersectRegions(input.Policy.Spec.Execution.AllowedRegions, input.Project.AllowedRegions)
	if len(e.hard.AllowedRegions) > 0 {
		allowedRegions = intersectRegions(allowedRegions, e.hard.AllowedRegions)
	}
	if len(allowedRegions) == 0 {
		allowedRegions = append(allowedRegions, input.Tenant.HomeRegion)
	}
	decision.AllowedRegions = allowedRegions
	if input.Region != "" && !containsFold(allowedRegions, input.Region) {
		decision.Violations = append(decision.Violations, ports.PolicyViolation{
			Code: "REGION_NOT_ALLOWED", Severity: domain.SeverityHigh, Message: "region " + input.Region + " is not allowed for this tenant",
		})
	}

	decision.Allowed = !hasBlockingViolation(decision.Violations)
	sort.Slice(decision.Violations, func(i, j int) bool { return decision.Violations[i].Code < decision.Violations[j].Code })
	return decision, nil
}

func isolationFloor(tier domain.SecurityTier) domain.IsolationTier {
	switch tier {
	case domain.SecurityTierRegulated:
		return domain.IsolationVM
	case domain.SecurityTierElevated:
		return domain.IsolationGVisor
	default:
		return domain.IsolationContainer
	}
}

func hasBlockingViolation(violations []ports.PolicyViolation) bool {
	for _, violation := range violations {
		if violation.Severity == domain.SeverityHigh || violation.Severity == domain.SeverityCritical {
			return true
		}
	}
	return false
}

type stringSet map[string]struct{}

func newStringSet() stringSet { return make(stringSet) }
func (s stringSet) add(value string) {
	if value != "" {
		s[value] = struct{}{}
	}
}
func (s stringSet) remove(value string) { delete(s, value) }
func (s stringSet) has(value string) bool {
	_, ok := s[value]
	return ok
}
func (s stringSet) sorted() []string {
	out := make([]string, 0, len(s))
	for value := range s {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func intersectRegions(lists ...[]string) []string {
	filtered := make([][]string, 0, len(lists))
	for _, list := range lists {
		if len(list) > 0 {
			filtered = append(filtered, list)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	lists = filtered
	seen := map[string]int{}
	for _, list := range lists {
		unique := map[string]struct{}{}
		for _, region := range list {
			region = strings.TrimSpace(region)
			if region == "" {
				continue
			}
			if _, ok := unique[region]; ok {
				continue
			}
			unique[region] = struct{}{}
			seen[region]++
		}
	}
	var out []string
	for region, count := range seen {
		if count == len(lists) {
			out = append(out, region)
		}
	}
	sort.Strings(out)
	return out
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
