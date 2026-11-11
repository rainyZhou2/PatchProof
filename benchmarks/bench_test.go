package benchmarks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/policy"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

func claimsFixture(count int) []domain.ClaimAssessment {
	claims := make([]domain.ClaimAssessment, 0, count)
	for i := 0; i < count; i++ {
		claims = append(claims, domain.ClaimAssessment{
			ClaimID: domain.ClaimID(fmt.Sprintf("claim-%04d", i)), Kind: domain.ClaimKindRegression,
			Title: "behavior preservation", Required: true, MinimumResult: domain.ClaimTested,
			Result: domain.ClaimTested, Scope: "signed 64-bit integer inputs",
			EvidenceIDs: []domain.EvidenceID{domain.EvidenceID(fmt.Sprintf("11111111-1111-4111-8111-%012d", i))},
		})
	}
	return claims
}

func BenchmarkCanonicalizeReport(b *testing.B) {
	report := domain.Report{
		SchemaVersion: domain.ReportSchemaV1, ReportID: "22222222-2222-4222-8222-222222222222",
		RunID: "33333333-3333-4333-8333-333333333333", TenantID: "44444444-4444-4444-8444-444444444444",
		Verdict: domain.VerdictVerified, Claims: claimsFixture(100), GeneratedAt: time.Unix(0, 0).UTC(),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := evidence.Canonicalize(report); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMerkleBuild1000Leaves(b *testing.B) {
	leaves := make([]evidence.Leaf, 0, 1000)
	for i := 0; i < 1000; i++ {
		leaves = append(leaves, evidence.Leaf{
			Path: fmt.Sprintf("evidence/%04d.json", i), Digest: fmt.Sprintf("%064x", i+1), Size: 256,
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		manifest, err := evidence.BuildManifest(leaves)
		if err != nil {
			b.Fatal(err)
		}
		if err := manifest.Verify(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMerkleProveAndVerify(b *testing.B) {
	leaves := make([]evidence.Leaf, 0, 1000)
	for i := 0; i < 1000; i++ {
		leaves = append(leaves, evidence.Leaf{
			Path: fmt.Sprintf("evidence/%04d.json", i), Digest: fmt.Sprintf("%064x", i+1), Size: 256,
		})
	}
	manifest, err := evidence.BuildManifest(leaves)
	if err != nil {
		b.Fatal(err)
	}
	leaf := leaves[517]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		proof, err := evidence.Prove(manifest, leaf.Path)
		if err != nil {
			b.Fatal(err)
		}
		if err := evidence.VerifyProof(leaf, proof, manifest.Root); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComputeVerdict100Claims(b *testing.B) {
	claims := make([]domain.Claim, 0, 100)
	evidenceList := make([]domain.Evidence, 0, 100)
	for i := 0; i < 100; i++ {
		id := domain.ClaimID(fmt.Sprintf("claim-%04d", i))
		claims = append(claims, domain.Claim{
			ID: id, RunID: "33333333-3333-4333-8333-333333333333", TenantID: "44444444-4444-4444-8444-444444444444",
			Kind: domain.ClaimKindRegression, Title: "preservation", Predicate: "observable", Domain: "ints",
			Observable: "return", Required: true, MinimumResult: domain.ClaimTested,
		})
		evidenceList = append(evidenceList, domain.Evidence{
			SchemaVersion: domain.EvidenceSchemaV1, ID: domain.EvidenceID(fmt.Sprintf("11111111-1111-4111-8111-%012d", i)),
			TenantID: "44444444-4444-4444-8444-444444444444", RunID: "33333333-3333-4333-8333-333333333333",
			ClaimID: id, Kind: domain.EvidenceDifferential, Result: domain.ClaimTested,
			Scope: domain.EvidenceScope{InputDomain: "integers"}, Engine: domain.EngineInfo{Name: "bench", Version: "1"},
			Environment: domain.EnvironmentInfo{Arch: "amd64", SandboxProfile: "trusted-local"},
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := domain.ComputeVerdict(domain.VerdictInput{Claims: claims, Evidence: evidenceList})
		if result.Verdict != domain.VerdictVerified {
			b.Fatalf("unexpected verdict %s", result.Verdict)
		}
	}
}

func BenchmarkPolicyEvaluation(b *testing.B) {
	engine, err := policy.NewEngine(policy.HardPolicy{
		MinIsolation: domain.IsolationContainer, RequireNoSubjectNetwork: true,
		RequiredChecks: []string{"compile"}, ProtectedPaths: []string{".github/**"},
		MaxChangedFiles: 2000, AllowedRegions: []string{"region-a"},
	})
	if err != nil {
		b.Fatal(err)
	}
	document, err := domain.ParsePolicyDocument([]byte(policyFixture))
	if err != nil {
		b.Fatal(err)
	}
	input := ports.PolicyInput{
		Tenant: domain.Tenant{
			ID: "11111111-1111-4111-8111-111111111111", HomeRegion: "region-a",
			SecurityTier: domain.SecurityTierStandard, Status: domain.TenantActive, ActiveWriterEpoch: 1,
		},
		Project: domain.Project{
			ID: "22222222-2222-4222-8222-222222222222", TenantID: "11111111-1111-4111-8111-111111111111",
			Provider: "git", RepositoryURI: "https://example.org/repo.git", Classification: "standard",
		},
		Policy: document, PolicyDigest: "sha256:abc", PolicyTrusted: true,
		ChangedPaths: []string{"internal/a.go", "internal/b.go"}, ChangedFiles: 2, Region: "region-a",
		Isolation: domain.IsolationGVisor,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.Evaluate(context.Background(), input); err != nil {
			b.Fatal(err)
		}
	}
}

const policyFixture = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: benchmark
spec:
  trustedSource: {type: project-admin, requireSignedRevision: true}
  sources: {maxRepositorySizeGiB: 2, maxChangedFiles: 2000, maxFileSizeMiB: 64, disallowSubmodulesUnlessApproved: true}
  verification:
    required: [compile, regression-test, differential-fuzz]
    optional: [bounded-smt]
    allowInconclusiveOn: [bounded-smt]
    minimumEvidenceLevels: {differential-fuzz: TESTED_WITHIN_SCOPE}
  execution:
    isolationTier: untrusted-gvisor
    allowedRegions: [region-a]
    defaultCpuMilli: 2000
    defaultMemoryMiB: 4096
    maxStepWallSeconds: 900
    maxRunWallSeconds: 3600
    subjectNetwork: none
    artifactTransport: direct
    workerImage: worker
  budgets: {totalCpuSeconds: 30000, fuzzCpuSeconds: 12000, maxParallelSteps: 16, maxStepCount: 32}
  protectedPaths: [".github/workflows/**"]
  report: {formats: [json, markdown, sarif], requireAttestation: true, includeCounterexamples: true}
`
