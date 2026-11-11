package policy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	policyTenantID  = domain.TenantID("11111111-1111-4111-8111-111111111111")
	policyProjectID = domain.ProjectID("22222222-2222-4222-8222-222222222222")
	policyDigest    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func testTenant() domain.Tenant {
	return domain.Tenant{
		ID:           policyTenantID,
		HomeRegion:   "us-east-1",
		SecurityTier: domain.SecurityTierStandard,
		Status:       domain.TenantActive,
	}
}

func testProject() domain.Project {
	return domain.Project{
		ID:            policyProjectID,
		TenantID:      policyTenantID,
		Provider:      "git",
		RepositoryURI: "https://example.com/repo.git",
		DefaultBranch: "main",
	}
}

func testPolicy() domain.PolicyDocument {
	return domain.PolicyDocument{
		APIVersion: domain.PolicyAPIVersion,
		Kind:       "VerificationPolicy",
		Metadata:   domain.PolicyMetadata{Name: "test-policy"},
		Spec: domain.PolicySpec{
			Verification: domain.VerificationSpec{Required: []string{"compile"}},
			Execution: domain.ExecutionSpec{
				IsolationTier:      domain.IsolationContainer,
				AllowedRegions:     []string{"us-east-1"},
				DefaultCPUMilli:    1000,
				DefaultMemoryMiB:   2048,
				DefaultDiskGiB:     10,
				MaxStepWallSeconds: 600,
				MaxRunWallSeconds:  1200,
				SubjectNetwork:     domain.NetworkNone,
				ArtifactTransport:  domain.TransportBrokeredOffline,
				WorkerImage:        "ghcr.io/patchproof/worker:test",
			},
		},
	}
}

func testInput(policy domain.PolicyDocument) ports.PolicyInput {
	return ports.PolicyInput{
		Tenant:        testTenant(),
		Project:       testProject(),
		Policy:        policy,
		PolicyDigest:  policyDigest,
		PolicyTrusted: true,
		Isolation:     domain.IsolationTrustedLocal,
	}
}

