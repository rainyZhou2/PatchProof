package proof

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

var proofTestNow = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

const proofPolicyYAML = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: proof-test
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

type proofClock struct{ at time.Time }

func (c proofClock) Now() time.Time { return c.at }

// proofSigner is a deterministic Ed25519 signer: the key is derived from a
// fixed seed and the signature timestamp is pinned.
type proofSigner struct {
	key   ed25519.PrivateKey
	keyID string
	at    time.Time
}

func newProofSigner() *proofSigner {
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	return &proofSigner{key: ed25519.NewKeyFromSeed(seed), keyID: "proof-test-key", at: proofTestNow}
}

func (s *proofSigner) KeyID() string     { return s.keyID }
func (s *proofSigner) Algorithm() string { return "ed25519" }
func (s *proofSigner) Sign(_ context.Context, payload []byte) (domain.Signature, error) {
	return domain.Signature{
		ID:        domain.NewSignatureID(),
		KeyID:     s.keyID,
		Algorithm: "ed25519",
		Signature: ed25519.Sign(s.key, payload),
		SignedAt:  s.at,
	}, nil
}

func (s *proofSigner) PublicKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

type failingProofSigner struct{ err error }

func (s *failingProofSigner) KeyID() string     { return "failing-key" }
func (s *failingProofSigner) Algorithm() string { return "ed25519" }
func (s *failingProofSigner) Sign(context.Context, []byte) (domain.Signature, error) {
	return domain.Signature{}, s.err
}

// proofBlobSource adapts the memory CAS to evidence.BlobSource using the
// namespace that the proof service writes reports under.
type proofBlobSource struct {
	cas      *casmemory.Store
	tenantID domain.TenantID
	region   string
}

func (s *proofBlobSource) Open(ctx context.Context, ref domain.CASRef) (io.ReadCloser, error) {
	return s.cas.Open(ctx, ports.SecurityScope{TenantID: s.tenantID, TrustDomain: "report", Region: s.region}, ref)
}

type proofFixture struct {
	store     *memory.Store
	cas       *casmemory.Store
	signer    *proofSigner
	service   *Service
	tenantID  domain.TenantID
	projectID domain.ProjectID
	run       domain.Run
	step      domain.Step
}

func proofRequirements() domain.ResourceRequirements {
	return domain.ResourceRequirements{
		Architecture:    "amd64",
		Isolation:       domain.IsolationContainer,
		CPUMilli:        1000,
		MemoryMiB:       1024,
		Network:         domain.NetworkRestricted,
		DeadlineSeconds: 600,
	}
}

func newProofClaim(run domain.Run, id string, kind domain.ClaimKind, required bool) domain.Claim {
	return domain.Claim{
		ID:            domain.ClaimID(id),
		RunID:         run.ID,
		TenantID:      run.TenantID,
		Kind:          kind,
		Title:         "claim " + id,
		Predicate:     "predicate " + id,
		Domain:        "domain " + id,
		MinimumResult: domain.ClaimTested,
		Required:      required,
		CreatedAt:     proofTestNow,
	}
}

