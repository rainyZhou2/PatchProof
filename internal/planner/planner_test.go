package planner_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/planner"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

const (
	planTenantID  = domain.TenantID("11111111-1111-4111-8111-111111111111")
	planProjectID = domain.ProjectID("22222222-2222-4222-8222-222222222222")
	planRunID     = domain.RunID("44444444-4444-4444-8444-444444444444")
	planPolicy    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	planSnapshot  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var planNow = time.Date(2026, time.March, 1, 10, 0, 0, 0, time.UTC)

func testRun() domain.Run {
	return domain.Run{
		ID:              planRunID,
		TenantID:        planTenantID,
		ProjectID:       planProjectID,
		BaseCommit:      strings.Repeat("a", 40),
		HeadCommit:      strings.Repeat("b", 40),
		PolicyDigest:    planPolicy,
		BaseSnapshot:    planSnapshot,
		HeadSnapshot:    planSnapshot,
		RequestedChecks: []string{"compile"},
		Status:          domain.RunPlanned,
	}
}

func contractAlpha() domain.ContractSpec {
	return domain.ContractSpec{
		ID:            "contract-alpha",
		Symbol:        "Parse",
		Language:      "go",
		Kind:          "behavior-preservation",
		Title:         "Parser behavior is preserved",
		Domain:        "ASCII digits",
		DomainRef:     "contracts/alpha-domain.yaml",
		Oracle:        "differential",
		OracleRef:     "contracts/alpha-oracle.yaml",
		Observable:    "return-value",
		Required:      true,
		MinimumResult: string(domain.ClaimTested),
	}
}

func contractBeta() domain.ContractSpec {
	return domain.ContractSpec{
		ID:            "contract-beta",
		Symbol:        "CanWithdraw",
		Language:      "go",
		Kind:          "fix-invariant",
		Title:         "Withdrawal invariant holds",
		Domain:        "signed 64-bit amounts",
		DomainRef:     "contracts/beta-domain.yaml",
		Oracle:        "differential",
		OracleRef:     "contracts/beta-oracle.yaml",
		Observable:    "return-value",
		Required:      true,
		MinimumResult: string(domain.ClaimProven),
	}
}

func testPolicy() domain.PolicyDocument {
	return domain.PolicyDocument{
		APIVersion: domain.PolicyAPIVersion,
		Kind:       "VerificationPolicy",
		Metadata:   domain.PolicyMetadata{Name: "planner-test"},
		Spec: domain.PolicySpec{
			Verification: domain.VerificationSpec{
				Required:  []string{"compile", "regression-test", "differential-fuzz"},
				Contracts: []domain.ContractSpec{contractAlpha(), contractBeta()},
			},
			Execution: domain.ExecutionSpec{
				RuntimeClass:       "standard",
				MaxStepWallSeconds: 600,
				MaxRunWallSeconds:  1200,
			},
		},
	}
}

func testDecision() ports.PolicyDecision {
	return ports.PolicyDecision{
		Allowed:           true,
		RequiredChecks:    []string{"compile", "regression-test", "differential-fuzz"},
		Isolation:         domain.IsolationContainer,
		SubjectNetwork:    domain.NetworkNone,
		ArtifactTransport: domain.TransportDirect,
	}
}

func testRequest() ports.PlanRequest {
	return ports.PlanRequest{
		Run:      testRun(),
		Policy:   testPolicy(),
		Decision: testDecision(),
		Now:      planNow,
	}
}

func newPlanner(t *testing.T, limits planner.Limits) *planner.Planner {
	t.Helper()
	if limits.DefaultIsolation == "" {
		limits.DefaultIsolation = domain.IsolationContainer
	}
	p, err := planner.New(16, limits)
	if err != nil {
		t.Fatalf("new planner: %v", err)
	}
	return p
}

func plan(t *testing.T, p *planner.Planner, request ports.PlanRequest) ports.PlanResult {
	t.Helper()
	result, err := p.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return result
}

func stepsByKey(t *testing.T, result ports.PlanResult) map[string]domain.Step {
	t.Helper()
	out := make(map[string]domain.Step, len(result.Steps))
	for _, step := range result.Steps {
		if _, dup := out[step.Key]; dup {
			t.Fatalf("duplicate step key %q", step.Key)
		}
		out[step.Key] = step
	}
	return out
}

func claimsByID(result ports.PlanResult) map[domain.ClaimID]domain.Claim {
	out := make(map[domain.ClaimID]domain.Claim, len(result.Claims))
	for _, claim := range result.Claims {
		out[claim.ID] = claim
	}
	return out
}

func sortedStepKeys(result ports.PlanResult) []string {
	keys := make([]string, 0, len(result.Steps))
	for _, step := range result.Steps {
		keys = append(keys, step.Key)
	}
	return keys
}

func dependencySet(t *testing.T, result ports.PlanResult, stepID domain.StepID) map[domain.StepID]struct{} {
	t.Helper()
	set := make(map[domain.StepID]struct{}, len(result.Dependencies[stepID]))
	for _, dependency := range result.Dependencies[stepID] {
		set[dependency] = struct{}{}
	}
	return set
}

func TestPlanFullPolicy(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 64})
	result := plan(t, p, testRequest())

	wantKeys := []string{
		"analyze", "impact", "compile-base", "compile-head", "regression",
		"differential-contract-alpha", "differential-contract-beta",
	}
	if got := sortedStepKeys(result); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("step keys = %v, want %v", got, wantKeys)
	}

	steps := stepsByKey(t, result)
	rootKeys := map[string]bool{"analyze": true, "compile-base": true, "compile-head": true}
	stepIDs := make(map[domain.StepID]struct{}, len(result.Steps))
	shards := make(map[int]string, len(result.Steps))
	for _, step := range result.Steps {
		if err := step.Validate(); err != nil {
			t.Fatalf("step %s invalid: %v", step.Key, err)
		}
		if _, dup := stepIDs[step.ID]; dup {
			t.Fatalf("duplicate step id %s", step.ID)
		}
		stepIDs[step.ID] = struct{}{}
		if step.RunID != planRunID || step.TenantID != planTenantID {
			t.Errorf("step %s has wrong ownership: %s/%s", step.Key, step.RunID, step.TenantID)
		}
		if step.InputManifestDigest == "" || !strings.HasPrefix(step.InputManifestDigest, "sha256:") {
			t.Errorf("step %s input manifest = %q", step.Key, step.InputManifestDigest)
		}
		if len(result.Dependencies[step.ID]) == 0 {
			if !rootKeys[step.Key] {
				t.Errorf("step %s has no dependencies but is not a root", step.Key)
			}
			if step.Status != domain.StepReady {
				t.Errorf("root step %s status = %s, want READY", step.Key, step.Status)
			}
		} else if step.Status != domain.StepBlocked {
			t.Errorf("dependent step %s status = %s, want BLOCKED", step.Key, step.Status)
		}
		if step.Shard < 0 || step.Shard >= 16 {
			t.Errorf("step %s shard = %d, out of range", step.Key, step.Shard)
		}
		if other, dup := shards[step.Shard]; dup {
			t.Errorf("step %s shares shard %d with %s", step.Key, step.Shard, other)
		}
		shards[step.Shard] = step.Key
		if step.CreatedAt != planNow || step.UpdatedAt != planNow {
			t.Errorf("step %s timestamps = %s/%s", step.Key, step.CreatedAt, step.UpdatedAt)
		}
	}

	// Dependency graph.
	impact := steps["impact"]
	if deps := dependencySet(t, result, impact.ID); len(deps) != 1 || !hasDep(deps, steps["analyze"].ID) {
		t.Errorf("impact dependencies = %v", result.Dependencies[impact.ID])
	}
	regression := steps["regression"]
	regressionDeps := dependencySet(t, result, regression.ID)
	if len(regressionDeps) != 2 {
		t.Errorf("regression dependencies = %v, want compile-head + impact", result.Dependencies[regression.ID])
	}
	if !hasDep(regressionDeps, steps["compile-head"].ID) || !hasDep(regressionDeps, impact.ID) {
		t.Errorf("regression dependencies = %v", result.Dependencies[regression.ID])
	}
	if hasDep(regressionDeps, steps["compile-base"].ID) {
		t.Error("regression must not depend on compile-base")
	}
	for _, key := range []string{"differential-contract-alpha", "differential-contract-beta"} {
		deps := dependencySet(t, result, steps[key].ID)
		if len(deps) != 3 {
			t.Errorf("%s dependencies = %v, want 3", key, result.Dependencies[steps[key].ID])
		}
		for _, expected := range []domain.StepID{steps["compile-base"].ID, steps["compile-head"].ID, impact.ID} {
			if !hasDep(deps, expected) {
				t.Errorf("%s missing dependency %s", key, expected)
			}
		}
	}

	// Claim set and properties.
	claims := claimsByID(result)
	wantClaims := []domain.ClaimID{"static-analysis", "compile", "regression", "contract-alpha", "contract-beta"}
	if len(claims) != len(wantClaims) {
		t.Fatalf("claims = %v, want %v", claims, wantClaims)
	}
	for _, id := range wantClaims {
		if _, ok := claims[id]; !ok {
			t.Errorf("missing claim %s", id)
		}
	}
	if claim := claims["compile"]; claim.Kind != domain.ClaimKindCompile || !claim.Required || claim.MinimumResult != domain.ClaimTested {
		t.Errorf("compile claim = %+v", claim)
	}
	if claim := claims["static-analysis"]; claim.Required {
		t.Error("static-analysis claim should be optional")
	}
	if claim := claims["regression"]; claim.Kind != domain.ClaimKindTest || !claim.Required {
		t.Errorf("regression claim = %+v", claim)
	}
	if claim := claims["contract-alpha"]; !claim.Required || claim.Kind != domain.ClaimKindRegression || claim.OracleRef != contractAlpha().DomainRef {
		t.Errorf("alpha claim = %+v", claim)
	}
	if claim := claims["contract-beta"]; claim.Kind != domain.ClaimKindFix || claim.MinimumResult != domain.ClaimProven {
		t.Errorf("beta claim = %+v", claim)
	}
	for _, claim := range result.Claims {
		if claim.RunID != planRunID || claim.TenantID != planTenantID {
			t.Errorf("claim %s has wrong ownership", claim.ID)
		}
	}

	// Options and requirements.
	if got := steps["analyze"].Options[stepopts.ClaimID]; got != "static-analysis" {
		t.Errorf("analyze claim option = %q", got)
	}
	if got := steps["compile-base"].Options[stepopts.ClaimID]; got != "compile" {
		t.Errorf("compile-base claim option = %q", got)
	}
	if got := steps["compile-base"].Options["target"]; got != "base" {
		t.Errorf("compile-base target = %q", got)
	}
	if got := steps["compile-head"].Options["target"]; got != "head" {
		t.Errorf("compile-head target = %q", got)
	}
	if got := steps["regression"].Options[stepopts.ClaimID]; got != "regression" {
		t.Errorf("regression claim option = %q", got)
	}
	alphaStep := steps["differential-contract-alpha"]
	for key, want := range map[string]string{
		stepopts.ClaimID:       "contract-alpha",
		stepopts.ContractID:    "contract-alpha",
		stepopts.ContractRef:   "contracts/alpha-domain.yaml",
		stepopts.TargetSymbol:  "Parse",
		stepopts.InputDomain:   "ASCII digits",
		stepopts.Observable:    "return-value",
		stepopts.MinimumResult: string(domain.ClaimTested),
	} {
		if got := alphaStep.Options[key]; got != want {
			t.Errorf("alpha step option %q = %q, want %q", key, got, want)
		}
	}
	if got := alphaStep.Options[stepopts.FuzzSeconds]; got != "30" {
		t.Errorf("alpha fuzz seconds = %q, want default 30", got)
	}
	betaStep := steps["differential-contract-beta"]
	if got := betaStep.Options[stepopts.MinimumResult]; got != string(domain.ClaimProven) {
		t.Errorf("beta minimum result = %q", got)
	}

	for _, step := range result.Steps {
		if step.Options[stepopts.Language] != "go" || step.Options[stepopts.Engine] == "" {
			t.Errorf("step %s missing language/engine options: %v", step.Key, step.Options)
		}
		if step.Options[stepopts.BaseCommit] != testRun().BaseCommit || step.Options[stepopts.HeadCommit] != testRun().HeadCommit {
			t.Errorf("step %s commit options = %v", step.Key, step.Options)
		}
		requirements := step.Requirements
		if requirements.Isolation != domain.IsolationContainer {
			t.Errorf("step %s isolation = %s", step.Key, requirements.Isolation)
		}
		if requirements.Network != domain.NetworkRestricted {
			t.Errorf("step %s platform network = %s, want restricted", step.Key, requirements.Network)
		}
		if requirements.SubjectNetwork != domain.NetworkNone {
			t.Errorf("step %s subject network = %s, want none", step.Key, requirements.SubjectNetwork)
		}
		if requirements.ArtifactTransport != domain.TransportDirect {
			t.Errorf("step %s transport = %s", step.Key, requirements.ArtifactTransport)
		}
		if requirements.RuntimeClass != "standard" {
			t.Errorf("step %s runtime class = %s", step.Key, requirements.RuntimeClass)
		}
		if len(requirements.RequiredFeatures) != 0 {
			t.Errorf("step %s features = %v", step.Key, requirements.RequiredFeatures)
		}
		if requirements.DeadlineSeconds > 600 {
			t.Errorf("step %s deadline = %d, exceeds policy limit", step.Key, requirements.DeadlineSeconds)
		}
		if requirements.CPUMilli <= 0 || requirements.MemoryMiB <= 0 {
			t.Errorf("step %s has non-positive resources: %+v", step.Key, requirements)
		}
	}

	// Priorities and max attempts by kind.
	wantPriorities := map[string]int{
		"analyze": 60, "impact": 55, "compile-base": 90, "compile-head": 95,
		"regression": 85, "differential-contract-alpha": 75, "differential-contract-beta": 75,
	}
	for key, want := range wantPriorities {
		if got := steps[key].Priority; got != want {
			t.Errorf("step %s priority = %d, want %d", key, got, want)
		}
	}
	for _, key := range []string{"analyze", "impact", "compile-base", "compile-head", "regression", "differential-contract-alpha", "differential-contract-beta"} {
		if got := steps[key].MaxAttempts; got != 2 {
			t.Errorf("step %s max attempts = %d, want 2", key, got)
		}
	}
}