func newEngine(t *testing.T, hard HardPolicy) *Engine {
	t.Helper()
	engine, err := NewEngine(hard)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func evaluate(t *testing.T, engine *Engine, input ports.PolicyInput) ports.PolicyDecision {
	t.Helper()
	decision, err := engine.Evaluate(context.Background(), input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return decision
}

func hasViolation(decision ports.PolicyDecision, code string) bool {
	for _, violation := range decision.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}

func violationByCode(t *testing.T, decision ports.PolicyDecision, code string) ports.PolicyViolation {
	t.Helper()
	for _, violation := range decision.Violations {
		if violation.Code == code {
			return violation
		}
	}
	t.Fatalf("violation %s not found in %+v", code, decision.Violations)
	return ports.PolicyViolation{}
}

func TestNewEngineDefaultsAndValidation(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	if engine.hard.MinIsolation != domain.IsolationContainer {
		t.Fatalf("default minimum isolation = %s", engine.hard.MinIsolation)
	}

	if _, err := NewEngine(HardPolicy{MinIsolation: domain.IsolationTier("bogus")}); err == nil {
		t.Fatal("invalid minimum isolation accepted")
	}
	if _, err := NewEngine(HardPolicy{ProtectedPaths: []string{"../escape"}}); err == nil {
		t.Fatal("invalid protected pattern accepted")
	}

	// The default platform floor raises a trusted-local policy and request.
	policy := testPolicy()
	policy.Spec.Execution.IsolationTier = domain.IsolationTrustedLocal
	input := testInput(policy)
	decision := evaluate(t, engine, input)
	if decision.Isolation != domain.IsolationContainer {
		t.Fatalf("isolation = %s, want isolated-container default", decision.Isolation)
	}
}

func TestEvaluateRequiredChecksUnion(t *testing.T) {
	engine := newEngine(t, HardPolicy{RequiredChecks: []string{"security-scan", "COMPILE"}})
	policy := testPolicy()
	policy.Spec.Verification.Required = []string{"compile", "regression-test"}
	policy.Spec.Verification.Optional = []string{"bounded-smt"}
	input := testInput(policy)
	input.RequestedChecks = []string{"differential-fuzz"}

	decision := evaluate(t, engine, input)
	wantRequired := []string{"compile", "differential-fuzz", "regression-test", "security-scan"}
	if !reflect.DeepEqual(decision.RequiredChecks, wantRequired) {
		t.Fatalf("required checks = %v, want %v", decision.RequiredChecks, wantRequired)
	}
	if !reflect.DeepEqual(decision.OptionalChecks, []string{"bounded-smt"}) {
		t.Fatalf("optional checks = %v", decision.OptionalChecks)
	}
	if !decision.Allowed {
		t.Fatalf("decision not allowed: %+v", decision.Violations)
	}
	if decision.Digest != policyDigest {
		t.Fatalf("digest = %q", decision.Digest)
	}
}

func TestEvaluateUnknownRequestedCheck(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	input := testInput(testPolicy())
	input.RequestedChecks = []string{"mystery", "  COMPILE "}

	decision := evaluate(t, engine, input)
	violation := violationByCode(t, decision, "UNKNOWN_CHECK")
	if violation.Severity != domain.SeverityHigh {
		t.Fatalf("unknown check severity = %s", violation.Severity)
	}
	if !strings.Contains(violation.Message, "mystery") {
		t.Fatalf("message = %q", violation.Message)
	}
	if decision.Allowed {
		t.Fatal("unknown requested check must block the decision")
	}
	for _, check := range decision.RequiredChecks {
		if check == "mystery" {
			t.Fatal("unknown check was added to required checks")
		}
	}
}

func TestEvaluateRequestedCheckOverridesOptional(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	policy := testPolicy()
	policy.Spec.Verification.Optional = []string{"bounded-smt"}
	input := testInput(policy)
	input.RequestedChecks = []string{"bounded-smt"}

	decision := evaluate(t, engine, input)
	if !reflect.DeepEqual(decision.RequiredChecks, []string{"bounded-smt", "compile"}) {
		t.Fatalf("required checks = %v", decision.RequiredChecks)
	}
	if len(decision.OptionalChecks) != 0 {
		t.Fatalf("optional checks = %v, want empty", decision.OptionalChecks)
	}
	if !decision.Allowed {
		t.Fatalf("decision not allowed: %+v", decision.Violations)
	}
}

func TestEvaluateInconclusiveOnRequired(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	policy := testPolicy()
	policy.Spec.Verification.Optional = []string{"bounded-smt"}
	policy.Spec.Verification.AllowInconclusiveOn = []string{"bounded-smt"}

	input := testInput(policy)
	decision := evaluate(t, engine, input)
	if !reflect.DeepEqual(decision.AllowInconclusive, []string{"bounded-smt"}) {
		t.Fatalf("allow inconclusive = %v", decision.AllowInconclusive)
	}
	if !decision.Allowed {
		t.Fatalf("optional inconclusive allowance should be allowed: %+v", decision.Violations)
	}

	input.RequestedChecks = []string{"bounded-smt"}
	decision = evaluate(t, engine, input)
	violation := violationByCode(t, decision, "INCONCLUSIVE_ON_REQUIRED")
	if violation.Severity != domain.SeverityHigh {
		t.Fatalf("severity = %s", violation.Severity)
	}
	if decision.Allowed {
		t.Fatal("inconclusive allowance for a required check must block")
	}
}

func TestEvaluateIsolationRaisedNotLowered(t *testing.T) {
	tests := []struct {
		name       string
		hard       HardPolicy
		tenant     func(*domain.Tenant)
		policyTier domain.IsolationTier
		inputTier  domain.IsolationTier
		want       domain.IsolationTier
	}{
		{
			name: "request raises above policy", policyTier: domain.IsolationContainer,
			inputTier: domain.IsolationGVisor, want: domain.IsolationGVisor,
		},
		{
			name: "request below policy does not lower", policyTier: domain.IsolationGVisor,
			inputTier: domain.IsolationContainer, want: domain.IsolationGVisor,
		},
		{
			name: "tenant floor raises", hard: HardPolicy{EnforceTenantFloor: true},
			tenant:     func(tenant *domain.Tenant) { tenant.SecurityTier = domain.SecurityTierRegulated },
			policyTier: domain.IsolationContainer, inputTier: domain.IsolationTrustedLocal,
			want: domain.IsolationVM,
		},
		{
			name: "tenant elevated floor raises to gvisor", hard: HardPolicy{EnforceTenantFloor: true},
			tenant:     func(tenant *domain.Tenant) { tenant.SecurityTier = domain.SecurityTierElevated },
			policyTier: domain.IsolationContainer, inputTier: domain.IsolationTrustedLocal,
			want: domain.IsolationGVisor,
		},
		{
			name: "platform minimum raises", hard: HardPolicy{MinIsolation: domain.IsolationGVisor},
			policyTier: domain.IsolationContainer, inputTier: domain.IsolationTrustedLocal,
			want: domain.IsolationGVisor,
		},
		{
			name: "hard minimum below policy does not lower", hard: HardPolicy{MinIsolation: domain.IsolationTrustedLocal},
			policyTier: domain.IsolationVM, inputTier: domain.IsolationTrustedLocal,
			want: domain.IsolationVM,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := newEngine(t, tc.hard)
			policy := testPolicy()
			policy.Spec.Execution.IsolationTier = tc.policyTier
			input := testInput(policy)
			input.Isolation = tc.inputTier
			if tc.tenant != nil {
				tc.tenant(&input.Tenant)
			}
			decision := evaluate(t, engine, input)
			if decision.Isolation != tc.want {
				t.Fatalf("isolation = %s, want %s (reasons: %v)", decision.Isolation, tc.want, decision.Reasons)
			}
		})
	}
}

func TestEvaluateTenantFloorReason(t *testing.T) {
	engine := newEngine(t, HardPolicy{EnforceTenantFloor: true})
	policy := testPolicy()
	input := testInput(policy)
	input.Tenant.SecurityTier = domain.SecurityTierRegulated
	decision := evaluate(t, engine, input)
	if decision.Isolation != domain.IsolationVM {
		t.Fatalf("isolation = %s", decision.Isolation)
	}
	joined := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(joined, "tenant security floor") {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

func TestEvaluateSubjectNetworkPolicy(t *testing.T) {
	t.Run("forced to none by platform policy", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{RequireNoSubjectNetwork: true})
		policy := testPolicy()
		policy.Spec.Execution.SubjectNetwork = domain.NetworkRestricted
		policy.Spec.Execution.ArtifactTransport = domain.TransportDirect
		decision := evaluate(t, engine, testInput(policy))
		if decision.SubjectNetwork != domain.NetworkNone {
			t.Fatalf("subject network = %s, want none", decision.SubjectNetwork)
		}
		joined := strings.Join(decision.Reasons, "; ")
		if !strings.Contains(joined, "forced to none") {
			t.Fatalf("reasons = %v", decision.Reasons)
		}
		if !decision.Allowed {
			t.Fatalf("decision should be allowed: %+v", decision.Violations)
		}
	})

	t.Run("untrusted isolation cannot keep restricted network", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Execution.IsolationTier = domain.IsolationGVisor
		policy.Spec.Execution.SubjectNetwork = domain.NetworkRestricted
		policy.Spec.Execution.ArtifactTransport = domain.TransportDirect
		decision := evaluate(t, engine, testInput(policy))
		violation := violationByCode(t, decision, "NETWORK_POLICY_WEAKER_THAN_ISOLATION")
		if violation.Severity != domain.SeverityHigh {
			t.Fatalf("severity = %s", violation.Severity)
		}
		if decision.Allowed {
			t.Fatal("restricted network with gvisor must block")
		}
	})

	t.Run("untrusted isolation allowed with no network", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{RequireNoSubjectNetwork: true})
		policy := testPolicy()
		policy.Spec.Execution.IsolationTier = domain.IsolationGVisor
		policy.Spec.Execution.SubjectNetwork = domain.NetworkRestricted
		policy.Spec.Execution.ArtifactTransport = domain.TransportDirect
		decision := evaluate(t, engine, testInput(policy))
		if !decision.Allowed {
			t.Fatalf("decision should be allowed after forcing network none: %+v", decision.Violations)
		}
	})
}

