package actioncache

import (
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func testInput() KeyInput {
	return KeyInput{
		TenantID: "11111111-1111-4111-8111-111111111111", CacheEpoch: "v1.2.3",
		StepKind: domain.StepKindCompile, Engine: "go-build", EngineVersion: "1.0.0",
		SnapshotDigest: "sha256:" + strings.Repeat("a", 64), BaseSnapshot: "cas://sha256/" + strings.Repeat("b", 64),
		HeadSnapshot: "cas://sha256/" + strings.Repeat("c", 64), PolicyDigest: "sha256:" + strings.Repeat("d", 64),
		InputManifest: "sha256:" + strings.Repeat("e", 64), Architecture: "amd64", Isolation: "isolated-container",
		SubjectNetwork: "none", RuntimeClass: "gvisor", WorkerImage: "worker@sha256:abc",
		Options: map[string]string{"target": "head", "claim.id": "compile"},
	}
}

func TestKeyIsDeterministicAndSensitive(t *testing.T) {
	first, err := Key(testInput())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Key(testInput())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("key is not deterministic: %s != %s", first, second)
	}
	cases := map[string]func(*KeyInput){
		"tenant":       func(in *KeyInput) { in.TenantID = "22222222-2222-4222-8222-222222222222" },
		"cache epoch":  func(in *KeyInput) { in.CacheEpoch = "v1.2.4" },
		"engine":       func(in *KeyInput) { in.EngineVersion = "1.0.1" },
		"snapshot":     func(in *KeyInput) { in.SnapshotDigest = "sha256:" + strings.Repeat("f", 64) },
		"policy":       func(in *KeyInput) { in.PolicyDigest = "sha256:" + strings.Repeat("0", 64) },
		"architecture": func(in *KeyInput) { in.Architecture = "arm64" },
		"isolation":    func(in *KeyInput) { in.Isolation = "untrusted-gvisor" },
		"worker image": func(in *KeyInput) { in.WorkerImage = "worker@sha256:def" },
		"option":       func(in *KeyInput) { in.Options["target"] = "base" },
	}
	for name, mutate := range cases {
		input := testInput()
		mutate(&input)
		other, err := Key(input)
		if err != nil {
			t.Fatal(err)
		}
		if other == first {
			t.Fatalf("changing %s did not change the key", name)
		}
	}
}

func TestOnlyHermeticStepsAreCacheable(t *testing.T) {
	base := domain.Step{Kind: domain.StepKindCompile, Options: map[string]string{"cache.enabled": "true"}}
	if !Cacheable(base) {
		t.Fatal("compile step should be cacheable when declared")
	}
	for _, kind := range []domain.StepKind{domain.StepKindDifferential, domain.StepKindRegression, domain.StepKindPolicy} {
		step := base
		step.Kind = kind
		if Cacheable(step) {
			t.Fatalf("%s must not be cacheable", kind)
		}
	}
	disabled := base
	disabled.Options = map[string]string{}
	if Cacheable(disabled) {
		t.Fatal("step without cache.enabled must not be cacheable")
	}
}

func TestStorableRejectsDiscoveries(t *testing.T) {
	success := domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("a", 64)}
	if !Storable(success) {
		t.Fatal("successful result should be storable")
	}
	violated := success
	violated.Evidence = []domain.Evidence{testEvidence(domain.ClaimViolated)}
	if Storable(violated) {
		t.Fatal("violated evidence must not be cached")
	}
	withCase := success
	withCase.Counterexamples = []domain.Counterexample{{CaseID: "ce-1"}}
	if Storable(withCase) {
		t.Fatal("counterexamples must not be cached")
	}
	failed := success
	failed.Outcome = domain.OutcomeFailed
	if Storable(failed) {
		t.Fatal("failed result must not be cached")
	}
}

func testEvidence(result domain.ClaimResult) domain.Evidence {
	return domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1, ID: "33333333-3333-4333-8333-333333333333",
		TenantID: "11111111-1111-4111-8111-111111111111", RunID: "44444444-4444-4444-8444-444444444444",
		ClaimID: "compile", Kind: domain.EvidenceCompile, Result: result,
		Scope: domain.EvidenceScope{InputDomain: "packages"}, Engine: domain.EngineInfo{Name: "go-build", Version: "1"},
		Environment: domain.EnvironmentInfo{Arch: "amd64", SandboxProfile: "isolated-container"},
		CreatedAt:   time.Now().UTC(),
	}
}

func TestCloneForStepReattributesEvidence(t *testing.T) {
	entry := domain.ActionCacheEntry{
		TenantID: "11111111-1111-4111-8111-111111111111", ActionKey: "sha256:" + strings.Repeat("a", 64),
		StepKind: domain.StepKindCompile, Engine: "go-build", EngineVersion: "1",
		Result: domain.StepResult{
			Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("b", 64),
			Evidence: []domain.Evidence{testEvidence(domain.ClaimTested)},
		},
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	if err := entry.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cloned, err := CloneForStep(entry, "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666", entry.TenantID, now)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Evidence[0].RunID != "55555555-5555-4555-8555-555555555555" || cloned.Evidence[0].StepID != "66666666-6666-4666-8666-666666666666" {
		t.Fatalf("evidence was not re-attributed: %#v", cloned.Evidence[0])
	}
	if cloned.Evidence[0].ID == entry.Result.Evidence[0].ID {
		t.Fatal("evidence id was reused")
	}
	if cloned.ResultDigest == entry.Result.ResultDigest {
		t.Fatal("result digest was not recomputed")
	}
	if err := cloned.Validate(); err != nil {
		t.Fatal(err)
	}
}