func hasDep(deps map[domain.StepID]struct{}, target domain.StepID) bool {
	_, ok := deps[target]
	return ok
}

func TestPlanCompileAlwaysPlanned(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"regression-test"}
	request.Decision.RequiredChecks = []string{"regression-test"}

	result := plan(t, p, request)
	steps := stepsByKey(t, result)
	for _, key := range []string{"compile-base", "compile-head", "regression"} {
		if _, ok := steps[key]; !ok {
			t.Errorf("missing step %s", key)
		}
	}
	if _, ok := steps["differential-contract-alpha"]; ok {
		t.Error("differential step planned although not required")
	}
	if _, ok := claimsByID(result)["compile"]; !ok {
		t.Fatal("compile claim missing even though compile is implicit")
	}
}

func TestPlanProtectedAssetsClaimHasNoStep(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"protected-assets"}
	request.Decision.RequiredChecks = []string{"protected-assets"}

	result := plan(t, p, request)
	if got := sortedStepKeys(result); !reflect.DeepEqual(got, []string{"analyze", "impact", "compile-base", "compile-head"}) {
		t.Fatalf("steps = %v", got)
	}
	claims := claimsByID(result)
	claim, ok := claims["protected-assets"]
	if !ok {
		t.Fatal("protected-assets claim missing")
	}
	if !claim.Required || claim.Kind != domain.ClaimKindSecurity {
		t.Fatalf("protected-assets claim = %+v", claim)
	}
	for _, step := range result.Steps {
		if step.Options[stepopts.ClaimID] == "protected-assets" {
			t.Fatalf("protected-assets must not own a step: %s", step.Key)
		}
	}
}