func TestEvaluateUntrustedPolicy(t *testing.T) {
	engine := newEngine(t, HardPolicy{RequireTrustedPolicy: true})
	input := testInput(testPolicy())
	input.PolicyTrusted = false
	decision := evaluate(t, engine, input)
	violation := violationByCode(t, decision, "UNTRUSTED_POLICY")
	if violation.Severity != domain.SeverityCritical {
		t.Fatalf("severity = %s", violation.Severity)
	}
	if decision.Allowed {
		t.Fatal("untrusted policy must block")
	}

	input.PolicyTrusted = true
	decision = evaluate(t, engine, input)
	if !decision.Allowed || len(decision.Violations) != 0 {
		t.Fatalf("trusted policy decision = %+v", decision)
	}
}

func TestEvaluateTenantStatus(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	for _, status := range []domain.TenantStatus{
		domain.TenantMigrating, domain.TenantSuspended, domain.TenantDeleting,
	} {
		t.Run(string(status), func(t *testing.T) {
			input := testInput(testPolicy())
			input.Tenant.Status = status
			decision := evaluate(t, engine, input)
			violation := violationByCode(t, decision, "TENANT_NOT_ACTIVE")
			if violation.Severity != domain.SeverityHigh {
				t.Fatalf("severity = %s", violation.Severity)
			}
			if decision.Allowed {
				t.Fatal("non-active tenant must block")
			}
		})
	}
}

