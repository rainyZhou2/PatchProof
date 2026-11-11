package compatibility

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestReportJSONToleratesAdditiveFields(t *testing.T) {
	document := map[string]any{
		"schema_version": domain.ReportSchemaV1,
		"report_id":      "11111111-1111-4111-8111-111111111111",
		"run_id":         "22222222-2222-4222-8222-222222222222",
		"tenant_id":      "33333333-3333-4333-8333-333333333333",
		"project_id":     "44444444-4444-4444-8444-444444444444",
		"verdict":        "INCONCLUSIVE",
		"claims": []map[string]any{{
			"claim_id": "compile", "kind": "COMPILE", "title": "compiles", "required": true,
			"minimum_result": "TESTED_WITHIN_SCOPE", "result": "NOT_RUN",
		}},
		"attestation_status":    "PENDING_ATTESTATION",
		"future_additive_field": map[string]any{"enabled": true},
		"generated_at":          time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var report domain.Report
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatalf("additive field broke decoding: %v", err)
	}
	if report.SchemaVersion != domain.ReportSchemaV1 || len(report.Claims) != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if _, err := json.Marshal(report); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaConstantsAreVersioned(t *testing.T) {
	for name, value := range map[string]string{
		"evidence": domain.EvidenceSchemaV1,
		"report":   domain.ReportSchemaV1,
		"policy":   domain.PolicySchemaV1,
		"ir":       domain.IRSchemaV1,
	} {
		if value == "" || !strings.Contains(value, "/v1") {
			t.Fatalf("%s schema is not versioned: %q", name, value)
		}
	}
	for _, eventType := range domain.EventTypes() {
		if !strings.HasSuffix(string(eventType), ".v1") {
			t.Fatalf("event %q is not versioned", eventType)
		}
	}
}

func TestPolicyParsingRejectsUnknownFields(t *testing.T) {
	raw := `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata: {name: compat}
spec:
  trustedSource: {type: project-admin, requireSignedRevision: false}
  sources: {maxRepositorySizeGiB: 1, maxChangedFiles: 10, maxFileSizeMiB: 1, disallowSubmodulesUnlessApproved: true}
  verification: {required: [compile], optional: [], allowInconclusiveOn: [], minimumEvidenceLevels: {}}
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
  budgets: {totalCpuSeconds: 10, maxParallelSteps: 1}
  futureField: true
  report: {formats: [json], requireAttestation: false, includeCounterexamples: true}
`
	if _, err := domain.ParsePolicyDocument([]byte(raw)); err == nil {
		t.Fatal("unknown policy field was accepted")
	}
}

func TestPolicyDigestIgnoresFormatting(t *testing.T) {
	const compact = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata: {name: digest}
spec:
  trustedSource: {type: project-admin, requireSignedRevision: false}
  sources: {maxRepositorySizeGiB: 1, maxChangedFiles: 10, maxFileSizeMiB: 1, disallowSubmodulesUnlessApproved: true}
  verification: {required: [compile], optional: [], allowInconclusiveOn: [], minimumEvidenceLevels: {}}
  execution: {isolationTier: trusted-local, allowedRegions: [local], defaultCpuMilli: 1000, defaultMemoryMiB: 1024, maxStepWallSeconds: 300, maxRunWallSeconds: 600, subjectNetwork: none, artifactTransport: direct, workerImage: worker}
  budgets: {totalCpuSeconds: 10, maxParallelSteps: 1}
  report: {formats: [json], requireAttestation: false, includeCounterexamples: true}
`
	const spaced = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: digest
spec:
  trustedSource:
    type: project-admin
    requireSignedRevision: false
  sources:
    maxRepositorySizeGiB: 1
    maxChangedFiles: 10
    maxFileSizeMiB: 1
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
  budgets: {totalCpuSeconds: 10, maxParallelSteps: 1}
  report: {formats: [json], requireAttestation: false, includeCounterexamples: true}
`
	first, err := domain.ParsePolicyDocument([]byte(compact))
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.ParsePolicyDocument([]byte(spaced))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("formatting changed the policy digest: %s vs %s", firstDigest, secondDigest)
	}
}

func TestProtobufContractsDeclareVersionAndFencing(t *testing.T) {
	files := map[string]string{
		filepath.Join("..", "..", "api", "proto", "patchproof", "plugin", "v1", "plugin.proto"): "fencing_generation",
		filepath.Join("..", "..", "api", "proto", "patchproof", "worker", "v1", "worker.proto"): "generation",
	}
	for path, fenceToken := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		for _, token := range []string{"syntax = \"proto3\"", "package patchproof", fenceToken} {
			if !strings.Contains(content, token) {
				t.Fatalf("%s is missing %q", path, token)
			}
		}
	}
}