func TestPlanSecurityScanStep(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"security-scan"}
	request.Decision.RequiredChecks = []string{"security-scan"}

	result := plan(t, p, request)
	steps := stepsByKey(t, result)
	security, ok := steps["security"]
	if !ok {
		t.Fatal("security step missing")
	}
	if security.Kind != domain.StepKindSecurity {
		t.Fatalf("security kind = %s", security.Kind)
	}
	if security.Priority != 70 {
		t.Fatalf("security priority = %d", security.Priority)
	}
	if security.MaxAttempts != 3 {
		t.Fatalf("security max attempts = %d, want 3", security.MaxAttempts)
	}
	if security.Options[stepopts.ClaimID] != "security-scan" {
		t.Fatalf("security claim option = %q", security.Options[stepopts.ClaimID])
	}
	deps := dependencySet(t, result, security.ID)
	if len(deps) != 1 || !hasDep(deps, steps["impact"].ID) {
		t.Fatalf("security dependencies = %v", result.Dependencies[security.ID])
	}
	claim, ok := claimsByID(result)["security-scan"]
	if !ok || !claim.Required || claim.Kind != domain.ClaimKindSecurity {
		t.Fatalf("security claim = %+v", claim)
	}
}

func TestPlanBoundedSMTClaimOnly(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"bounded-smt"}
	request.Decision.RequiredChecks = []string{"bounded-smt"}

	_, err := p.Plan(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "bounded-smt") {
		t.Fatalf("Plan error = %v, want bounded-smt rejection", err)
	}
}