func TestEvaluateSourceLimits(t *testing.T) {
	t.Run("policy changed file limit", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Sources.MaxChangedFiles = 5

		input := testInput(policy)
		input.ChangedFiles = 5
		decision := evaluate(t, engine, input)
		if hasViolation(decision, "SOURCE_LIMITS_EXCEEDED") {
			t.Fatal("changed files at the limit must pass")
		}

		input.ChangedFiles = 6
		decision = evaluate(t, engine, input)
		if !hasViolation(decision, "SOURCE_LIMITS_EXCEEDED") || decision.Allowed {
			t.Fatalf("changed files over the limit accepted: %+v", decision)
		}
	})

	t.Run("hard changed file limit wins", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{MaxChangedFiles: 2})
		policy := testPolicy()
		policy.Spec.Sources.MaxChangedFiles = 10
		input := testInput(policy)
		input.ChangedFiles = 3
		decision := evaluate(t, engine, input)
		if !hasViolation(decision, "SOURCE_LIMITS_EXCEEDED") || decision.Allowed {
			t.Fatalf("hard changed file limit not enforced: %+v", decision)
		}
		input.ChangedFiles = 2
		decision = evaluate(t, engine, input)
		if hasViolation(decision, "SOURCE_LIMITS_EXCEEDED") {
			t.Fatal("changed files at hard limit must pass")
		}
	})

	t.Run("repository size limit", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Sources.MaxRepositorySizeGiB = 1
		input := testInput(policy)
		input.RepositorySizeGiB = 1.5
		decision := evaluate(t, engine, input)
		if !hasViolation(decision, "REPOSITORY_TOO_LARGE") || decision.Allowed {
			t.Fatalf("repository size over the limit accepted: %+v", decision)
		}
		input.RepositorySizeGiB = 1
		decision = evaluate(t, engine, input)
		if hasViolation(decision, "REPOSITORY_TOO_LARGE") {
			t.Fatal("repository size at the limit must pass")
		}
	})

	t.Run("hard repository size limit wins", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{MaxRepositoryBytes: 512 << 20})
		policy := testPolicy()
		policy.Spec.Sources.MaxRepositorySizeGiB = 2
		input := testInput(policy)
		input.RepositorySizeGiB = 0.75
		decision := evaluate(t, engine, input)
		if !hasViolation(decision, "REPOSITORY_TOO_LARGE") || decision.Allowed {
			t.Fatalf("hard repository size limit not enforced: %+v", decision)
		}
		input.RepositorySizeGiB = 0.5
		decision = evaluate(t, engine, input)
		if hasViolation(decision, "REPOSITORY_TOO_LARGE") {
			t.Fatal("repository size at hard limit must pass")
		}
	})

	t.Run("submodules", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Sources.DisallowSubmodulesUnlessApproved = true
		input := testInput(policy)
		input.Submodules = true
		decision := evaluate(t, engine, input)
		if !hasViolation(decision, "SUBMODULES_NOT_ALLOWED") || decision.Allowed {
			t.Fatalf("submodules accepted: %+v", decision)
		}
		input.Submodules = false
		decision = evaluate(t, engine, input)
		if hasViolation(decision, "SUBMODULES_NOT_ALLOWED") {
			t.Fatal("run without submodules flagged")
		}
	})

	t.Run("hard submodule ban", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{DisallowSubmodules: true})
		input := testInput(testPolicy())
		input.Submodules = true
		decision := evaluate(t, engine, input)
		if !hasViolation(decision, "SUBMODULES_NOT_ALLOWED") || decision.Allowed {
			t.Fatalf("hard submodule ban not enforced: %+v", decision)
		}
	})
}