// newProofFixture creates a planned run with the supplied claims and one
// READY compile step backed by the in-memory metadata store and CAS.
func newProofFixture(t *testing.T, claims []domain.Claim, stepKind domain.StepKind) *proofFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	cas := casmemory.New()
	signer := newProofSigner()
	service, err := New(store, cas, signer, Config{
		Region: "region-a", Version: "test", Clock: proofClock{at: proofTestNow},
	}, nil, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	tenantID := domain.NewTenantID()
	projectID := domain.NewProjectID()
	document, err := domain.ParsePolicyDocument([]byte(proofPolicyYAML))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	digest, err := document.Digest()
	if err != nil {
		t.Fatalf("policy digest: %v", err)
	}
	if err := store.PutPolicy(ctx, domain.Policy{
		TenantID: tenantID, Digest: digest, Version: "v1",
		Source: domain.PolicySourceAdmin, Trusted: true,
		Document: []byte(proofPolicyYAML), CreatedBy: "alice", CreatedAt: proofTestNow,
	}); err != nil {
		t.Fatalf("put policy: %v", err)
	}

	run := domain.Run{
		ID: domain.NewRunID(), TenantID: tenantID, ProjectID: projectID,
		RequestKey: domain.NewRunID().String(), Fingerprint: "fingerprint",
		BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40),
		PolicyDigest: digest, RequestedChecks: []string{"compile"},
		Budget: domain.Budget{CPUSeconds: 300, WallSeconds: 1800, FuzzCPUSeconds: 50, MaxParallelSteps: 4},
		Status: domain.RunValidating, Region: "region-a",
		CreatedAt: proofTestNow, UpdatedAt: proofTestNow,
	}
	if _, err := store.CreateRun(ctx, domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for i := range claims {
		claims[i].RunID = run.ID
		claims[i].TenantID = tenantID
		if claims[i].CreatedAt.IsZero() {
			claims[i].CreatedAt = proofTestNow
		}
	}
	step := domain.Step{
		ID: domain.NewStepID(), RunID: run.ID, TenantID: tenantID, Key: "compile",
		Kind: stepKind, Status: domain.StepReady, MaxAttempts: 3,
		Requirements: proofRequirements(), Priority: 10,
		CreatedAt: proofTestNow, UpdatedAt: proofTestNow,
	}
	if _, err := store.PlanRun(ctx, domain.PlanTransaction{
		TenantID: tenantID, RunID: run.ID, ExpectedVersion: 0,
		Steps: []domain.Step{step}, Claims: claims, At: proofTestNow,
	}); err != nil {
		t.Fatalf("plan run: %v", err)
	}
	refreshed, err := store.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	return &proofFixture{
		store: store, cas: cas, signer: signer, service: service,
		tenantID: tenantID, projectID: projectID, run: refreshed, step: step,
	}
}

func (f *proofFixture) source() *proofBlobSource {
	return &proofBlobSource{cas: f.cas, tenantID: f.tenantID, region: "region-a"}
}

func (f *proofFixture) commit(t *testing.T, result domain.StepResult) {
	t.Helper()
	ctx := context.Background()
	lease, err := f.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: f.tenantID, RunID: f.run.ID, StepID: f.step.ID,
		AttemptID: domain.NewAttemptID(), ExpectedGeneration: 0, Owner: "worker-1",
		TTL: time.Minute, Now: proofTestNow,
	})
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	if result.Outcome == "" {
		result.Outcome = domain.OutcomeSucceeded
	}
	if result.ResultDigest == "" {
		result.ResultDigest = "result-digest"
	}
	if result.FinishedAt.IsZero() {
		result.FinishedAt = proofTestNow
	}
	if _, err := f.store.CommitAttempt(ctx, domain.AttemptCommit{Ref: lease.Ref(), Result: result, At: proofTestNow}); err != nil {
		t.Fatalf("commit attempt: %v", err)
	}
}

func testProofEvidence(run domain.Run, claimID string, kind domain.EvidenceKind, result domain.ClaimResult) domain.Evidence {
	item := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            domain.NewEvidenceID(),
		TenantID:      run.TenantID,
		RunID:         run.ID,
		ClaimID:       domain.ClaimID(claimID),
		Kind:          kind,
		Result:        result,
		Scope:         domain.EvidenceScope{InputDomain: "declared claim domain"},
		Engine:        domain.EngineInfo{Name: "go-build", Version: "1.0.0"},
		Environment:   domain.EnvironmentInfo{Arch: "arm64", SandboxProfile: "test"},
		CreatedAt:     proofTestNow,
	}
	if result == domain.ClaimViolated {
		item.Artifacts = []domain.CASRef{domain.NewCASRef(strings.Repeat("d", 64), 4)}
	}
	return item
}