func TestPlanDifferentialWithoutContracts(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Contracts = nil
	_, err := p.Plan(context.Background(), request)
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if !strings.Contains(validationErr.Reason, "contract") {
		t.Fatalf("reason = %q", validationErr.Reason)
	}
}

func TestPlanContractWithoutRef(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	bad := contractAlpha()
	bad.DomainRef = ""
	bad.OracleRef = ""
	request.Policy.Spec.Verification.Contracts = []domain.ContractSpec{bad}
	_, err := p.Plan(context.Background(), request)
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if validationErr.Field != "contract.domainRef" {
		t.Fatalf("field = %q, want contract.domainRef", validationErr.Field)
	}
}

func TestPlanDuplicateContractIDs(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Contracts = []domain.ContractSpec{contractAlpha(), contractAlpha()}
	_, err := p.Plan(context.Background(), request)
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if !strings.Contains(validationErr.Reason, "duplicate") {
		t.Fatalf("reason = %q", validationErr.Reason)
	}
}

func TestPlanDifferentialDisabled(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"compile"}
	request.Decision.RequiredChecks = []string{"compile"}

	result := plan(t, p, request)
	if got := sortedStepKeys(result); !reflect.DeepEqual(got, []string{"analyze", "impact", "compile-base", "compile-head"}) {
		t.Fatalf("steps = %v", got)
	}
	claims := claimsByID(result)
	if len(claims) != 2 {
		t.Fatalf("claims = %v, want static-analysis and compile only", claims)
	}
	for _, id := range []domain.ClaimID{"contract-alpha", "contract-beta", "regression"} {
		if _, ok := claims[id]; ok {
			t.Errorf("claim %s planned although differential/regression is disabled", id)
		}
	}
}