func TestEvaluateProtectedPaths(t *testing.T) {
	t.Run("policy protected paths", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.ProtectedPaths = []string{"internal/**", "secrets/*.pem"}
		input := testInput(policy)
		input.ChangedPaths = []string{"internal/a.go", "README.md", "secrets/key.pem"}
		decision := evaluate(t, engine, input)
		violation := violationByCode(t, decision, "PROTECTED_PATH_MODIFIED")
		if violation.Severity != domain.SeverityHigh {
			t.Fatalf("severity = %s", violation.Severity)
		}
		if !strings.Contains(violation.Message, "internal/a.go") || !strings.Contains(violation.Message, "secrets/key.pem") {
			t.Fatalf("message = %q", violation.Message)
		}
		if strings.Contains(violation.Message, "README.md") {
			t.Fatalf("unprotected path reported: %q", violation.Message)
		}
		if decision.Allowed {
			t.Fatal("protected path modification must block")
		}
	})

	t.Run("hard protected paths", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{ProtectedPaths: []string{"secrets/**"}})
		input := testInput(testPolicy())
		input.ChangedPaths = []string{"secrets/key.pem"}
		decision := evaluate(t, engine, input)
		violation := violationByCode(t, decision, "PROTECTED_PATH_MODIFIED")
		if violation.Severity != domain.SeverityCritical {
			t.Fatalf("severity = %s", violation.Severity)
		}
		if !strings.Contains(violation.Message, "platform-protected") {
			t.Fatalf("message = %q", violation.Message)
		}
		if decision.Allowed {
			t.Fatal("platform protected path modification must block")
		}
	})

	t.Run("no protected path hit", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.ProtectedPaths = []string{"internal/**"}
		input := testInput(policy)
		input.ChangedPaths = []string{"docs/readme.md"}
		decision := evaluate(t, engine, input)
		if hasViolation(decision, "PROTECTED_PATH_MODIFIED") {
			t.Fatalf("unexpected protected path violation: %+v", decision.Violations)
		}
		if !decision.Allowed {
			t.Fatalf("decision not allowed: %+v", decision.Violations)
		}
	})
}

func TestEvaluateRegions(t *testing.T) {
	t.Run("policy and project intersection", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Execution.AllowedRegions = []string{"us-east-1", "eu-west-1"}
		input := testInput(policy)
		input.Project.AllowedRegions = []string{"eu-west-1", "ap-south-1"}
		decision := evaluate(t, engine, input)
		if !reflect.DeepEqual(decision.AllowedRegions, []string{"eu-west-1"}) {
			t.Fatalf("allowed regions = %v", decision.AllowedRegions)
		}

		input.Region = "eu-west-1"
		decision = evaluate(t, engine, input)
		if !decision.Allowed || hasViolation(decision, "REGION_NOT_ALLOWED") {
			t.Fatalf("allowed region rejected: %+v", decision)
		}

		input.Region = "us-east-1"
		decision = evaluate(t, engine, input)
		if !hasViolation(decision, "REGION_NOT_ALLOWED") || decision.Allowed {
			t.Fatalf("region outside the intersection accepted: %+v", decision)
		}
	})

	t.Run("empty intersection falls back to tenant home", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Execution.AllowedRegions = []string{"us-east-1"}
		input := testInput(policy)
		input.Project.AllowedRegions = []string{"eu-west-1"}
		decision := evaluate(t, engine, input)
		if !reflect.DeepEqual(decision.AllowedRegions, []string{"us-east-1"}) {
			t.Fatalf("allowed regions = %v, want tenant home", decision.AllowedRegions)
		}
		input.Region = "us-east-1"
		decision = evaluate(t, engine, input)
		if !decision.Allowed {
			t.Fatalf("tenant home region rejected: %+v", decision.Violations)
		}
		input.Region = "eu-west-1"
		decision = evaluate(t, engine, input)
		if !hasViolation(decision, "REGION_NOT_ALLOWED") {
			t.Fatalf("region outside tenant home accepted: %+v", decision)
		}
	})

	t.Run("hard allowlist narrows intersection", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{AllowedRegions: []string{"us-east-1"}})
		policy := testPolicy()
		policy.Spec.Execution.AllowedRegions = []string{"us-east-1", "eu-west-1"}
		input := testInput(policy)
		input.Project.AllowedRegions = []string{"us-east-1", "eu-west-1"}
		decision := evaluate(t, engine, input)
		if !reflect.DeepEqual(decision.AllowedRegions, []string{"us-east-1"}) {
			t.Fatalf("allowed regions = %v", decision.AllowedRegions)
		}
		input.Region = "eu-west-1"
		decision = evaluate(t, engine, input)
		if !hasViolation(decision, "REGION_NOT_ALLOWED") {
			t.Fatalf("region outside hard allowlist accepted: %+v", decision)
		}
	})

	t.Run("region comparison is case insensitive", func(t *testing.T) {
		engine := newEngine(t, HardPolicy{})
		policy := testPolicy()
		policy.Spec.Execution.AllowedRegions = []string{"eu-west-1"}
		input := testInput(policy)
		input.Region = "EU-WEST-1"
		decision := evaluate(t, engine, input)
		if hasViolation(decision, "REGION_NOT_ALLOWED") {
			t.Fatalf("case-insensitive region rejected: %+v", decision.Violations)
		}
	})
}