func testProofCounterexample(run domain.Run, claimID string) domain.Counterexample {
	return domain.Counterexample{
		CaseID:             "case-1",
		TenantID:           run.TenantID,
		RunID:              run.ID,
		ClaimID:            domain.ClaimID(claimID),
		InputCodec:         "json",
		InputRef:           domain.NewCASRef(strings.Repeat("1", 64), 1),
		BaseObservationRef: domain.NewCASRef(strings.Repeat("2", 64), 1),
		HeadObservationRef: domain.NewCASRef(strings.Repeat("3", 64), 1),
		Reproduced:         true,
		Scope:              "declared claim domain",
		Minimized:          true,
		CreatedAt:          proofTestNow,
	}
}

func readBlob(t *testing.T, source *proofBlobSource, ref domain.CASRef) []byte {
	t.Helper()
	reader, err := source.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref.URI(), err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read %s: %v", ref.URI(), err)
	}
	return data
}

func TestAggregateVerifiedReport(t *testing.T) {
	ctx := context.Background()
	fixture := newProofFixture(t, []domain.Claim{
		newProofClaim(domain.Run{}, "compile", domain.ClaimKindCompile, true),
		newProofClaim(domain.Run{}, "regression", domain.ClaimKindTest, false),
	}, domain.StepKindCompile)
	evidenceItem := testProofEvidence(fixture.run, "compile", domain.EvidenceCompile, domain.ClaimTested)
	fixture.commit(t, domain.StepResult{Evidence: []domain.Evidence{evidenceItem}})

	report, err := fixture.service.Aggregate(ctx, fixture.run)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report.Verdict != domain.VerdictVerified {
		t.Fatalf("verdict = %s, want VERIFIED_WITHIN_SCOPE", report.Verdict)
	}
	if report.Attestation != domain.AttestationSigned || len(report.Signatures) != 1 {
		t.Fatalf("attestation = %s with %d signatures, want SIGNED with 1", report.Attestation, len(report.Signatures))
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("report.Validate: %v", err)
	}
	if err := report.Manifest.Validate(); err != nil {
		t.Fatalf("manifest ref: %v", err)
	}
	if len(report.Claims) != 2 {
		t.Fatalf("claims = %d, want 2", len(report.Claims))
	}
	byID := map[domain.ClaimID]domain.ClaimAssessment{}
	for _, claim := range report.Claims {
		byID[claim.ClaimID] = claim
	}
	if byID["compile"].Result != domain.ClaimTested {
		t.Fatalf("compile claim = %s, want TESTED_WITHIN_SCOPE", byID["compile"].Result)
	}
	if byID["regression"].Result != domain.ClaimNotRun {
		t.Fatalf("optional claim = %s, want NOT_RUN", byID["regression"].Result)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", report.Findings)
	}

	// The manifest opens and every leaf resolves to canonical bytes whose
	// digest matches the manifest entry.
	source := fixture.source()
	manifestData := readBlob(t, source, report.Manifest)
	var manifest evidence.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	leaves := map[string]evidence.Leaf{}
	for _, leaf := range manifest.Leaves {
		leaves[leaf.Path] = leaf
	}
	for _, required := range []string{"run.json", "claims.json", "evidence/" + evidenceItem.ID.String() + ".json"} {
		leaf, ok := leaves[required]
		if !ok {
			t.Fatalf("manifest missing required leaf %q (leaves: %v)", required, leaves)
		}
		data := readBlob(t, source, domain.NewCASRef(leaf.Digest, leaf.Size))
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != leaf.Digest || int64(len(data)) != leaf.Size {
			t.Fatalf("leaf %q does not match its manifest entry", required)
		}
		if required != "run.json" && required != "claims.json" {
			var stored domain.Evidence
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatalf("decode evidence leaf: %v", err)
			}
			if err := stored.Validate(); err != nil {
				t.Fatalf("evidence leaf invalid: %v", err)
			}
		}
	}

	// A trust store seeded with the signer's public key verifies the report,
	// including deep verification of every leaf blob.
	trust := evidence.NewTrustStore()
	if err := trust.Add(evidence.TrustedKey{
		KeyID: fixture.signer.KeyID(), Identity: "proof-test", PublicKey: fixture.signer.PublicKey(),
	}); err != nil {
		t.Fatalf("add trusted key: %v", err)
	}
	result, err := evidence.VerifyReport(ctx, report, source, evidence.VerifyOptions{
		Trust: trust, Now: proofTestNow.Add(time.Minute), Deep: true,
	})
	if err != nil {
		t.Fatalf("VerifyReport: %v", err)
	}
	if result.Status != evidence.StatusValidIntegrity {
		t.Fatalf("verification status = %s, want VALID_INTEGRITY (issues: %v)", result.Status, result.Issues)
	}
}