func TestPlanMaxStepCount(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 4})
	_, err := p.Plan(context.Background(), testRequest())
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if !strings.Contains(validationErr.Reason, "limit") {
		t.Fatalf("reason = %q", validationErr.Reason)
	}
}

func TestPlanDecisionNotAllowed(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Decision.Allowed = false
	_, err := p.Plan(context.Background(), request)
	var policyErr *domain.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error = %v, want *PolicyError", err)
	}
}

func TestPlanMissingPolicyDigest(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Run.PolicyDigest = ""
	_, err := p.Plan(context.Background(), request)
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if validationErr.Field != "policy_digest" {
		t.Fatalf("field = %q, want policy_digest", validationErr.Field)
	}
}

func TestPlanBrokeredOfflineRequirements(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Decision.ArtifactTransport = domain.TransportBrokeredOffline
	request.Decision.SubjectNetwork = domain.NetworkNone

	result := plan(t, p, request)
	for _, step := range result.Steps {
		requirements := step.Requirements
		if requirements.Network != domain.NetworkNone {
			t.Errorf("step %s platform network = %s, want none", step.Key, requirements.Network)
		}
		if requirements.SubjectNetwork != domain.NetworkNone {
			t.Errorf("step %s subject network = %s, want none", step.Key, requirements.SubjectNetwork)
		}
		if requirements.ArtifactTransport != domain.TransportBrokeredOffline {
			t.Errorf("step %s transport = %s", step.Key, requirements.ArtifactTransport)
		}
		if !reflect.DeepEqual(requirements.RequiredFeatures, []string{"brokered-offline-io"}) {
			t.Errorf("step %s features = %v", step.Key, requirements.RequiredFeatures)
		}
	}
}

