package orchestrator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"sync"
	"testing"
	"time"

	casfs "github.com/patchproof/patchproof/internal/adapters/cas/fs"
	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/app/proof"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/planner"
	"github.com/patchproof/patchproof/internal/ports"
)

var orchestratorTestNow = time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)

const orchestratorPolicyYAML = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: orchestrator-test
spec:
  verification:
    required:
      - compile
      - protected-assets
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
  protectedPaths:
    - secrets/**
`

type orchestratorClock struct{ at time.Time }

func (c orchestratorClock) Now() time.Time { return c.at }

type orchestratorSigner struct{ key ed25519.PrivateKey }

func newOrchestratorSigner() *orchestratorSigner {
	return &orchestratorSigner{key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x24}, ed25519.SeedSize))}
}

func (s *orchestratorSigner) KeyID() string     { return "orchestrator-test-key" }
func (s *orchestratorSigner) Algorithm() string { return "ed25519" }
func (s *orchestratorSigner) Sign(_ context.Context, payload []byte) (domain.Signature, error) {
	return domain.Signature{
		ID:        domain.NewSignatureID(),
		KeyID:     s.KeyID(),
		Algorithm: "ed25519",
		Signature: ed25519.Sign(s.key, payload),
		SignedAt:  orchestratorTestNow,
	}, nil
}

// fakeSnapshotter serves fixed manifests and writes tiny archive blobs through
// the CAS so no git or toolchain is required.
type fakeSnapshotter struct {
	mu        sync.Mutex
	cas       ports.ArtifactStore
	tenantID  domain.TenantID
	region    string
	manifests map[string]domain.SnapshotManifest
	archives  map[string]domain.CASRef
}

func newFakeSnapshotter(cas ports.ArtifactStore, tenantID domain.TenantID, base, head domain.SnapshotManifest) *fakeSnapshotter {
	return &fakeSnapshotter{
		cas: cas, tenantID: tenantID, region: "region-a",
		manifests: map[string]domain.SnapshotManifest{base.Commit: base, head.Commit: head},
		archives:  map[string]domain.CASRef{},
	}
}

func (s *fakeSnapshotter) Snapshot(ctx context.Context, request ports.SnapshotRequest) (ports.SnapshotResult, error) {
	commit := request.Repository.Commit
	s.mu.Lock()
	manifest, ok := s.manifests[commit]
	s.mu.Unlock()
	if !ok {
		manifest = domain.SnapshotManifest{Commit: commit, Repository: request.Repository.URI, Digest: "sha256:" + strings.Repeat("0", 64)}
	}
	ref, err := s.cas.Put(ctx, ports.SecurityScope{
		TenantID: request.TenantID, TrustDomain: "run", Region: s.region,
	}, strings.NewReader("archive:"+commit))
	if err != nil {
		return ports.SnapshotResult{}, err
	}
	s.mu.Lock()
	s.archives[commit] = ref
	s.mu.Unlock()
	return ports.SnapshotResult{Manifest: manifest, Archive: ref, ChangedAt: orchestratorTestNow}, nil
}

func (s *fakeSnapshotter) Health(context.Context) error { return nil }

func (s *fakeSnapshotter) archive(commit string) (domain.CASRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.archives[commit]
	return ref, ok
}

// fakePolicyEngine returns a single canned decision and records admissions.
type fakePolicyEngine struct {
	mu       sync.Mutex
	decision ports.PolicyDecision
	err      error
	inputs   []ports.PolicyInput
}

func (e *fakePolicyEngine) Evaluate(_ context.Context, input ports.PolicyInput) (ports.PolicyDecision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inputs = append(e.inputs, input)
	return e.decision, e.err
}

func (e *fakePolicyEngine) snapshot() []ports.PolicyInput {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ports.PolicyInput(nil), e.inputs...)
}

type orchestratorFixture struct {
	store        *memory.Store
	service      *Service
	proof        *proof.Service
	engine       *fakePolicyEngine
	snapshots    *fakeSnapshotter
	tenantID     domain.TenantID
	projectID    domain.ProjectID
	policy       domain.Policy
	run          domain.Run
	baseManifest domain.SnapshotManifest
	headManifest domain.SnapshotManifest
}

func newOrchestratorFixture(t *testing.T, decision ports.PolicyDecision) *orchestratorFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	cas, err := casfs.New(casfs.Config{Root: t.TempDir(), CreateRoot: true})
	if err != nil {
		t.Fatalf("new filesystem cas: %v", err)
	}
	signer := newOrchestratorSigner()
	proofService, err := proof.New(store, cas, signer, proof.Config{
		Region: "region-a", Version: "test", Clock: orchestratorClock{at: orchestratorTestNow},
	}, nil, nil)
	if err != nil {
		t.Fatalf("new proof service: %v", err)
	}

	tenantID := domain.NewTenantID()
	projectID := domain.NewProjectID()
	if err := store.CreateTenant(ctx, domain.Tenant{
		ID: tenantID, HomeRegion: "region-a", SecurityTier: domain.SecurityTierStandard,
		Status: domain.TenantActive, ActiveWriterEpoch: 1,
		CreatedAt: orchestratorTestNow, UpdatedAt: orchestratorTestNow,
	}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	document, err := domain.ParsePolicyDocument([]byte(orchestratorPolicyYAML))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	digest, err := document.Digest()
	if err != nil {
		t.Fatalf("policy digest: %v", err)
	}
	policy := domain.Policy{
		TenantID: tenantID, Digest: digest, Version: "v1", Source: domain.PolicySourceAdmin,
		Trusted: true, Document: []byte(orchestratorPolicyYAML), CreatedBy: "alice",
		CreatedAt: orchestratorTestNow,
	}
	if err := store.PutPolicy(ctx, policy); err != nil {
		t.Fatalf("put policy: %v", err)
	}
	if err := store.CreateProject(ctx, domain.Project{
		ID: projectID, TenantID: tenantID, Provider: "git",
		RepositoryURI: "https://example.com/acme/app.git", DefaultBranch: "main",
		DefaultPolicyDigest: digest, Classification: "standard",
		CreatedAt: orchestratorTestNow, UpdatedAt: orchestratorTestNow,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	baseCommit := strings.Repeat("a", 40)
	headCommit := strings.Repeat("b", 40)
	baseManifest := domain.SnapshotManifest{
		Digest: "sha256:" + strings.Repeat("1", 64), Commit: baseCommit,
		Repository: "https://example.com/acme/app.git",
		Files: []domain.SnapshotFile{
			{Path: "docs/readme.md", Digest: "sha256:" + strings.Repeat("2", 64), Size: 5, Mode: "100644"},
			{Path: "src/main.go", Digest: "sha256:" + strings.Repeat("3", 64), Size: 10, Mode: "100644"},
		},
		TotalBytes: 15, FileCount: 2, FetchedAt: orchestratorTestNow,
	}
	headManifest := domain.SnapshotManifest{
		Digest: "sha256:" + strings.Repeat("4", 64), Commit: headCommit,
		Repository: "https://example.com/acme/app.git",
		Files: []domain.SnapshotFile{
			{Path: "docs/readme.md", Digest: "sha256:" + strings.Repeat("2", 64), Size: 5, Mode: "100644"},
			{Path: "src/main.go", Digest: "sha256:" + strings.Repeat("5", 64), Size: 12, Mode: "100644"},
			{Path: "src/new.go", Digest: "sha256:" + strings.Repeat("6", 64), Size: 3, Mode: "100644"},
		},
		TotalBytes: 20, FileCount: 3, FetchedAt: orchestratorTestNow,
	}
	snapshots := newFakeSnapshotter(cas, tenantID, baseManifest, headManifest)

	plannerInstance, err := planner.New(4, planner.Limits{
		MaxStepCount: 64, MaxParallelSteps: 4, MaxStepWallSeconds: 600,
		MaxRunWallSeconds: 1800, DefaultIsolation: domain.IsolationContainer, Architecture: "amd64",
	})
	if err != nil {
		t.Fatalf("new planner: %v", err)
	}
	engine := &fakePolicyEngine{decision: decision}
	service, err := New(store, plannerInstance, engine, snapshots, cas, proofService, Config{
		Region: "region-a", Version: "test", PollInterval: 10 * time.Millisecond,
		ValidationWorkers: 2, MaxRepositoriesBytes: 1 << 30, ShardCount: 4,
		Clock: orchestratorClock{at: orchestratorTestNow},
	}, nil)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	run := domain.Run{
		ID: domain.NewRunID(), TenantID: tenantID, ProjectID: projectID,
		RequestKey: "orchestrator-" + domain.NewRunID().String(), Fingerprint: "fingerprint",
		BaseCommit: baseCommit, HeadCommit: headCommit, PolicyDigest: digest,
		RequestedChecks: []string{"compile"},
		Budget:          domain.Budget{CPUSeconds: 300, WallSeconds: 1800, FuzzCPUSeconds: 50, MaxParallelSteps: 4},
		Status:          domain.RunValidating, Region: "region-a",
		CreatedAt: orchestratorTestNow, UpdatedAt: orchestratorTestNow,
	}
	if _, err := store.CreateRun(ctx, domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return &orchestratorFixture{
		store: store, service: service, proof: proofService, engine: engine,
		snapshots: snapshots, tenantID: tenantID, projectID: projectID,
		policy: policy, run: run, baseManifest: baseManifest, headManifest: headManifest,
	}
}

func (f *orchestratorFixture) waitForStatus(t *testing.T, want domain.RunStatus) domain.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last domain.Run
	for time.Now().Before(deadline) {
		run, err := f.store.GetRun(context.Background(), f.tenantID, f.run.ID)
		if err == nil {
			last = run
			if run.Status == want {
				return run
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run did not reach %s within 5s; last status %s", want, last.Status)
	return domain.Run{}
}

// runUntil starts Run, waits for the requested status, and then shuts the
// loop down.
func (f *orchestratorFixture) runUntil(t *testing.T, want domain.RunStatus) domain.Run {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.service.Run(ctx) }()
	run := f.waitForStatus(t, want)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("orchestrator Run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrator Run did not stop within 5s")
	}
	return run
}

func allowedDecision(required ...string) ports.PolicyDecision {
	return ports.PolicyDecision{
		Allowed:           true,
		RequiredChecks:    required,
		Isolation:         domain.IsolationContainer,
		SubjectNetwork:    domain.NetworkNone,
		ArtifactTransport: domain.TransportDirect,
		Digest:            "decision-digest",
	}
}

func TestRunPlansValidationThroughFullTick(t *testing.T) {
	fixture := newOrchestratorFixture(t, allowedDecision("compile", "protected-assets"))
	run := fixture.runUntil(t, domain.RunQueued)

	if run.Verdict != domain.VerdictNone {
		t.Fatalf("verdict = %q, want empty before aggregation", run.Verdict)
	}
	if run.PolicyDigest != fixture.policy.Digest {
		t.Fatalf("policy digest = %q, want %q", run.PolicyDigest, fixture.policy.Digest)
	}

	expectedDigest, err := snapshotDigest(fixture.baseManifest.Digest, fixture.headManifest.Digest)
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	if run.SnapshotDigest != expectedDigest {
		t.Fatalf("frozen snapshot digest = %q, want %q", run.SnapshotDigest, expectedDigest)
	}
	baseArchive, ok := fixture.snapshots.archive(fixture.baseManifest.Commit)
	if !ok {
		t.Fatal("base archive was not written to the CAS")
	}
	headArchive, ok := fixture.snapshots.archive(fixture.headManifest.Commit)
	if !ok {
		t.Fatal("head archive was not written to the CAS")
	}
	if run.BaseSnapshot != baseArchive.URI() {
		t.Fatalf("base snapshot = %q, want %q", run.BaseSnapshot, baseArchive.URI())
	}
	if run.HeadSnapshot != headArchive.URI() {
		t.Fatalf("head snapshot = %q, want %q", run.HeadSnapshot, headArchive.URI())
	}

	dag, err := fixture.store.GetDAG(context.Background(), fixture.tenantID, run.ID)
	if err != nil {
		t.Fatalf("get dag: %v", err)
	}
	if len(dag.Steps) == 0 {
		t.Fatal("planned DAG has no steps")
	}
	if len(dag.Claims) == 0 {
		t.Fatal("planned DAG has no claims")
	}
	steps := map[string]domain.Step{}
	for _, step := range dag.Steps {
		steps[step.Key] = step
	}
	for _, key := range []string{"analyze", "compile-base", "compile-head"} {
		step, ok := steps[key]
		if !ok {
			t.Fatalf("missing root step %q (steps: %v)", key, steps)
		}
		if step.Status != domain.StepReady {
			t.Fatalf("root step %q status = %s, want READY", key, step.Status)
		}
	}
	impact, ok := steps["impact"]
	if !ok {
		t.Fatal("missing impact step")
	}
	if impact.Status != domain.StepBlocked {
		t.Fatalf("impact status = %s, want BLOCKED", impact.Status)
	}
	analyze, ok := steps["analyze"]
	if !ok {
		t.Fatal("missing analyze step")
	}
	dependencies := dag.Dependencies[impact.ID]
	if len(dependencies) != 1 || dependencies[0] != analyze.ID {
		t.Fatalf("impact dependencies = %v, want [%s]", dependencies, analyze.ID)
	}

	claimIDs := map[domain.ClaimID]domain.Claim{}
	for _, claim := range dag.Claims {
		claimIDs[claim.ID] = claim
	}
	for _, id := range []domain.ClaimID{"compile", "protected-assets"} {
		claim, ok := claimIDs[id]
		if !ok {
			t.Fatalf("missing claim %q (claims: %v)", id, claimIDs)
		}
		if !claim.Required {
			t.Fatalf("claim %q is not required", id)
		}
	}

	// The policy engine is consulted twice: before and after snapshotting.
	inputs := fixture.engine.snapshot()
	if len(inputs) != 2 {
		t.Fatalf("policy evaluations = %d, want 2", len(inputs))
	}
	if inputs[0].ChangedFiles != 0 || len(inputs[0].ChangedPaths) != 0 {
		t.Fatalf("first admission already had changes: %+v", inputs[0])
	}
	wantChanged := []string{"src/main.go", "src/new.go"}
	if len(inputs[1].ChangedPaths) != len(wantChanged) {
		t.Fatalf("changed paths = %v, want %v", inputs[1].ChangedPaths, wantChanged)
	}
	for i := range wantChanged {
		if inputs[1].ChangedPaths[i] != wantChanged[i] {
			t.Fatalf("changed paths = %v, want %v", inputs[1].ChangedPaths, wantChanged)
		}
	}
	if inputs[1].ChangedFiles != 2 {
		t.Fatalf("changed files = %d, want 2", inputs[1].ChangedFiles)
	}

	// The protected-assets claim is evaluated against the changed paths and
	// recorded as policy evidence.
	evidenceList, err := fixture.store.ListEvidence(context.Background(), fixture.tenantID, run.ID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(evidenceList) != 1 {
		t.Fatalf("policy evidence = %d entries, want 1", len(evidenceList))
	}
	if evidenceList[0].Kind != domain.EvidencePolicy || evidenceList[0].Result != domain.ClaimTested {
		t.Fatalf("policy evidence = %+v", evidenceList[0])
	}
}

func TestRunRejectsOnPolicyDecision(t *testing.T) {
	decision := allowedDecision("compile", "protected-assets")
	decision.Allowed = false
	decision.Violations = []ports.PolicyViolation{{
		Code: "PROTECTED_PATH_MODIFIED", Severity: domain.SeverityHigh,
		Message: "patch modifies protected paths: secrets/token",
	}}
	fixture := newOrchestratorFixture(t, decision)
	run := fixture.runUntil(t, domain.RunRejected)

	if run.Verdict != domain.VerdictPolicyBlocked {
		t.Fatalf("verdict = %s, want POLICY_BLOCKED", run.Verdict)
	}
	report, err := fixture.store.GetReport(context.Background(), fixture.tenantID, run.ID)
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if report.Verdict != domain.VerdictPolicyBlocked {
		t.Fatalf("report verdict = %s, want POLICY_BLOCKED", report.Verdict)
	}
	if len(report.Claims) != 1 {
		t.Fatalf("report claims = %d, want 1", len(report.Claims))
	}
	if err := report.Manifest.Validate(); err != nil {
		t.Fatalf("report manifest: %v", err)
	}
	if report.Attestation != domain.AttestationSigned || len(report.Signatures) != 1 {
		t.Fatalf("report attestation = %s with %d signatures", report.Attestation, len(report.Signatures))
	}
	var found bool
	for _, finding := range report.Findings {
		if finding.Code == "POLICY_BLOCKED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report findings = %+v, want POLICY_BLOCKED", report.Findings)
	}
	if inputs := fixture.engine.snapshot(); len(inputs) != 1 {
		t.Fatalf("policy evaluations = %d, want 1 before rejection", len(inputs))
	}
	if _, ok := fixture.snapshots.archive(fixture.baseManifest.Commit); ok {
		t.Fatal("snapshotter was called before the rejection")
	}
}

func TestChangedPaths(t *testing.T) {
	base := domain.SnapshotManifest{Files: []domain.SnapshotFile{
		{Path: "a.go", Digest: "d1"},
		{Path: "b.go", Digest: "d2"},
		{Path: "c.go", Digest: "d3"},
	}}
	head := domain.SnapshotManifest{Files: []domain.SnapshotFile{
		{Path: "a.go", Digest: "d1"},
		{Path: "b.go", Digest: "changed"},
		{Path: "d.go", Digest: "d4"},
	}}
	got := changedPaths(base, head)
	want := []string{"b.go", "c.go", "d.go"}
	if len(got) != len(want) {
		t.Fatalf("changed paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("changed paths = %v, want %v", got, want)
		}
	}
	if changed := changedPaths(base, base); len(changed) != 0 {
		t.Fatalf("identical snapshots reported changes: %v", changed)
	}
	if changed := changedPaths(domain.SnapshotManifest{}, domain.SnapshotManifest{}); len(changed) != 0 {
		t.Fatalf("empty snapshots reported changes: %v", changed)
	}
}

func TestNeedsPartial(t *testing.T) {
	base := domain.Report{
		Attestation:     domain.AttestationSigned,
		Claims:          []domain.ClaimAssessment{{ClaimID: "compile", Required: true, MinimumResult: domain.ClaimTested, Result: domain.ClaimTested}},
		RequestedChecks: []string{"compile"},
		CompletedChecks: []string{"compile"},
	}
	if needsPartial(base, false) {
		t.Fatal("satisfied report was marked partial")
	}
	if needsPartial(base, true) {
		t.Fatal("signed report was marked partial with require-attestation")
	}

	unsigned := base
	unsigned.Attestation = domain.AttestationPending
	if !needsPartial(unsigned, true) {
		t.Fatal("unsigned report was not marked partial with require-attestation")
	}
	if needsPartial(unsigned, false) {
		t.Fatal("unsigned report was marked partial without require-attestation")
	}

	unknown := base
	unknown.Claims = []domain.ClaimAssessment{{ClaimID: "compile", Required: true, MinimumResult: domain.ClaimTested, Result: domain.ClaimUnknown}}
	if !needsPartial(unknown, false) {
		t.Fatal("required UNKNOWN claim was not marked partial")
	}

	missingCheck := base
	missingCheck.RequestedChecks = []string{"compile", "security-scan"}
	if !needsPartial(missingCheck, false) {
		t.Fatal("uncompleted requested check was not marked partial")
	}

	policyCheck := base
	policyCheck.RequestedChecks = []string{"compile", "protected-assets"}
	if needsPartial(policyCheck, false) {
		t.Fatal("protected-assets should be exempt from partial checks")
	}
}

func TestRejectionCode(t *testing.T) {
	if code := rejectionCode(domain.VerdictPolicyBlocked); code != "POLICY_BLOCKED" {
		t.Fatalf("policy blocked code = %q", code)
	}
	for _, verdict := range []domain.Verdict{domain.VerdictInconclusive, domain.VerdictInfraFailed} {
		if code := rejectionCode(verdict); code != "REJECTED" {
			t.Fatalf("%s code = %q, want REJECTED", verdict, code)
		}
	}
}

func TestSnapshotDigestIsDeterministic(t *testing.T) {
	first, err := snapshotDigest("base", "head")
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	second, err := snapshotDigest("base", "head")
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	if first != second {
		t.Fatalf("snapshot digest is not deterministic: %q vs %q", first, second)
	}
	other, err := snapshotDigest("head", "base")
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	if first == other {
		t.Fatal("snapshot digest does not depend on ordering")
	}
}

func TestRunWatchdogTerminatesOverdueRun(t *testing.T) {
	fixture := newOrchestratorFixture(t, ports.PolicyDecision{Allowed: true})
	ctx := context.Background()

	overdue := fixture.run
	overdue.ID = domain.NewRunID()
	overdue.RequestKey = "overdue-" + overdue.ID.String()
	overdue.CreatedAt = orchestratorTestNow.Add(-2 * time.Hour)
	overdue.UpdatedAt = overdue.CreatedAt
	overdue.Budget.WallSeconds = 60
	created, err := fixture.store.CreateRun(ctx, domain.CreateRunRecord{Run: overdue})
	if err != nil {
		t.Fatalf("create overdue run: %v", err)
	}

	if err := fixture.service.process(ctx, created); err != nil {
		t.Fatalf("process overdue run: %v", err)
	}
	updated, err := fixture.store.GetRun(ctx, created.TenantID, created.ID)
	if err != nil {
		t.Fatalf("get overdue run: %v", err)
	}
	if updated.Status != domain.RunInfraFailed {
		t.Fatalf("status = %s, want INFRA_FAILED", updated.Status)
	}
	if updated.Verdict != domain.VerdictInfraFailed {
		t.Fatalf("verdict = %s, want INFRA_FAILED", updated.Verdict)
	}
	if updated.FailureClass != domain.ErrTimeout {
		t.Fatalf("failure class = %s, want TIMEOUT", updated.FailureClass)
	}
	report, err := fixture.store.GetReport(ctx, created.TenantID, created.ID)
	if err != nil {
		t.Fatalf("timeout run has no report: %v", err)
	}
	if report.Verdict != domain.VerdictInfraFailed {
		t.Fatalf("report verdict = %s, want INFRA_FAILED", report.Verdict)
	}
}
