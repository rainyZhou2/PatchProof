package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	gitrepo "github.com/patchproof/patchproof/internal/adapters/repos/git"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/signing"
	"github.com/patchproof/patchproof/internal/policy"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

const protectedPolicy = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: protected
spec:
  trustedSource:
    type: project-admin
    requireSignedRevision: true
  sources:
    maxRepositorySizeGiB: 1
    maxChangedFiles: 10
    maxFileSizeMiB: 8
    disallowSubmodulesUnlessApproved: true
  verification:
    required: [compile]
    optional: []
    allowInconclusiveOn: []
    minimumEvidenceLevels: {}
  execution:
    isolationTier: trusted-local
    allowedRegions: [local]
    defaultCpuMilli: 1000
    defaultMemoryMiB: 1024
    maxStepWallSeconds: 300
    maxRunWallSeconds: 600
    subjectNetwork: none
    artifactTransport: direct
    workerImage: worker
  budgets:
    totalCpuSeconds: 600
    maxParallelSteps: 2
  protectedPaths: [".github/**"]
  report:
    formats: [json]
    requireAttestation: true
`

func TestPathMatcherRejectsTraversalPatterns(t *testing.T) {
	for _, pattern := range []string{"/etc/**", "../secrets/**", "a/../../b"} {
		if _, err := policy.NewPathMatcher([]string{pattern}); err == nil {
			t.Fatalf("pattern %q was accepted", pattern)
		}
	}
	matcher, err := policy.NewPathMatcher([]string{".github/workflows/**", "**/*.pem"})
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Match(".github/workflows/release.yml") || !matcher.Match("config/server.pem") {
		t.Fatal("expected protected path match")
	}
	if matcher.Match("internal/server.go") {
		t.Fatal("unexpected protected path match")
	}
}

func TestProtectedPathPolicyBlocksPatch(t *testing.T) {
	engine, err := policy.NewEngine(policy.HardPolicy{
		MinIsolation: domain.IsolationTrustedLocal, RequiredChecks: []string{"compile"},
		MaxChangedFiles: 100, ProtectedPaths: []string{".github/**"}, RequireTrustedPolicy: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	document, err := domain.ParsePolicyDocument([]byte(protectedPolicy))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.Evaluate(context.Background(), ports.PolicyInput{
		Tenant: domain.Tenant{
			ID: "11111111-1111-4111-8111-111111111111", HomeRegion: "local", SecurityTier: domain.SecurityTierStandard,
			Status: domain.TenantActive, ActiveWriterEpoch: 1,
		},
		Project: domain.Project{
			ID: "22222222-2222-4222-8222-222222222222", TenantID: "11111111-1111-4111-8111-111111111111",
			Provider: "git", RepositoryURI: "https://example.org/repo.git", Classification: "trusted-local",
		},
		Policy: document, PolicyDigest: "sha256:abc", PolicyTrusted: true,
		ChangedPaths: []string{".github/workflows/ci.yml"}, ChangedFiles: 1, Region: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed {
		t.Fatal("protected path modification was allowed")
	}
	found := false
	for _, violation := range decision.Violations {
		if violation.Code == "PROTECTED_PATH_MODIFIED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PROTECTED_PATH_MODIFIED, got %#v", decision.Violations)
	}
}

func TestGitSnapshotterRejectsLocalAndCredentialedRepositories(t *testing.T) {
	store := casmemory.New()
	snapshotter, err := gitrepo.New(gitrepo.Config{AllowFileRepositories: false, WorkDir: t.TempDir()}, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshotter.Snapshot(context.Background(), ports.SnapshotRequest{
		Repository: domain.RepositoryRef{
			Provider: "git", URI: "file:///tmp/repo", Commit: strings.Repeat("a", 40),
			TenantID: "11111111-1111-4111-8111-111111111111", TrustDomain: "run",
		},
		TenantID: "11111111-1111-4111-8111-111111111111",
	})
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	_, err = snapshotter.Snapshot(context.Background(), ports.SnapshotRequest{
		Repository: domain.RepositoryRef{
			Provider: "git", URI: "https://user:password@example.org/repo.git", Commit: strings.Repeat("a", 40),
			TenantID: "11111111-1111-4111-8111-111111111111", TrustDomain: "run",
		},
		TenantID: "11111111-1111-4111-8111-111111111111",
	})
	if !errors.As(err, &validation) {
		t.Fatalf("expected credential rejection, got %v", err)
	}
}

func TestCASRejectsDigestMismatchAndInvalidScope(t *testing.T) {
	store := casmemory.New()
	scope := ports.SecurityScope{TenantID: "11111111-1111-4111-8111-111111111111", TrustDomain: "run"}
	payload := []byte("trusted evidence")
	ref, err := store.Put(context.Background(), scope, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	forged := ref
	forged.Digest = strings.Repeat("0", 64)
	if _, err := store.Open(context.Background(), scope, forged); err == nil {
		t.Fatal("forged digest was accepted")
	}
	if _, err := store.Open(context.Background(), ports.SecurityScope{TenantID: scope.TenantID, TrustDomain: "../escape"}, ref); err == nil {
		t.Fatal("invalid trust domain was accepted")
	}
	if _, err := store.Open(context.Background(), ports.SecurityScope{TenantID: "other", TrustDomain: "run"}, ref); err == nil {
		t.Fatal("cross-tenant read was accepted")
	}
}

func TestTamperedSignedReportIsRejected(t *testing.T) {
	store := casmemory.New()
	tenant := domain.TenantID("11111111-1111-4111-8111-111111111111")
	runID := domain.RunID("22222222-2222-4222-8222-222222222222")
	scope := ports.SecurityScope{TenantID: tenant, TrustDomain: "report", Region: "local"}
	evidenceID := domain.NewEvidenceID()
	claims := []domain.ClaimAssessment{{
		ClaimID: "compile", Kind: domain.ClaimKindCompile, Title: "compiles", Required: true,
		MinimumResult: domain.ClaimTested, Result: domain.ClaimTested, Scope: "all packages",
		EvidenceIDs: []domain.EvidenceID{evidenceID},
	}}
	attestation := evidence.RunAttestation{
		RunID: runID.String(), TenantID: tenant.String(), ProjectID: "33333333-3333-4333-8333-333333333333",
		BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40), SnapshotDigest: "sha256:" + strings.Repeat("c", 64),
		PolicyDigest: "sha256:" + strings.Repeat("d", 64), RequestedChecks: []string{"compile"},
	}
	put := func(value any) (domain.CASRef, error) {
		encoded, err := evidence.Canonicalize(value)
		if err != nil {
			return domain.CASRef{}, err
		}
		return store.Put(context.Background(), scope, strings.NewReader(string(encoded)))
	}
	runRef, err := put(attestation)
	if err != nil {
		t.Fatal(err)
	}
	claimsRef, err := put(claims)
	if err != nil {
		t.Fatal(err)
	}
	evidenceRef, err := put(domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1, ID: evidenceID, TenantID: tenant, RunID: runID,
		ClaimID: "compile", Kind: domain.EvidenceCompile, Result: domain.ClaimTested,
		Scope:       domain.EvidenceScope{InputDomain: "all packages"},
		Engine:      domain.EngineInfo{Name: "go-build", Version: "test"},
		Environment: domain.EnvironmentInfo{Arch: "amd64", SandboxProfile: "isolated-container"},
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := evidence.BuildManifest([]evidence.Leaf{
		{Path: "run.json", Digest: runRef.Digest, Size: runRef.SizeBytes},
		{Path: "claims.json", Digest: claimsRef.Digest, Size: claimsRef.SizeBytes},
		{Path: "evidence/" + evidenceID.String() + ".json", Digest: evidenceRef.Digest, Size: evidenceRef.SizeBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestRef, err := put(manifest)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signing.NewEd25519Signer("security", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	report := domain.Report{
		SchemaVersion: domain.ReportSchemaV1, ReportID: "44444444-4444-4444-8444-444444444444", RunID: runID,
		TenantID: tenant, ProjectID: domain.ProjectID(attestation.ProjectID), Verdict: domain.VerdictVerified,
		BaseCommit: attestation.BaseCommit, HeadCommit: attestation.HeadCommit, SnapshotDigest: attestation.SnapshotDigest,
		PolicyDigest: attestation.PolicyDigest, RequestedChecks: []string{"compile"}, CompletedChecks: []string{"compile"},
		Claims: claims, Manifest: manifestRef, MerkleRoot: manifest.Root, Attestation: domain.AttestationSigned,
		GeneratedAt: time.Now().UTC(),
	}
	signed, err := evidence.SignReport(context.Background(), report, signer)
	if err != nil {
		t.Fatal(err)
	}
	trust := evidence.NewTrustStore()
	if err := trust.Add(evidence.TrustedKey{KeyID: "security", PublicKey: publicKey}); err != nil {
		t.Fatal(err)
	}
	source := &proofSource{store: store, scope: scope}
	result, err := evidence.VerifyReport(context.Background(), signed, source, evidence.VerifyOptions{
		Trust: trust, RequireSignature: true, Deep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != evidence.StatusValidIntegrity {
		t.Fatalf("expected VALID_INTEGRITY, got %s: %v", result.Status, result.Issues)
	}
	tampered := signed
	tampered.Claims = []domain.ClaimAssessment{{ClaimID: "compile", Required: true, Result: domain.ClaimViolated}}
	result, err = evidence.VerifyReport(context.Background(), tampered, source, evidence.VerifyOptions{
		Trust: trust, RequireSignature: true, Deep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != evidence.StatusInvalid {
		t.Fatalf("tampered report produced %s", result.Status)
	}
	encoded, _ := json.Marshal(tampered)
	if !strings.Contains(string(encoded), "VIOLATED") {
		t.Fatal("test fixture did not change")
	}
}

type proofSource struct {
	store ports.ArtifactStore
	scope ports.SecurityScope
}

func (s *proofSource) Open(ctx context.Context, ref domain.CASRef) (io.ReadCloser, error) {
	return s.store.Open(ctx, s.scope, ref)
}