func TestPlanFallsBackToDefaultIsolation(t *testing.T) {
	p := newPlanner(t, planner.Limits{DefaultIsolation: domain.IsolationGVisor})
	request := testRequest()
	request.Decision.Isolation = ""
	result := plan(t, p, request)
	for _, step := range result.Steps {
		if step.Requirements.Isolation != domain.IsolationGVisor {
			t.Errorf("step %s isolation = %s, want gvisor default", step.Key, step.Requirements.Isolation)
		}
	}
}

func TestPlanRejectsRequiredCheckWithoutEngine(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"compile", "bounded-smt"}
	request.Decision.RequiredChecks = []string{"compile", "bounded-smt"}
	_, err := p.Plan(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "bounded-smt") {
		t.Fatalf("Plan error = %v, want bounded-smt rejection", err)
	}
}

func TestPlanRejectsIsolationAboveMaximum(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxIsolation: domain.IsolationContainer})
	request := testRequest()
	request.Decision.Isolation = domain.IsolationGVisor
	_, err := p.Plan(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "maxIsolation") {
		t.Fatalf("Plan error = %v, want maxIsolation rejection", err)
	}
}

func TestPlanFuzzBudgetOptions(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Budgets.FuzzCPUSeconds = 120
	request.Policy.Spec.Execution.MaxStepWallSeconds = 600
	result := plan(t, p, request)
	steps := stepsByKey(t, result)
	alpha := steps["differential-contract-alpha"]
	if got := alpha.Options[stepopts.FuzzSeconds]; got != "60" {
		t.Fatalf("fuzz seconds = %q, want 60", got)
	}
	if alpha.Requirements.DeadlineSeconds != 180 {
		t.Fatalf("differential deadline = %d, want fuzz+120", alpha.Requirements.DeadlineSeconds)
	}
}

func TestInputManifestIsStableAcrossRuns(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	first := plan(t, p, testRequest())
	secondRequest := testRequest()
	secondRequest.Run.ID = domain.NewRunID()
	second := plan(t, p, secondRequest)
	firstSteps := stepsByKey(t, first)
	secondSteps := stepsByKey(t, second)
	if firstSteps["analyze"].InputManifestDigest != secondSteps["analyze"].InputManifestDigest {
		t.Fatalf("analyze input digest changed across runs: %s vs %s",
			firstSteps["analyze"].InputManifestDigest, secondSteps["analyze"].InputManifestDigest)
	}
	if firstSteps["analyze"].InputManifestDigest == "" {
		t.Fatal("analyze input digest is empty")
	}
}