func TestAggregateCounterexampleVerdict(t *testing.T) {
	ctx := context.Background()
	fixture := newProofFixture(t, []domain.Claim{
		newProofClaim(domain.Run{}, "compile", domain.ClaimKindCompile, true),
	}, domain.StepKindCompile)
	evidenceItem := testProofEvidence(fixture.run, "compile", domain.EvidenceCompile, domain.ClaimViolated)
	counterexample := testProofCounterexample(fixture.run, "compile")
	fixture.commit(t, domain.StepResult{
		Evidence:        []domain.Evidence{evidenceItem},
		Counterexamples: []domain.Counterexample{counterexample},
	})

	report, err := fixture.service.Aggregate(ctx, fixture.run)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report.Verdict != domain.VerdictCounterexample {
		t.Fatalf("verdict = %s, want COUNTEREXAMPLE_FOUND", report.Verdict)
	}
	if len(report.Counterexamples) != 1 || report.Counterexamples[0].CaseID != "case-1" {
		t.Fatalf("counterexamples = %+v", report.Counterexamples)
	}
	if len(report.Claims) != 1 || report.Claims[0].Result != domain.ClaimViolated {
		t.Fatalf("claims = %+v, want one VIOLATED claim", report.Claims)
	}
	var found bool
	for _, finding := range report.Findings {
		if finding.Code == "COMPILATION_FAILED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want COMPILATION_FAILED", report.Findings)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("report.Validate: %v", err)
	}
}

func TestAggregatePolicyBlocked(t *testing.T) {
	ctx := context.Background()
	fixture := newProofFixture(t, []domain.Claim{
		newProofClaim(domain.Run{}, "compile", domain.ClaimKindCompile, true),
		newProofClaim(domain.Run{}, "protected-assets", domain.ClaimKindSecurity, true),
	}, domain.StepKindCompile)
	compileEvidence := testProofEvidence(fixture.run, "compile", domain.EvidenceCompile, domain.ClaimTested)
	policyEvidence := testProofEvidence(fixture.run, "protected-assets", domain.EvidencePolicy, domain.ClaimViolated)
	fixture.commit(t, domain.StepResult{Evidence: []domain.Evidence{compileEvidence, policyEvidence}})

	report, err := fixture.service.Aggregate(ctx, fixture.run)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report.Verdict != domain.VerdictPolicyBlocked {
		t.Fatalf("verdict = %s, want POLICY_BLOCKED", report.Verdict)
	}
	var found bool
	for _, finding := range report.Findings {
		if finding.Code == "POLICY_BLOCKED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want POLICY_BLOCKED", report.Findings)
	}
	byID := map[domain.ClaimID]domain.ClaimAssessment{}
	for _, claim := range report.Claims {
		byID[claim.ClaimID] = claim
	}
	if byID["protected-assets"].Result != domain.ClaimViolated {
		t.Fatalf("protected-assets claim = %s, want VIOLATED", byID["protected-assets"].Result)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("report.Validate: %v", err)
	}
}

