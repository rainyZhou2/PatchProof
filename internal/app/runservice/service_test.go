package runservice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
)

var runTestNow = time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)

const runPolicyYAML = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: baseline
spec:
  verification:
    required:
      - compile
    optional:
      - regression-test
  execution:
    isolationTier: isolated-container
    defaultCpuMilli: 1000
    defaultMemoryMiB: 1024
    maxStepWallSeconds: 600
    maxRunWallSeconds: 1800
    subjectNetwork: none
    artifactTransport: direct
  budgets:
    totalCpuSeconds: 300
    fuzzCpuSeconds: 50
    maxParallelSteps: 4
`

type runClock struct{ at time.Time }

func (c runClock) Now() time.Time { return c.at }

type runFixture struct {
	store        *memory.Store
	service      *Service
	clock        runClock
	tenantID     domain.TenantID
	projectID    domain.ProjectID
	policyDigest string
}

func defaultRunLimits() Limits {
	return Limits{
		DefaultCPUSeconds:     500,
		DefaultWallSeconds:    3600,
		DefaultFuzzCPUSeconds: 100,
		MaxParallelSteps:      8,
		APIListMaxLimit:       200,
	}
}

func newRunFixtureWithLimits(t *testing.T, limits Limits) *runFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	tenantID := domain.NewTenantID()
	tenant := domain.Tenant{
		ID: tenantID, HomeRegion: "region-a", SecurityTier: domain.SecurityTierStandard,
		Status: domain.TenantActive, ActiveWriterEpoch: 1,
		CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}
	if err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	policy := putRunPolicy(t, store, tenantID, "baseline", []byte(runPolicyYAML), domain.PolicySourceAdmin, true)
	project := domain.Project{
		ID: domain.NewProjectID(), TenantID: tenantID, Provider: "git",
		RepositoryURI: "https://example.com/acme/app.git", DefaultBranch: "main",
		DefaultPolicyDigest: policy.Digest, Classification: "standard",
		CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	service, err := New(store, Config{Region: "region-a", Clock: runClock{at: runTestNow}, Limits: limits}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &runFixture{
		store: store, service: service, clock: runClock{at: runTestNow},
		tenantID: tenantID, projectID: project.ID, policyDigest: policy.Digest,
	}
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	return newRunFixtureWithLimits(t, defaultRunLimits())
}

func putRunPolicy(t *testing.T, store *memory.Store, tenantID domain.TenantID, name string, raw []byte, source domain.PolicySource, trusted bool) domain.Policy {
	t.Helper()
	document, err := domain.ParsePolicyDocument(raw)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	digest, err := document.Digest()
	if err != nil {
		t.Fatalf("policy digest: %v", err)
	}
	policy := domain.Policy{
		TenantID: tenantID, Digest: digest, Version: "v1", Source: source, Trusted: trusted,
		Document: raw, CreatedBy: "alice", CreatedAt: runTestNow,
	}
	if err := store.PutPolicy(context.Background(), policy); err != nil {
		t.Fatalf("put policy %s: %v", name, err)
	}
	return policy
}

func (f *runFixture) input(requestKey string) SubmitInput {
	return SubmitInput{
		TenantID:        f.tenantID,
		ProjectID:       f.projectID,
		RequestKey:      requestKey,
		BaseCommit:      strings.Repeat("a", 40),
		HeadCommit:      strings.Repeat("b", 40),
		PolicyDigest:    f.policyDigest,
		RequestedChecks: []string{"compile"},
		CreatedBy:       "alice",
	}
}

func TestSubmitCreatesCreatedRun(t *testing.T) {
	fixture := newRunFixture(t)
	input := fixture.input("create-1")
	input.RequestedChecks = []string{" Compile ", "compile", "regression-test", " "}
	input.BaseCommit = strings.Repeat("A", 40)

	run, err := fixture.service.Submit(context.Background(), input)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if run.Status != domain.RunCreated {
		t.Fatalf("status = %s, want CREATED", run.Status)
	}
	if run.StateVersion != 0 {
		t.Fatalf("state version = %d, want 0", run.StateVersion)
	}
	wantChecks := []string{"compile", "regression-test"}
	if len(run.RequestedChecks) != len(wantChecks) {
		t.Fatalf("requested checks = %v, want %v", run.RequestedChecks, wantChecks)
	}
	for i := range wantChecks {
		if run.RequestedChecks[i] != wantChecks[i] {
			t.Fatalf("requested checks = %v, want %v", run.RequestedChecks, wantChecks)
		}
	}
	if run.BaseCommit != strings.Repeat("a", 40) {
		t.Fatalf("base commit = %q, want lowercased", run.BaseCommit)
	}
	if run.HeadCommit != strings.Repeat("b", 40) {
		t.Fatalf("head commit = %q", run.HeadCommit)
	}
	if run.Region != "region-a" {
		t.Fatalf("region = %q, want tenant home region", run.Region)
	}
	if run.PolicyDigest != fixture.policyDigest {
		t.Fatalf("policy digest = %q, want %q", run.PolicyDigest, fixture.policyDigest)
	}
	if run.Fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
	wantBudget := domain.Budget{CPUSeconds: 300, WallSeconds: 1800, FuzzCPUSeconds: 50, MaxParallelSteps: 4}
	if run.Budget != wantBudget {
		t.Fatalf("budget = %+v, want %+v", run.Budget, wantBudget)
	}
	wantFingerprint, err := fingerprintOf(domain.RunRequest{
		ProjectID: fixture.projectID, BaseCommit: strings.Repeat("a", 40),
		HeadCommit: strings.Repeat("b", 40), PolicyDigest: fixture.policyDigest,
		RequestedChecks: wantChecks, Budget: wantBudget, Region: "region-a",
	})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if run.Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", run.Fingerprint, wantFingerprint)
	}
	if !run.CreatedAt.Equal(runTestNow) || !run.UpdatedAt.Equal(runTestNow) {
		t.Fatalf("timestamps = %v/%v, want %v", run.CreatedAt, run.UpdatedAt, runTestNow)
	}
	if run.CreatedBy != "alice" {
		t.Fatalf("created by = %q, want alice", run.CreatedBy)
	}
}

func TestSubmitIsIdempotentPerRequestKey(t *testing.T) {
	fixture := newRunFixture(t)
	ctx := context.Background()
	input := fixture.input("idempotent-1")

	first, err := fixture.service.Submit(ctx, input)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := fixture.service.Submit(ctx, input)
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate submission created a new run: %s vs %s", first.ID, second.ID)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("duplicate fingerprint mismatch: %q vs %q", first.Fingerprint, second.Fingerprint)
	}
	page, err := fixture.service.List(ctx, domain.RunFilter{TenantID: fixture.tenantID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(page.Runs))
	}

	changed := input
	changed.HeadCommit = strings.Repeat("c", 40)
	_, err = fixture.service.Submit(ctx, changed)
	var conflict *domain.IdempotencyConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("changed-input error = %v, want *domain.IdempotencyConflictError", err)
	}
	if conflict.RequestKey != input.RequestKey {
		t.Fatalf("conflict request key = %q, want %q", conflict.RequestKey, input.RequestKey)
	}
	page, err = fixture.service.List(ctx, domain.RunFilter{TenantID: fixture.tenantID})
	if err != nil {
		t.Fatalf("list runs after conflict: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs after conflict = %d, want 1", len(page.Runs))
	}
}

func TestSubmitResolvesPolicyFromRef(t *testing.T) {
	fixture := newRunFixture(t)
	input := fixture.input("policy-ref-1")
	input.PolicyDigest = ""
	input.PolicyRef = "baseline@" + fixture.policyDigest
	run, err := fixture.service.Submit(context.Background(), input)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if run.PolicyDigest != fixture.policyDigest {
		t.Fatalf("policy digest = %q, want %q", run.PolicyDigest, fixture.policyDigest)
	}
}

func TestSubmitRejectsUnsupportedChecks(t *testing.T) {
	fixture := newRunFixture(t)
	ctx := context.Background()
	cases := [][]string{
		{"sast"},
		{"compile", "not-a-check"},
	}
	for _, checks := range cases {
		input := fixture.input("bad-check-" + strings.Join(checks, "-"))
		input.RequestedChecks = checks
		_, err := fixture.service.Submit(ctx, input)
		var validation *domain.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("checks %v: error = %v, want *domain.ValidationError", checks, err)
		}
		if validation.Field != "requested_checks" {
			t.Fatalf("checks %v: error field = %q, want requested_checks", checks, validation.Field)
		}
	}
	page, err := fixture.service.List(ctx, domain.RunFilter{TenantID: fixture.tenantID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(page.Runs) != 0 {
		t.Fatalf("runs created for rejected checks: %d", len(page.Runs))
	}
}

func TestSubmitRejectsUntrustedPolicy(t *testing.T) {
	fixture := newRunFixture(t)
	ctx := context.Background()

	untrusted := putRunPolicy(t, fixture.store, fixture.tenantID, "untrusted",
		[]byte(strings.Replace(runPolicyYAML, "name: baseline", "name: untrusted", 1)),
		domain.PolicySourceAdmin, false)
	wrongSource := putRunPolicy(t, fixture.store, fixture.tenantID, "base-branch",
		[]byte(strings.Replace(runPolicyYAML, "name: baseline", "name: base-branch", 1)),
		domain.PolicySourceBaseBranch, true)

	for _, digest := range []string{untrusted.Digest, wrongSource.Digest} {
		input := fixture.input("untrusted-" + digest)
		input.PolicyDigest = digest
		_, err := fixture.service.Submit(ctx, input)
		var policyErr *domain.PolicyError
		if !errors.As(err, &policyErr) {
			t.Fatalf("policy %s: error = %v, want *domain.PolicyError", digest, err)
		}
		if policyErr.Code != "UNTRUSTED_POLICY" {
			t.Fatalf("policy code = %q, want UNTRUSTED_POLICY", policyErr.Code)
		}
	}
}

func TestSubmitRejectsInactiveTenant(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	tenantID := domain.NewTenantID()
	tenant := domain.Tenant{
		ID: tenantID, HomeRegion: "region-a", SecurityTier: domain.SecurityTierStandard,
		Status: domain.TenantSuspended, ActiveWriterEpoch: 1,
		CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}
	if err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	policy := putRunPolicy(t, store, tenantID, "baseline", []byte(runPolicyYAML), domain.PolicySourceAdmin, true)
	project := domain.Project{
		ID: domain.NewProjectID(), TenantID: tenantID, Provider: "git",
		RepositoryURI: "https://example.com/acme/app.git", DefaultBranch: "main",
		DefaultPolicyDigest: policy.Digest, Classification: "standard",
		CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	service, err := New(store, Config{Clock: runClock{at: runTestNow}, Limits: defaultRunLimits()}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	_, err = service.Submit(ctx, SubmitInput{
		TenantID: tenantID, ProjectID: project.ID, RequestKey: "inactive-1",
		BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40),
		PolicyDigest: policy.Digest, RequestedChecks: []string{"compile"}, CreatedBy: "alice",
	})
	var policyErr *domain.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error = %v, want *domain.PolicyError", err)
	}
	if policyErr.Code != "TENANT_NOT_ACTIVE" {
		t.Fatalf("policy code = %q, want TENANT_NOT_ACTIVE", policyErr.Code)
	}
}

func TestSubmitRejectsProjectFromAnotherTenant(t *testing.T) {
	ctx := context.Background()
	fixture := newRunFixture(t)
	otherTenant := domain.NewTenantID()
	if err := fixture.store.CreateTenant(ctx, domain.Tenant{
		ID: otherTenant, HomeRegion: "region-a", SecurityTier: domain.SecurityTierStandard,
		Status: domain.TenantActive, ActiveWriterEpoch: 1, CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}); err != nil {
		t.Fatalf("create other tenant: %v", err)
	}
	foreignProject := domain.Project{
		ID: domain.NewProjectID(), TenantID: otherTenant, Provider: "git",
		RepositoryURI: "https://example.com/other/app.git", DefaultBranch: "main",
		Classification: "standard", CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}
	if err := fixture.store.CreateProject(ctx, foreignProject); err != nil {
		t.Fatalf("create foreign project: %v", err)
	}
	input := fixture.input("foreign-project")
	input.ProjectID = foreignProject.ID
	_, err := fixture.service.Submit(ctx, input)
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %v, want *domain.NotFoundError", err)
	}
	if notFound.Resource != "project" {
		t.Fatalf("resource = %q, want project", notFound.Resource)
	}
}

func TestSubmitBudgetDefaultsTighteningAndCap(t *testing.T) {
	ctx := context.Background()
	policyBudget := domain.Budget{CPUSeconds: 300, WallSeconds: 1800, FuzzCPUSeconds: 50, MaxParallelSteps: 4}

	fixture := newRunFixtureWithLimits(t, Limits{
		DefaultCPUSeconds:     500,
		DefaultWallSeconds:    3600,
		DefaultFuzzCPUSeconds: 100,
		MaxParallelSteps:      8,
		MaxRunWallSeconds:     6000,
		APIListMaxLimit:       200,
	})
	defaults, err := fixture.service.Submit(ctx, fixture.input("budget-defaults"))
	if err != nil {
		t.Fatalf("submit defaults: %v", err)
	}
	if defaults.Budget != policyBudget {
		t.Fatalf("default budget = %+v, want %+v", defaults.Budget, policyBudget)
	}

	tighterInput := fixture.input("budget-tighter")
	tighterInput.Budget = domain.Budget{CPUSeconds: 200, WallSeconds: 900, FuzzCPUSeconds: 10, MaxParallelSteps: 2}
	tighter, err := fixture.service.Submit(ctx, tighterInput)
	if err != nil {
		t.Fatalf("submit tighter: %v", err)
	}
	if tighter.Budget != tighterInput.Budget {
		t.Fatalf("tightened budget = %+v, want %+v", tighter.Budget, tighterInput.Budget)
	}

	looserInput := fixture.input("budget-looser")
	looserInput.Budget = domain.Budget{CPUSeconds: 1000, WallSeconds: 5000, FuzzCPUSeconds: 500, MaxParallelSteps: 10}
	looser, err := fixture.service.Submit(ctx, looserInput)
	if err != nil {
		t.Fatalf("submit looser: %v", err)
	}
	if looser.Budget != policyBudget {
		t.Fatalf("looser budget = %+v, want policy defaults %+v", looser.Budget, policyBudget)
	}

	cappedFixture := newRunFixtureWithLimits(t, Limits{
		DefaultCPUSeconds:     500,
		DefaultWallSeconds:    3600,
		DefaultFuzzCPUSeconds: 100,
		MaxParallelSteps:      8,
		MaxRunWallSeconds:     600,
		APIListMaxLimit:       200,
	})
	capped, err := cappedFixture.service.Submit(ctx, cappedFixture.input("budget-capped"))
	if err != nil {
		t.Fatalf("submit capped: %v", err)
	}
	if capped.Budget.WallSeconds != 600 {
		t.Fatalf("capped wall seconds = %d, want 600", capped.Budget.WallSeconds)
	}
	cappedRequestInput := cappedFixture.input("budget-capped-request")
	cappedRequestInput.Budget = domain.Budget{WallSeconds: 900}
	cappedRequest, err := cappedFixture.service.Submit(ctx, cappedRequestInput)
	if err != nil {
		t.Fatalf("submit capped request: %v", err)
	}
	if cappedRequest.Budget.WallSeconds != 600 {
		t.Fatalf("capped request wall seconds = %d, want 600", cappedRequest.Budget.WallSeconds)
	}
}

func testRequirements() domain.ResourceRequirements {
	return domain.ResourceRequirements{
		Architecture:    "amd64",
		Isolation:       domain.IsolationContainer,
		CPUMilli:        1000,
		MemoryMiB:       1024,
		Network:         domain.NetworkRestricted,
		DeadlineSeconds: 600,
	}
}

func validEvidence(tenantID domain.TenantID, runID domain.RunID) domain.Evidence {
	return domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      tenantID,
		RunID:         runID,
		ClaimID:       "compile",
		Kind:          domain.EvidenceCompile,
		Result:        domain.ClaimTested,
		Scope:         domain.EvidenceScope{InputDomain: "all packages"},
		Engine:        domain.EngineInfo{Name: "go-build", Version: "1.0.0"},
		Environment:   domain.EnvironmentInfo{Arch: "arm64", SandboxProfile: "test"},
		CreatedAt:     runTestNow,
	}
}

func validCounterexample(tenantID domain.TenantID, runID domain.RunID) domain.Counterexample {
	return domain.Counterexample{
		CaseID:             "case-1",
		TenantID:           tenantID,
		RunID:              runID,
		ClaimID:            "compile",
		InputCodec:         "json",
		InputRef:           domain.NewCASRef(strings.Repeat("1", 64), 1),
		BaseObservationRef: domain.NewCASRef(strings.Repeat("2", 64), 1),
		HeadObservationRef: domain.NewCASRef(strings.Repeat("3", 64), 1),
		Reproduced:         true,
		Scope:              "unit",
		Minimized:          true,
		CreatedAt:          runTestNow,
	}
}

// planOneReadyStep moves a freshly submitted CREATED run through VALIDATING and
// installs a single READY compile step.
func planOneReadyStep(t *testing.T, store *memory.Store, run domain.Run) domain.Step {
	t.Helper()
	validating, err := store.UpdateRun(context.Background(), domain.RunUpdate{
		TenantID: run.TenantID, RunID: run.ID, ExpectedVersion: run.StateVersion,
		From: run.Status, To: domain.RunValidating, At: runTestNow,
	})
	if err != nil {
		t.Fatalf("transition to VALIDATING: %v", err)
	}
	return planOneReadyStepFromValidating(t, store, validating)
}

func planOneReadyStepFromValidating(t *testing.T, store *memory.Store, validating domain.Run) domain.Step {
	t.Helper()
	step := domain.Step{
		ID: domain.NewStepID(), RunID: validating.ID, TenantID: validating.TenantID, Key: "compile",
		Kind: domain.StepKindCompile, Status: domain.StepReady, MaxAttempts: 3,
		Requirements: testRequirements(), Priority: 10,
		CreatedAt: runTestNow, UpdatedAt: runTestNow,
	}
	if _, err := store.PlanRun(context.Background(), domain.PlanTransaction{
		TenantID: validating.TenantID, RunID: validating.ID, ExpectedVersion: validating.StateVersion,
		Steps: []domain.Step{step}, At: runTestNow,
	}); err != nil {
		t.Fatalf("plan run: %v", err)
	}
	return step
}

func commitEvidence(t *testing.T, store *memory.Store, run domain.Run, step domain.Step, evidence []domain.Evidence, counterexamples []domain.Counterexample) {
	t.Helper()
	ctx := context.Background()
	lease, err := store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: run.TenantID, RunID: run.ID, StepID: step.ID,
		AttemptID: domain.NewAttemptID(), ExpectedGeneration: 0, Owner: "worker-1",
		TTL: time.Minute, Now: runTestNow,
	})
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	if _, err := store.CommitAttempt(ctx, domain.AttemptCommit{
		Ref: lease.Ref(),
		Result: domain.StepResult{
			Outcome:         domain.OutcomeSucceeded,
			ResultDigest:    "digest",
			Evidence:        evidence,
			Counterexamples: counterexamples,
		},
		At: runTestNow,
	}); err != nil {
		t.Fatalf("commit attempt: %v", err)
	}
}

func TestQueryDelegationScopesByTenant(t *testing.T) {
	fixture := newRunFixture(t)
	ctx := context.Background()
	run, err := fixture.service.Submit(ctx, fixture.input("delegation-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	step := planOneReadyStep(t, fixture.store, run)
	evidence := validEvidence(fixture.tenantID, run.ID)
	counterexample := validCounterexample(fixture.tenantID, run.ID)
	commitEvidence(t, fixture.store, run, step, []domain.Evidence{evidence}, []domain.Counterexample{counterexample})

	report := domain.Report{
		SchemaVersion:   domain.ReportSchemaV1,
		ReportID:        domain.NewReportID(),
		RunID:           run.ID,
		TenantID:        fixture.tenantID,
		ProjectID:       fixture.projectID,
		Verdict:         domain.VerdictVerified,
		BaseCommit:      run.BaseCommit,
		HeadCommit:      run.HeadCommit,
		PolicyDigest:    fixture.policyDigest,
		RequestedChecks: []string{"compile"},
		CompletedChecks: []string{"compile"},
		Claims: []domain.ClaimAssessment{{
			ClaimID: "compile", Kind: domain.ClaimKindCompile, Title: "compile",
			Required: true, MinimumResult: domain.ClaimTested, Result: domain.ClaimTested,
		}},
		Manifest:    domain.NewCASRef(strings.Repeat("4", 64), 10),
		MerkleRoot:  "merkle-root",
		Attestation: domain.AttestationPending,
		GeneratedAt: runTestNow,
	}
	if err := fixture.store.PutReport(ctx, report); err != nil {
		t.Fatalf("put report: %v", err)
	}
	otherTenant := domain.NewTenantID()

	got, err := fixture.service.Get(ctx, fixture.tenantID, run.ID)
	if err != nil || got.ID != run.ID {
		t.Fatalf("get own run: %v (run %+v)", err, got)
	}
	if _, err := fixture.service.Get(ctx, otherTenant, run.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant get error = %v, want not found", err)
	}

	page, err := fixture.service.List(ctx, domain.RunFilter{TenantID: fixture.tenantID})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("own list = %+v, err=%v", page, err)
	}
	otherPage, err := fixture.service.List(ctx, domain.RunFilter{TenantID: otherTenant})
	if err != nil {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if len(otherPage.Runs) != 0 {
		t.Fatalf("cross-tenant list returned %d runs", len(otherPage.Runs))
	}

	dag, err := fixture.service.DAG(ctx, fixture.tenantID, run.ID)
	if err != nil {
		t.Fatalf("dag: %v", err)
	}
	if dag.RunID != run.ID || len(dag.Steps) != 1 {
		t.Fatalf("dag = %+v", dag)
	}
	if _, err := fixture.service.DAG(ctx, otherTenant, run.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant dag error = %v, want not found", err)
	}

	evidencePage, err := fixture.service.Evidence(ctx, fixture.tenantID, run.ID)
	if err != nil || len(evidencePage) != 1 {
		t.Fatalf("evidence = %v, err=%v", evidencePage, err)
	}
	otherEvidence, err := fixture.service.Evidence(ctx, otherTenant, run.ID)
	if err != nil {
		t.Fatalf("cross-tenant evidence: %v", err)
	}
	if len(otherEvidence) != 0 {
		t.Fatalf("cross-tenant evidence returned %d entries", len(otherEvidence))
	}

	counterexamples, err := fixture.service.Counterexamples(ctx, fixture.tenantID, run.ID)
	if err != nil || len(counterexamples) != 1 {
		t.Fatalf("counterexamples = %v, err=%v", counterexamples, err)
	}
	otherCounterexamples, err := fixture.service.Counterexamples(ctx, otherTenant, run.ID)
	if err != nil {
		t.Fatalf("cross-tenant counterexamples: %v", err)
	}
	if len(otherCounterexamples) != 0 {
		t.Fatalf("cross-tenant counterexamples returned %d entries", len(otherCounterexamples))
	}

	gotReport, err := fixture.service.Report(ctx, fixture.tenantID, run.ID)
	if err != nil || gotReport.ReportID != report.ReportID {
		t.Fatalf("report = %+v, err=%v", gotReport, err)
	}
	if _, err := fixture.service.Report(ctx, otherTenant, run.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant report error = %v, want not found", err)
	}
}

func isNotFound(err error) bool {
	var notFound *domain.NotFoundError
	return errors.As(err, &notFound)
}

func TestCancelTransitionsStepsAndIsIdempotent(t *testing.T) {
	fixture := newRunFixture(t)
	ctx := context.Background()
	run, err := fixture.service.Submit(ctx, fixture.input("cancel-1"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	step := planOneReadyStep(t, fixture.store, run)

	cancelling, err := fixture.service.Cancel(ctx, fixture.tenantID, run.ID, "alice")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelling.Status != domain.RunCancelling {
		t.Fatalf("status = %s, want CANCELLING", cancelling.Status)
	}
	storedStep, err := fixture.store.GetStep(ctx, fixture.tenantID, step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if storedStep.Status != domain.StepCancelled {
		t.Fatalf("step status = %s, want CANCELLED", storedStep.Status)
	}

	repeated, err := fixture.service.Cancel(ctx, fixture.tenantID, run.ID, "alice")
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if repeated.Status != domain.RunCancelling {
		t.Fatalf("repeated status = %s, want CANCELLING", repeated.Status)
	}
	if repeated.StateVersion != cancelling.StateVersion {
		t.Fatalf("repeated cancel bumped version: %d -> %d", cancelling.StateVersion, repeated.StateVersion)
	}

	finished, err := fixture.store.UpdateRun(ctx, domain.RunUpdate{
		TenantID: fixture.tenantID, RunID: run.ID, ExpectedVersion: cancelling.StateVersion,
		From: domain.RunCancelling, To: domain.RunCancelled, At: runTestNow,
	})
	if err != nil {
		t.Fatalf("finish cancellation: %v", err)
	}
	// Cancelling an already CANCELLED run is deliberately idempotent.
	alreadyCancelled, err := fixture.service.Cancel(ctx, fixture.tenantID, run.ID, "alice")
	if err != nil {
		t.Fatalf("cancel cancelled run: %v", err)
	}
	if alreadyCancelled.Status != domain.RunCancelled || alreadyCancelled.StateVersion != finished.StateVersion {
		t.Fatalf("cancelled run = %s/%d, want CANCELLED/%d", alreadyCancelled.Status, alreadyCancelled.StateVersion, finished.StateVersion)
	}

	// Any other terminal status is a conflict.
	completed := fixture.input("cancel-terminal")
	completedRun, err := fixture.service.Submit(ctx, completed)
	if err != nil {
		t.Fatalf("submit completed run: %v", err)
	}
	current := completedRun
	for _, next := range []domain.RunStatus{
		domain.RunValidating, domain.RunQueued, domain.RunRunning, domain.RunAggregating, domain.RunCompleted,
	} {
		update := domain.RunUpdate{
			TenantID: fixture.tenantID, RunID: current.ID, ExpectedVersion: current.StateVersion,
			From: current.Status, To: next, At: runTestNow,
		}
		if next == domain.RunValidating {
			// VALIDATING normally installs a plan before leaving the state.
			current, err = fixture.store.UpdateRun(ctx, update)
			if err != nil {
				t.Fatalf("transition to %s: %v", next, err)
			}
			planOneReadyStepFromValidating(t, fixture.store, current)
			current, err = fixture.store.GetRun(ctx, fixture.tenantID, current.ID)
			if err != nil {
				t.Fatalf("reload run: %v", err)
			}
			continue
		}
		if next == domain.RunCompleted {
			update.Verdict = domain.VerdictVerified
		}
		current, err = fixture.store.UpdateRun(ctx, update)
		if err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	_, err = fixture.service.Cancel(ctx, fixture.tenantID, completedRun.ID, "alice")
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("terminal cancel error = %v, want *domain.ConflictError", err)
	}
	if conflict.Resource != "run" {
		t.Fatalf("conflict resource = %q, want run", conflict.Resource)
	}

	if _, err := fixture.service.Cancel(ctx, domain.NewTenantID(), run.ID, "alice"); !isNotFound(err) {
		t.Fatalf("cross-tenant cancel error = %v, want not found", err)
	}
}