func TestPlanDeterminism(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 64})
	request := testRequest()
	first := plan(t, p, request)
	second := plan(t, p, request)

	if len(first.Steps) != len(second.Steps) {
		t.Fatalf("step counts differ: %d vs %d", len(first.Steps), len(second.Steps))
	}
	firstKeys := stepsByKey(t, first)
	secondKeys := stepsByKey(t, second)
	for key, firstStep := range firstKeys {
		secondStep, ok := secondKeys[key]
		if !ok {
			t.Fatalf("step %s missing from second plan", key)
		}
		if firstStep.ID == secondStep.ID {
			t.Errorf("step IDs should differ across plans: %s", firstStep.ID)
		}
		if firstStep.Priority != secondStep.Priority {
			t.Errorf("step %s priority differs", key)
		}
		if firstStep.Shard != secondStep.Shard {
			t.Errorf("step %s shard differs: %d vs %d", key, firstStep.Shard, secondStep.Shard)
		}
		if firstStep.InputManifestDigest != secondStep.InputManifestDigest {
			t.Errorf("step %s input manifest differs: %s vs %s", key, firstStep.InputManifestDigest, secondStep.InputManifestDigest)
		}
		if !reflect.DeepEqual(firstStep.Options, secondStep.Options) {
			t.Errorf("step %s options differ", key)
		}
		if !reflect.DeepEqual(firstStep.Requirements, secondStep.Requirements) {
			t.Errorf("step %s requirements differ", key)
		}
	}
	if !reflect.DeepEqual(first.Claims, second.Claims) {
		t.Fatalf("claims differ across plans")
	}

	// Dependency shape must be identical once keyed by step key.
	for key, firstStep := range firstKeys {
		secondStep := secondKeys[key]
		firstDeps := make(map[string]struct{})
		for _, dep := range first.Dependencies[firstStep.ID] {
			firstDeps[dependencyKey(t, firstKeys, dep)] = struct{}{}
		}
		secondDeps := make(map[string]struct{})
		for _, dep := range second.Dependencies[secondStep.ID] {
			secondDeps[dependencyKey(t, secondKeys, dep)] = struct{}{}
		}
		if !reflect.DeepEqual(firstDeps, secondDeps) {
			t.Errorf("step %s dependency keys differ: %v vs %v", key, firstDeps, secondDeps)
		}
	}
}

func dependencyKey(t *testing.T, steps map[string]domain.Step, id domain.StepID) string {
	t.Helper()
	for key, step := range steps {
		if step.ID == id {
			return key
		}
	}
	t.Fatalf("dependency %s does not map to a step key", id)
	return ""
}

// TestPlanContractWithOnlyDomainRefReproduction verifies that a policy
// contract using only domainRef/oracleRef is planned with a non-empty claim
// domain derived from the referenced contract document.
func TestPlanContractWithOnlyDomainRefReproduction(t *testing.T) {
	p := newPlanner(t, planner.Limits{})
	request := testRequest()
	request.Policy.Spec.Execution.IsolationTier = domain.IsolationContainer
	request.Policy.Spec.Execution.DefaultCPUMilli = 1000
	request.Policy.Spec.Execution.DefaultMemoryMiB = 1024
	request.Policy.Spec.Execution.MaxRunWallSeconds = 1200
	request.Policy.Spec.Execution.SubjectNetwork = domain.NetworkNone
	request.Policy.Spec.Execution.ArtifactTransport = domain.TransportDirect
	request.Policy.Spec.Verification.Contracts = []domain.ContractSpec{{
		ID:        "contract-ref-only",
		Symbol:    "Parse",
		Language:  "go",
		Kind:      "behavior-preservation",
		DomainRef: "contracts/ref-only-domain.yaml",
		OracleRef: "contracts/ref-only-oracle.yaml",
		Required:  true,
	}}
	if err := request.Policy.Validate(); err != nil {
		t.Fatalf("ref-only contract policy should validate: %v", err)
	}
	if _, err := p.Plan(context.Background(), request); err != nil {
		t.Fatalf("valid ref-only contract rejected: %v", err)
	}
}