func TestBuildRejectionReport(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		verdict    domain.Verdict
		code       string
		wantResult domain.ClaimResult
	}{
		{name: "policy blocked", verdict: domain.VerdictPolicyBlocked, code: "POLICY_BLOCKED", wantResult: domain.ClaimViolated},
		{name: "cancelled", verdict: domain.VerdictCancelled, code: "CANCELLED", wantResult: domain.ClaimUnknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newProofFixture(t, []domain.Claim{
				newProofClaim(domain.Run{}, "compile", domain.ClaimKindCompile, true),
			}, domain.StepKindCompile)
			report, err := fixture.service.BuildRejectionReport(ctx, fixture.run, testCase.verdict, testCase.code, "summary", "reason")
			if err != nil {
				t.Fatalf("build rejection report: %v", err)
			}
			if report.Verdict != testCase.verdict {
				t.Fatalf("verdict = %s, want %s", report.Verdict, testCase.verdict)
			}
			if len(report.Claims) != 1 {
				t.Fatalf("claims = %d, want 1", len(report.Claims))
			}
			if report.Claims[0].ClaimID != "policy-admission" {
				t.Fatalf("claim id = %q, want policy-admission", report.Claims[0].ClaimID)
			}
			if report.Claims[0].Result != testCase.wantResult {
				t.Fatalf("claim result = %s, want %s", report.Claims[0].Result, testCase.wantResult)
			}
			if report.Attestation != domain.AttestationSigned || len(report.Signatures) != 1 {
				t.Fatalf("attestation = %s with %d signatures", report.Attestation, len(report.Signatures))
			}
			if err := report.Manifest.Validate(); err != nil {
				t.Fatalf("manifest = %+v: %v", report.Manifest, err)
			}
			if err := report.Validate(); err != nil {
				t.Fatalf("report.Validate: %v", err)
			}
			statement, err := evidence.StatementFromReport(report)
			if err != nil {
				t.Fatalf("statement: %v", err)
			}
			payload, err := statement.Bytes()
			if err != nil {
				t.Fatalf("statement bytes: %v", err)
			}
			if !ed25519.Verify(fixture.signer.PublicKey(), payload, report.Signatures[0].Signature) {
				t.Fatal("report signature did not verify")
			}
			stored, err := fixture.store.GetReport(ctx, fixture.tenantID, fixture.run.ID)
			if err != nil {
				t.Fatalf("stored report: %v", err)
			}
			if stored.ReportID != report.ReportID {
				t.Fatalf("stored report id = %s, want %s", stored.ReportID, report.ReportID)
			}
		})
	}
}

func TestAggregateSigningFailureKeepsPendingAttestation(t *testing.T) {
	ctx := context.Background()
	fixture := newProofFixture(t, []domain.Claim{
		newProofClaim(domain.Run{}, "compile", domain.ClaimKindCompile, true),
	}, domain.StepKindCompile)
	evidenceItem := testProofEvidence(fixture.run, "compile", domain.EvidenceCompile, domain.ClaimTested)
	fixture.commit(t, domain.StepResult{Evidence: []domain.Evidence{evidenceItem}})

	signer := &failingProofSigner{err: errors.New("hsm unavailable")}
	service, err := New(fixture.store, fixture.cas, signer, Config{
		Region: "region-a", Version: "test", Clock: proofClock{at: proofTestNow},
	}, nil, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	report, err := service.Aggregate(ctx, fixture.run)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report.Attestation != domain.AttestationPending {
		t.Fatalf("attestation = %s, want PENDING_ATTESTATION", report.Attestation)
	}
	var pending bool
	for _, finding := range report.Findings {
		if finding.Code == "ATTESTATION_PENDING" {
			pending = true
		}
	}
	if !pending {
		t.Fatalf("findings = %+v, want ATTESTATION_PENDING", report.Findings)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("report.Validate: %v", err)
	}
	stored, err := fixture.store.GetReport(ctx, fixture.tenantID, fixture.run.ID)
	if err != nil {
		t.Fatalf("stored report: %v", err)
	}
	if stored.Attestation != domain.AttestationPending {
		t.Fatalf("stored attestation = %s, want PENDING_ATTESTATION", stored.Attestation)
	}
}

func TestNewValidation(t *testing.T) {
	store := memory.New()
	cas := casmemory.New()
	signer := newProofSigner()
	if _, err := New(nil, cas, signer, Config{}, nil, nil); err == nil {
		t.Fatal("New accepted a nil metadata store")
	}
	if _, err := New(store, nil, signer, Config{}, nil, nil); err == nil {
		t.Fatal("New accepted a nil artifact store")
	}
	if _, err := New(store, cas, nil, Config{RequireAttestation: true}, nil, nil); err == nil {
		t.Fatal("New accepted missing signer while attestation is required")
	}
	if _, err := New(store, cas, nil, Config{}, nil, nil); err != nil {
		t.Fatalf("New with no signer: %v", err)
	}
}

func TestDowngradePartialClaimEvidence(t *testing.T) {
	dag := domain.DAG{Steps: []domain.Step{{
		Status:  domain.StepFailed,
		Options: map[string]string{"claim.id": "native-fuzz"},
	}}}
	verdict := domain.VerdictResult{
		Verdict: domain.VerdictVerified,
		Claims: []domain.ClaimAssessment{{
			ClaimID: "native-fuzz", Required: true,
			MinimumResult: domain.ClaimTested, Result: domain.ClaimTested,
		}},
	}
	downgraded := downgradePartialClaimEvidence(dag, verdict)
	if downgraded.Verdict != domain.VerdictInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE", downgraded.Verdict)
	}
	if downgraded.Claims[0].Result != domain.ClaimUnknown {
		t.Fatalf("claim result = %s, want UNKNOWN", downgraded.Claims[0].Result)
	}
	found := false
	for _, finding := range downgraded.Findings {
		if finding.Code == "CLAIM_STEP_FAILED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want CLAIM_STEP_FAILED", downgraded.Findings)
	}
}

