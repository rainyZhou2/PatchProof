package domain_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/patchproof/patchproof/internal/domain"
)

const validPolicyYAML = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: baseline
  labels:
    team: platform
spec:
  trustedSource:
    type: git
    requireSignedRevision: true
  sources:
    maxRepositorySizeGiB: 2
    maxChangedFiles: 100
    maxFileSizeMiB: 5
    disallowSubmodulesUnlessApproved: true
  verification:
    required:
      - compile
      - regression-test
    optional:
      - differential-fuzz
    allowInconclusiveOn:
      - differential-fuzz
    minimumEvidenceLevels:
      compile: PROVEN_WITHIN_MODEL
  execution:
    isolationTier: isolated-container
    allowedRegions:
      - us-east-1
    defaultCpuMilli: 1000
    defaultMemoryMiB: 2048
    defaultDiskGiB: 10
    maxStepWallSeconds: 600
    maxRunWallSeconds: 1200
    subjectNetwork: restricted
    artifactTransport: direct
    workerImage: ghcr.io/patchproof/worker:1
    runtimeClass: standard
    allowNetworkEgress: true
  budgets:
    totalCpuSeconds: 1000
    fuzzCpuSeconds: 100
    maxParallelSteps: 8
    maxStepCount: 64
    maxFuzzCases: 1000
  protectedPaths:
    - secrets/**
  report:
    formats:
      - json
    requireAttestation: true
    includeCounterexamples: true
`

func mustParsePolicy(t *testing.T, raw string) domain.PolicyDocument {
	t.Helper()
	doc, err := domain.ParsePolicyDocument([]byte(raw))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	return doc
}

func validPolicy(t *testing.T) domain.PolicyDocument {
	t.Helper()
	return mustParsePolicy(t, validPolicyYAML)
}

func TestParsePolicyDocumentValid(t *testing.T) {
	doc := validPolicy(t)
	if doc.APIVersion != domain.PolicyAPIVersion {
		t.Errorf("api version = %q", doc.APIVersion)
	}
	if doc.Kind != "VerificationPolicy" {
		t.Errorf("kind = %q", doc.Kind)
	}
	if doc.Metadata.Name != "baseline" {
		t.Errorf("metadata name = %q", doc.Metadata.Name)
	}
	if doc.Metadata.Labels["team"] != "platform" {
		t.Errorf("labels = %v", doc.Metadata.Labels)
	}
	if doc.Spec.Execution.IsolationTier != domain.IsolationContainer {
		t.Errorf("isolation = %s", doc.Spec.Execution.IsolationTier)
	}
	if doc.Spec.Execution.SubjectNetwork != domain.NetworkRestricted {
		t.Errorf("subject network = %s", doc.Spec.Execution.SubjectNetwork)
	}
	if !doc.Spec.Execution.AllowNetworkEgress {
		t.Error("allowNetworkEgress not parsed")
	}
	if got := doc.Spec.Budgets.MaxStepCount; got != 64 {
		t.Errorf("max step count = %d", got)
	}
	if len(doc.Spec.ProtectedPaths) != 1 {
		t.Errorf("protected paths = %v", doc.Spec.ProtectedPaths)
	}
}

func TestParsePolicyDocumentUnknownFieldsRejected(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"unknown top-level field", strings.Replace(validPolicyYAML, "apiVersion:", "unexpected: true\napiVersion:", 1)},
		{"unknown spec field", strings.Replace(validPolicyYAML, "spec:\n", "spec:\n  unknownField: true\n", 1)},
		{"unknown verification field", strings.Replace(validPolicyYAML, "    required:\n", "    unknownVerification: true\n    required:\n", 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.ParsePolicyDocument([]byte(tc.raw)); err == nil {
				t.Fatal("unknown field accepted")
			}
		})
	}
}

func TestPolicyDocumentValidateRequiredChecks(t *testing.T) {
	t.Run("missing required checks", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Required = nil
		assertValidationError(t, doc.Validate())
	})
	t.Run("empty required yaml", func(t *testing.T) {
		raw := strings.Replace(validPolicyYAML, "    required:\n      - compile\n      - regression-test\n", "    required: []\n", 1)
		doc, err := domain.ParsePolicyDocument([]byte(raw))
		if err == nil {
			t.Fatalf("empty required checks accepted: %+v", doc.Spec.Verification.Required)
		}
	})
	t.Run("unknown required check", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Required = append(doc.Spec.Verification.Required, "mystery")
		assertValidationError(t, doc.Validate())
	})
	t.Run("duplicate required check normalized", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Required = append(doc.Spec.Verification.Required, "  COMPILE ")
		assertValidationError(t, doc.Validate())
	})
	t.Run("unknown optional check", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Optional = append(doc.Spec.Verification.Optional, "mystery")
		assertValidationError(t, doc.Validate())
	})
	t.Run("required and optional overlap", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Optional = append(doc.Spec.Verification.Optional, "compile")
		assertValidationError(t, doc.Validate())
	})
	t.Run("duplicate optional check", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Optional = append(doc.Spec.Verification.Optional, "differential-fuzz")
		assertValidationError(t, doc.Validate())
	})
	t.Run("allow inconclusive on required check", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.AllowInconclusiveOn = []string{"compile"}
		assertValidationError(t, doc.Validate())
	})
	t.Run("allow inconclusive on unknown check", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.AllowInconclusiveOn = []string{"not-optional"}
		assertValidationError(t, doc.Validate())
	})
	t.Run("unknown minimum evidence check", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.MinimumEvidenceLevels = map[string]string{"mystery": string(domain.ClaimTested)}
		assertValidationError(t, doc.Validate())
	})
	t.Run("invalid minimum evidence level", func(t *testing.T) {
		doc := validPolicy(t)
		for _, level := range []string{string(domain.ClaimViolated), string(domain.ClaimInvalidEvidence), "BOGUS", string(domain.ClaimUnknown)} {
			doc.Spec.Verification.MinimumEvidenceLevels = map[string]string{"compile": level}
			assertValidationError(t, doc.Validate())
		}
	})
}

func TestPolicyDocumentContracts(t *testing.T) {
	validContract := domain.ContractSpec{
		ID:            "contract-1",
		Symbol:        "Parse",
		Language:      "go",
		Kind:          "fix-invariant",
		Title:         "parser behavior",
		Domain:        "ASCII digits",
		DomainRef:     "sha256:" + digestHexA,
		Oracle:        "differential",
		OracleRef:     "sha256:" + digestHexB,
		InputSchema:   "schema",
		Required:      true,
		MinimumResult: string(domain.ClaimProven),
	}

	t.Run("valid contract", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Contracts = []domain.ContractSpec{validContract}
		if err := doc.Validate(); err != nil {
			t.Fatalf("valid contract rejected: %v", err)
		}
	})
	t.Run("invalid contract id", func(t *testing.T) {
		doc := validPolicy(t)
		bad := validContract
		bad.ID = "bad id!"
		doc.Spec.Verification.Contracts = []domain.ContractSpec{bad}
		assertValidationError(t, doc.Validate())
	})
	t.Run("duplicate contract ids", func(t *testing.T) {
		doc := validPolicy(t)
		doc.Spec.Verification.Contracts = []domain.ContractSpec{validContract, validContract}
		assertValidationError(t, doc.Validate())
	})
	t.Run("missing symbol", func(t *testing.T) {
		doc := validPolicy(t)
		bad := validContract
		bad.Symbol = ""
		doc.Spec.Verification.Contracts = []domain.ContractSpec{bad}
		assertValidationError(t, doc.Validate())
	})
	t.Run("contract document reference satisfies domain and oracle", func(t *testing.T) {
		doc := validPolicy(t)
		bad := validContract
		bad.Domain = ""
		bad.Oracle = ""
		doc.Spec.Verification.Contracts = []domain.ContractSpec{bad}
		if err := doc.Validate(); err != nil {
			t.Fatalf("contract with document refs rejected: %v", err)
		}
	})
	t.Run("missing domain and ref", func(t *testing.T) {
		doc := validPolicy(t)
		bad := validContract
		bad.Domain = ""
		bad.DomainRef = ""
		bad.OracleRef = ""
		doc.Spec.Verification.Contracts = []domain.ContractSpec{bad}
		assertValidationError(t, doc.Validate())
	})
	t.Run("missing oracle and ref", func(t *testing.T) {
		doc := validPolicy(t)
		bad := validContract
		bad.Oracle = ""
		bad.OracleRef = ""
		bad.DomainRef = ""
		doc.Spec.Verification.Contracts = []domain.ContractSpec{bad}
		assertValidationError(t, doc.Validate())
	})
	t.Run("invalid minimum result", func(t *testing.T) {
		doc := validPolicy(t)
		bad := validContract
		bad.MinimumResult = "BOGUS"
		doc.Spec.Verification.Contracts = []domain.ContractSpec{bad}
		assertValidationError(t, doc.Validate())
	})
}

func TestPolicyExecutionValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.ExecutionSpec)
	}{
		{"unknown isolation", func(e *domain.ExecutionSpec) { e.IsolationTier = domain.IsolationTier("unknown") }},
		{"zero cpu", func(e *domain.ExecutionSpec) { e.DefaultCPUMilli = 0 }},
		{"zero memory", func(e *domain.ExecutionSpec) { e.DefaultMemoryMiB = 0 }},
		{"short step wall clock", func(e *domain.ExecutionSpec) { e.MaxStepWallSeconds = 10 }},
		{"run wall clock below step wall clock", func(e *domain.ExecutionSpec) { e.MaxRunWallSeconds = e.MaxStepWallSeconds - 1 }},
		{"invalid subject network", func(e *domain.ExecutionSpec) { e.SubjectNetwork = domain.NetworkMode("open") }},
		{"invalid artifact transport", func(e *domain.ExecutionSpec) { e.ArtifactTransport = domain.ArtifactTransport("ftp") }},
		{"brokered offline with restricted subject network", func(e *domain.ExecutionSpec) {
			e.ArtifactTransport = domain.TransportBrokeredOffline
			e.SubjectNetwork = domain.NetworkRestricted
		}},
		{"egress with no subject network", func(e *domain.ExecutionSpec) {
			e.SubjectNetwork = domain.NetworkNone
			e.AllowNetworkEgress = true
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := validPolicy(t)
			tc.mutate(&doc.Spec.Execution)
			assertValidationError(t, doc.Validate())
		})
	}

	valid := validPolicy(t)
	valid.Spec.Execution.SubjectNetwork = domain.NetworkNone
	valid.Spec.Execution.ArtifactTransport = domain.TransportBrokeredOffline
	valid.Spec.Execution.AllowNetworkEgress = false
	if err := valid.Validate(); err != nil {
		t.Fatalf("brokered offline with no network rejected: %v", err)
	}
}

func TestPolicyBudgetsAndProtectedPathsValidation(t *testing.T) {
	doc := validPolicy(t)
	doc.Spec.Budgets.TotalCPUSeconds = -1
	assertValidationError(t, doc.Validate())

	doc = validPolicy(t)
	doc.Spec.Budgets.FuzzCPUSeconds = -1
	assertValidationError(t, doc.Validate())

	doc = validPolicy(t)
	doc.Spec.Budgets.MaxParallelSteps = -1
	assertValidationError(t, doc.Validate())

	doc = validPolicy(t)
	doc.Spec.ProtectedPaths = []string{"   "}
	assertValidationError(t, doc.Validate())
}

func TestPolicyDocumentDigestStability(t *testing.T) {
	first := validPolicy(t)
	second := validPolicy(t)
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("digest not stable: %s vs %s", firstDigest, secondDigest)
	}
	if !strings.HasPrefix(firstDigest, "sha256:") || len(firstDigest) != len("sha256:")+64 {
		t.Fatalf("digest shape = %q", firstDigest)
	}

	marshaled, err := yaml.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	roundTripped, err := domain.ParsePolicyDocument(marshaled)
	if err != nil {
		t.Fatalf("parse marshaled policy: %v", err)
	}
	roundTripDigest, err := roundTripped.Digest()
	if err != nil {
		t.Fatalf("round-trip digest: %v", err)
	}
	if roundTripDigest != firstDigest {
		t.Fatalf("digest changed across marshal/parse: %s vs %s", firstDigest, roundTripDigest)
	}
}

func TestPolicyDocumentHelpers(t *testing.T) {
	doc := validPolicy(t)
	if got := doc.RequiredChecks(); len(got) != 2 || got[0] != "compile" || got[1] != "regression-test" {
		t.Fatalf("required checks = %v", got)
	}
	if got := doc.AllChecks(); len(got) != 3 || got[0] != "compile" || got[1] != "differential-fuzz" || got[2] != "regression-test" {
		t.Fatalf("all checks = %v", got)
	}
	if got := doc.MinimumLevel("compile"); got != domain.ClaimProven {
		t.Fatalf("compile minimum = %s, want PROVEN_WITHIN_MODEL", got)
	}
	if got := doc.MinimumLevel("regression-test"); got != domain.ClaimTested {
		t.Fatalf("default minimum = %s, want TESTED_WITHIN_SCOPE", got)
	}
	budget := doc.Budget(newTime)
	if budget.CPUSeconds != 1000 || budget.WallSeconds != 1200 || budget.FuzzCPUSeconds != 100 || budget.MaxParallelSteps != 8 {
		t.Fatalf("budget = %+v", budget)
	}

	known := domain.KnownChecks()
	if len(known) != 7 {
		t.Fatalf("known checks = %v", known)
	}
	for _, check := range known {
		if _, ok := domain.CheckStepKind(check); !ok {
			t.Errorf("known check %q has no step kind", check)
		}
	}
	if _, ok := domain.CheckStepKind("mystery"); ok {
		t.Error("unknown check mapped to a step kind")
	}
	if kind, _ := domain.CheckStepKind("  COMPILE "); kind != domain.StepKindCompile {
		t.Errorf("normalized compile kind = %s", kind)
	}
}