func TestPlanUsesPolicyMinimumEvidenceLevels(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 64})
	request := testRequest()
	request.Policy.Spec.Verification.MinimumEvidenceLevels = map[string]string{
		"compile":           string(domain.ClaimProven),
		"regression-test":   string(domain.ClaimProven),
		"differential-fuzz": string(domain.ClaimProven),
	}
	result := plan(t, p, request)
	claims := make(map[domain.ClaimID]domain.Claim, len(result.Claims))
	for _, claim := range result.Claims {
		claims[claim.ID] = claim
	}
	for _, id := range []domain.ClaimID{"compile", "regression", "contract-alpha", "contract-beta"} {
		claim, ok := claims[id]
		if !ok {
			t.Fatalf("claim %s is missing", id)
		}
		if claim.MinimumResult != domain.ClaimProven {
			t.Fatalf("claim %s minimum = %s, want PROVEN", id, claim.MinimumResult)
		}
	}
}

func TestPlanNativeFuzzSteps(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 64})
	request := testRequest()
	request.Policy.Spec.Verification.Required = []string{"compile", "native-fuzz"}
	request.Decision.RequiredChecks = []string{"compile", "native-fuzz"}
	request.Run.RequestedChecks = []string{"compile", "native-fuzz"}
	result := plan(t, p, request)
	steps := stepsByKey(t, result)
	for _, key := range []string{"native-fuzz-contract-alpha", "native-fuzz-contract-beta"} {
		step, ok := steps[key]
		if !ok {
			t.Fatalf("step %s is missing: %v", key, sortedStepKeys(result))
		}
		if step.Requirements.Engine != "go-native-fuzz" {
			t.Fatalf("step %s engine = %s", key, step.Requirements.Engine)
		}
		if step.Options[stepopts.CheckName] != "native-fuzz" || step.Options[stepopts.ClaimID] != "native-fuzz" {
			t.Fatalf("step %s options = %v", key, step.Options)
		}
	}
	if _, ok := steps["differential-contract-alpha"]; ok {
		t.Fatal("native-fuzz must not plan differential steps")
	}
	claim, ok := claimsByID(result)["native-fuzz"]
	if !ok || !claim.Required || claim.MinimumResult != domain.ClaimTested {
		t.Fatalf("native-fuzz claim = %+v", claim)
	}
}

func TestPlanReplayPinsContractAndInput(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 64})
	request := testRequest()
	request.Run.Replay = &domain.ReplaySpec{
		SourceRunID: domain.NewRunID(),
		CaseID:      "ce-abc",
		ClaimID:     "contract-alpha",
		InputRef:    domain.NewCASRef(strings.Repeat("9", 64), 12),
		InputCodec:  "json-args/v1",
	}
	result := plan(t, p, request)
	steps := stepsByKey(t, result)
	alpha, ok := steps["differential-contract-alpha"]
	if !ok {
		t.Fatal("replay step is missing")
	}
	if _, ok := steps["differential-contract-beta"]; ok {
		t.Fatal("replay must plan only the matching contract")
	}
	if got := alpha.Options[stepopts.ReplayCase]; got != "ce-abc" {
		t.Fatalf("replay.case = %q", got)
	}
	if got := alpha.Options[stepopts.InputPrefix+"replay"]; got != request.Run.Replay.InputRef.URI() {
		t.Fatalf("replay input = %q", got)
	}
}

func TestPlanReplayRejectsUnknownClaim(t *testing.T) {
	p := newPlanner(t, planner.Limits{MaxStepCount: 64})
	request := testRequest()
	request.Run.Replay = &domain.ReplaySpec{
		SourceRunID: domain.NewRunID(),
		CaseID:      "ce-abc",
		ClaimID:     "missing-contract",
		InputRef:    domain.NewCASRef(strings.Repeat("9", 64), 12),
		InputCodec:  "json-args/v1",
	}
	if _, err := p.Plan(context.Background(), request); err == nil {
		t.Fatal("unknown replay claim should be rejected")
	}
}