func TestDowngradePartialClaimEvidenceLeavesCompleteClaims(t *testing.T) {
	dag := domain.DAG{Steps: []domain.Step{{Status: domain.StepSucceeded, Options: map[string]string{"claim.id": "compile"}}}}
	verdict := domain.VerdictResult{
		Verdict: domain.VerdictVerified,
		Claims: []domain.ClaimAssessment{{
			ClaimID: "compile", Required: true,
			MinimumResult: domain.ClaimTested, Result: domain.ClaimTested,
		}},
	}
	if got := downgradePartialClaimEvidence(dag, verdict); got.Verdict != domain.VerdictVerified {
		t.Fatalf("verdict = %s, want VERIFIED", got.Verdict)
	}
}

func TestAggregateDeterministicFailureWithoutCounterexampleVerifies(t *testing.T) {
	ctx := context.Background()
	fixture := newProofFixture(t, []domain.Claim{
		newProofClaim(domain.Run{}, "compile", domain.ClaimKindCompile, true),
	}, domain.StepKindCompile)
	evidenceItem := testProofEvidence(fixture.run, "compile", domain.EvidenceCompile, domain.ClaimViolated)
	artifact, err := fixture.cas.Put(ctx, ports.SecurityScope{
		TenantID: fixture.tenantID, TrustDomain: "report", Region: "region-a",
	}, strings.NewReader("go build failed: syntax error"))
	if err != nil {
		t.Fatalf("put failure log: %v", err)
	}
	evidenceItem.Artifacts = []domain.CASRef{artifact}
	fixture.commit(t, domain.StepResult{Evidence: []domain.Evidence{evidenceItem}})

	report, err := fixture.service.Aggregate(ctx, fixture.run)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if report.Verdict != domain.VerdictCounterexample {
		t.Fatalf("verdict = %s, want COUNTEREXAMPLE_FOUND", report.Verdict)
	}
	if len(report.Counterexamples) != 0 {
		t.Fatalf("counterexamples = %d, want 0 for a deterministic compile failure", len(report.Counterexamples))
	}

	trust := evidence.NewTrustStore()
	if err := trust.Add(evidence.TrustedKey{
		KeyID: fixture.signer.KeyID(), Identity: "proof-test", PublicKey: fixture.signer.PublicKey(),
	}); err != nil {
		t.Fatalf("add trusted key: %v", err)
	}
	result, err := evidence.VerifyReport(ctx, report, fixture.source(), evidence.VerifyOptions{
		Trust: trust, Now: proofTestNow.Add(time.Minute), Deep: true,
	})
	if err != nil {
		t.Fatalf("VerifyReport: %v", err)
	}
	if result.Status != evidence.StatusValidIntegrity {
		t.Fatalf("verification status = %s, want VALID_INTEGRITY (issues: %v)", result.Status, result.Issues)
	}
}