func TestEvaluateDeterministicViolationOrdering(t *testing.T) {
	engine := newEngine(t, HardPolicy{RequireTrustedPolicy: true})
	policy := testPolicy()
	policy.Spec.Sources.MaxChangedFiles = 1
	policy.Spec.Sources.DisallowSubmodulesUnlessApproved = true
	policy.Spec.ProtectedPaths = []string{"secret/**"}
	policy.Spec.Execution.AllowedRegions = []string{"us-east-1"}

	input := testInput(policy)
	input.PolicyTrusted = false
	input.Tenant.Status = domain.TenantMigrating
	input.ChangedFiles = 3
	input.Submodules = true
	input.ChangedPaths = []string{"secret/key.pem"}
	input.Region = "eu-west-1"
	input.RequestedChecks = []string{"mystery"}
	input.Project.AllowedRegions = []string{"eu-west-1"}

	first := evaluate(t, engine, input)
	second := evaluate(t, engine, input)
	if !reflect.DeepEqual(first.Violations, second.Violations) {
		t.Fatalf("violations not deterministic:\n%+v\n%+v", first.Violations, second.Violations)
	}

	codes := make([]string, 0, len(first.Violations))
	for _, violation := range first.Violations {
		codes = append(codes, violation.Code)
	}
	want := []string{
		"PROTECTED_PATH_MODIFIED",
		"REGION_NOT_ALLOWED",
		"SOURCE_LIMITS_EXCEEDED",
		"SUBMODULES_NOT_ALLOWED",
		"TENANT_NOT_ACTIVE",
		"UNKNOWN_CHECK",
		"UNTRUSTED_POLICY",
	}
	if !reflect.DeepEqual(codes, want) {
		t.Fatalf("violation order = %v, want %v", codes, want)
	}
	if first.Allowed {
		t.Fatal("decision with blocking violations must not be allowed")
	}
}

func TestHasBlockingViolation(t *testing.T) {
	tests := []struct {
		severity domain.Severity
		want     bool
	}{
		{domain.SeverityCritical, true},
		{domain.SeverityHigh, true},
		{domain.SeverityMedium, false},
		{domain.SeverityLow, false},
		{domain.SeverityInfo, false},
		{domain.Severity(""), false},
	}
	for _, tc := range tests {
		violations := []ports.PolicyViolation{{Code: "X", Severity: tc.severity}}
		if got := hasBlockingViolation(violations); got != tc.want {
			t.Errorf("severity %q blocking = %v, want %v", tc.severity, got, tc.want)
		}
	}
	if hasBlockingViolation(nil) {
		t.Error("no violations must not block")
	}
}

func TestEvaluateRejectsMismatchedProjectTenant(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	input := testInput(testPolicy())
	input.Project.TenantID = domain.TenantID("99999999-9999-4999-8999-999999999999")
	if _, err := engine.Evaluate(context.Background(), input); err == nil {
		t.Fatal("project belonging to another tenant accepted")
	}
}

func TestEvaluateRejectsInvalidPolicy(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	input := testInput(testPolicy())
	input.Policy.Spec.Verification.Required = nil
	if _, err := engine.Evaluate(context.Background(), input); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

func TestEvaluateHonorsContextCancellation(t *testing.T) {
	engine := newEngine(t, HardPolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Evaluate(ctx, testInput(testPolicy())); err == nil {
		t.Fatal("cancelled context accepted")
	}
}
