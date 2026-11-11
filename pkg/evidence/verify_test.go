package evidence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type bundle struct {
	report     domain.Report
	source     *memorySource
	signer     *seedSigner
	trust      *TrustStore
	manifest   Manifest
	evidence   domain.Evidence
	claim      domain.ClaimAssessment
	leafByPath map[string]Leaf
}

func buildBundle(t *testing.T) *bundle {
	t.Helper()
	source := newMemorySource()
	signer := newSeedSigner(11)
	claim := domain.ClaimAssessment{
		ClaimID:       evClaimID,
		Kind:          domain.ClaimKindCompile,
		Title:         "Base and head snapshots compile",
		Required:      true,
		MinimumResult: domain.ClaimTested,
		Result:        domain.ClaimProven,
		EvidenceIDs:   []domain.EvidenceID{evEvidenceID},
		Scope:         "all packages",
	}
	evidence := domain.Evidence{
		SchemaVersion: domain.EvidenceSchemaV1,
		ID:            evEvidenceID,
		TenantID:      evTenantID,
		RunID:         evRunID,
		StepID:        evStepID,
		ClaimID:       evClaimID,
		Kind:          domain.EvidenceCompile,
		Result:        domain.ClaimProven,
		Scope:         domain.EvidenceScope{InputDomain: "all packages"},
		Engine:        domain.EngineInfo{Name: "go-build", Version: "1.2.3"},
		Environment:   domain.EnvironmentInfo{Arch: "amd64", SandboxProfile: "isolated-container"},
		CreatedAt:     evGeneratedAt.Add(-time.Minute),
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("test evidence invalid: %v", err)
	}
	claimsRef := source.put(mustCanonical(t, []domain.ClaimAssessment{claim}))
	runRef := source.put(mustCanonical(t, RunAttestation{
		RunID:           evRunID.String(),
		TenantID:        evTenantID.String(),
		ProjectID:       evProjectID.String(),
		BaseCommit:      evBaseCommit,
		HeadCommit:      evHeadCommit,
		SnapshotDigest:  evSnapshotDigest,
		PolicyDigest:    evPolicyDigest,
		RequestedChecks: []string{"compile"},
	}))
	evidenceRef := source.put(mustCanonical(t, evidence))

	// Leaf digests carry the "sha256:" prefix; verifyLeafBlob and readLeaf
	// compare the prefixed hex form against Leaf.Digest.
	leafDigest := func(ref domain.CASRef) string { return "sha256:" + ref.Hex() }
	leaves := []Leaf{
		{Path: "claims.json", Digest: leafDigest(claimsRef), Size: claimsRef.SizeBytes, MediaType: "application/json"},
		{Path: "run.json", Digest: leafDigest(runRef), Size: runRef.SizeBytes, MediaType: "application/json"},
		{Path: "evidence/" + evEvidenceID.String() + ".json", Digest: leafDigest(evidenceRef), Size: evidenceRef.SizeBytes, MediaType: "application/json"},
	}
	manifest, err := BuildManifest(leaves)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	manifestRef := source.put(mustCanonical(t, manifest))

	report := domain.Report{
		SchemaVersion:   domain.ReportSchemaV1,
		ReportID:        evReportID,
		RunID:           evRunID,
		TenantID:        evTenantID,
		ProjectID:       evProjectID,
		Verdict:         domain.VerdictVerified,
		BaseCommit:      evBaseCommit,
		HeadCommit:      evHeadCommit,
		SnapshotDigest:  evSnapshotDigest,
		PolicyDigest:    evPolicyDigest,
		RequestedChecks: []string{"compile"},
		CompletedChecks: []string{"compile"},
		Claims:          []domain.ClaimAssessment{claim},
		Manifest:        manifestRef,
		MerkleRoot:      manifest.Root,
		Attestation:     domain.AttestationPending,
		GeneratedAt:     evGeneratedAt,
	}
	signed, err := SignReport(context.Background(), report, signer)
	if err != nil {
		t.Fatalf("sign report: %v", err)
	}
	if err := signed.Validate(); err != nil {
		t.Fatalf("signed report invalid: %v", err)
	}

	leafByPath := make(map[string]Leaf, len(manifest.Leaves))
	for _, leaf := range manifest.Leaves {
		leafByPath[leaf.Path] = leaf
	}
	return &bundle{
		report:     signed,
		source:     source,
		signer:     signer,
		trust:      signer.trustStore(),
		manifest:   manifest,
		evidence:   evidence,
		claim:      claim,
		leafByPath: leafByPath,
	}
}

func mustCanonical(t *testing.T, value any) []byte {
	t.Helper()
	data, err := Canonicalize(value)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return data
}

func defaultVerifyOptions(b *bundle) VerifyOptions {
	return VerifyOptions{
		Trust:            b.trust,
		Now:              evGeneratedAt.Add(time.Hour),
		RequireSignature: true,
		MaxReportAge:     24 * time.Hour,
		Deep:             true,
	}
}

func checksNamed(result VerificationResult, name string) []Check {
	var out []Check
	for _, check := range result.Checks {
		if check.Name == name {
			out = append(out, check)
		}
	}
	return out
}

func hasCheckStatus(result VerificationResult, name string, status CheckStatus) bool {
	for _, check := range checksNamed(result, name) {
		if check.Status == status {
			return true
		}
	}
	return false
}

func assertIssueContains(t *testing.T, result VerificationResult, fragment string) {
	t.Helper()
	for _, issue := range result.Issues {
		if strings.Contains(issue, fragment) {
			return
		}
	}
	t.Fatalf("no issue contains %q: %v", fragment, result.Issues)
}

// signedWithManifest returns a copy of the signed report bound to a new
// manifest, re-signed so the signature itself is valid.
func (b *bundle) signedWithManifest(t *testing.T, manifest Manifest) domain.Report {
	t.Helper()
	manifestRef := b.source.put(mustCanonical(t, manifest))
	report := b.report
	report.Signatures = nil
	report.Attestation = domain.AttestationPending
	report.Manifest = manifestRef
	report.MerkleRoot = manifest.Root
	signed, err := SignReport(context.Background(), report, b.signer)
	if err != nil {
		t.Fatalf("re-sign report: %v", err)
	}
	return signed
}

func TestVerifyReportValidIntegrity(t *testing.T) {
	b := buildBundle(t)
	result, err := VerifyReport(context.Background(), b.report, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusValidIntegrity {
		t.Fatalf("status = %s, want VALID_INTEGRITY (issues: %v)", result.Status, result.Issues)
	}
	if len(result.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", result.Issues)
	}
	for _, name := range []string{"manifest", "claims", "run", "evidence", "signatures", "verdict"} {
		if !hasCheckStatus(result, name, CheckOK) {
			t.Errorf("check %q did not pass OK: %+v", name, checksNamed(result, name))
		}
	}
	if result.RunID != evRunID.String() {
		t.Fatalf("result run id = %q", result.RunID)
	}
}

func TestVerifyReportTamperedLeafBlob(t *testing.T) {
	b := buildBundle(t)
	evidencePath := "evidence/" + evEvidenceID.String() + ".json"
	leaf, ok := b.leafByPath[evidencePath]
	if !ok {
		t.Fatalf("missing leaf %s", evidencePath)
	}
	// Keep the declared size intact so the digest check, not the size check,
	// is the check that fails.
	tamperedBlob := make([]byte, int(leaf.Size))
	for i := range tamperedBlob {
		tamperedBlob[i] = 'x'
	}
	b.source.replaceBlob(domain.NewCASRef(leaf.Digest, leaf.Size), tamperedBlob)

	result, err := VerifyReport(context.Background(), b.report, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "leaves", CheckFailed) {
		t.Fatalf("expected leaves failure check: %+v", result.Checks)
	}
	assertIssueContains(t, result, "digest mismatch")
}

func TestVerifyReportRemovedLeaf(t *testing.T) {
	b := buildBundle(t)
	filtered := make([]Leaf, 0, len(b.manifest.Leaves))
	for _, leaf := range b.manifest.Leaves {
		if leaf.Path == "claims.json" {
			continue
		}
		filtered = append(filtered, leaf)
	}
	manifest, err := BuildManifest(filtered)
	if err != nil {
		t.Fatalf("build filtered manifest: %v", err)
	}
	report := b.signedWithManifest(t, manifest)

	result, err := VerifyReport(context.Background(), report, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID (issues: %v)", result.Status, result.Issues)
	}
}

func TestVerifyReportModifiedClaimsAfterSigning(t *testing.T) {
	b := buildBundle(t)
	tampered := b.report
	tampered.Claims = append([]domain.ClaimAssessment(nil), b.report.Claims...)
	tampered.Claims[0].Result = domain.ClaimViolated
	tampered.Verdict = domain.VerdictCounterexample

	result, err := VerifyReport(context.Background(), tampered, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "signatures", CheckFailed) && !hasCheckStatus(result, "claims", CheckFailed) {
		t.Fatalf("expected signature or claims failure: %+v", result.Checks)
	}
}

func TestVerifyReportTamperedMerkleRoot(t *testing.T) {
	b := buildBundle(t)
	tampered := b.report
	tampered.MerkleRoot = "sha256:" + evDigestHexC

	result, err := VerifyReport(context.Background(), tampered, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "manifest", CheckFailed) {
		t.Fatalf("expected manifest failure: %+v", result.Checks)
	}
	assertIssueContains(t, result, "does not match report root")
}

func TestVerifyReportTamperedSignature(t *testing.T) {
	b := buildBundle(t)
	tampered := b.report
	tampered.Signatures = append([]domain.Signature(nil), b.report.Signatures...)
	tampered.Signatures[0].Signature = append([]byte(nil), b.report.Signatures[0].Signature...)
	tampered.Signatures[0].Signature[0] ^= 0xff

	result, err := VerifyReport(context.Background(), tampered, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	assertIssueContains(t, result, "does not verify")
}

func TestVerifyReportUnknownKey(t *testing.T) {
	b := buildBundle(t)
	tampered := b.report
	tampered.Signatures = append([]domain.Signature(nil), b.report.Signatures...)
	tampered.Signatures[0].KeyID = "unknown-key"

	result, err := VerifyReport(context.Background(), tampered, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "signatures", CheckUnverifiable) {
		t.Fatalf("expected unverifiable key check: %+v", result.Checks)
	}
	assertIssueContains(t, result, "unknown key")
	assertIssueContains(t, result, "no valid trusted signature")
}

func TestVerifyReportRevokedKey(t *testing.T) {
	b := buildBundle(t)
	revokedAt := evGeneratedAt.Add(time.Minute)
	trust := NewTrustStore()
	if err := trust.Add(TrustedKey{
		KeyID:     b.signer.keyID,
		Identity:  "revoked signer",
		PublicKey: b.signer.publicKey(),
		RevokedAt: &revokedAt,
	}); err != nil {
		t.Fatalf("add trusted key: %v", err)
	}
	options := defaultVerifyOptions(b)
	options.Trust = trust

	result, err := VerifyReport(context.Background(), b.report, b.source, options)
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	assertIssueContains(t, result, "revoked key")
}

func TestVerifyReportExpired(t *testing.T) {
	b := buildBundle(t)
	options := defaultVerifyOptions(b)
	options.Now = evGeneratedAt.Add(48 * time.Hour)
	options.MaxReportAge = 24 * time.Hour

	result, err := VerifyReport(context.Background(), b.report, b.source, options)
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "freshness", CheckFailed) {
		t.Fatalf("expected freshness failure: %+v", result.Checks)
	}
}

func TestVerifyReportNoTrustStore(t *testing.T) {
	b := buildBundle(t)
	options := defaultVerifyOptions(b)
	options.Trust = nil

	result, err := VerifyReport(context.Background(), b.report, b.source, options)
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusUnverifiable {
		t.Fatalf("status = %s, want UNVERIFIABLE", result.Status)
	}
	assertIssueContains(t, result, "no trusted keys")
}

func TestVerifyReportSignatureNotRequired(t *testing.T) {
	b := buildBundle(t)
	unsigned := b.report
	unsigned.Signatures = nil
	unsigned.Attestation = domain.AttestationPending
	options := defaultVerifyOptions(b)
	options.RequireSignature = false
	options.Trust = nil

	result, err := VerifyReport(context.Background(), unsigned, b.source, options)
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusValidIntegrity {
		t.Fatalf("status = %s, want VALID_INTEGRITY (issues: %v)", result.Status, result.Issues)
	}
	if !hasCheckStatus(result, "signatures", CheckSkipped) {
		t.Fatalf("expected skipped signatures: %+v", result.Checks)
	}
}

func TestVerifyReportNoBlobSource(t *testing.T) {
	b := buildBundle(t)
	result, err := VerifyReport(context.Background(), b.report, nil, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusUnverifiable {
		t.Fatalf("status = %s, want UNVERIFIABLE", result.Status)
	}
}

func TestVerifyReportTamperedManifestBlob(t *testing.T) {
	b := buildBundle(t)
	other, err := BuildManifest([]Leaf{
		{Path: "run.json", Digest: DigestBytes([]byte("other")), Size: 5},
	})
	if err != nil {
		t.Fatalf("build other manifest: %v", err)
	}
	b.source.replaceBlob(b.report.Manifest, mustCanonical(t, other))

	result, err := VerifyReport(context.Background(), b.report, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusUnverifiable {
		t.Fatalf("status = %s, want UNVERIFIABLE (issues: %v)", result.Status, result.Issues)
	}
	assertIssueContains(t, result, "does not match its digest")
}

func TestVerifyReportContradictoryVerdict(t *testing.T) {
	b := buildBundle(t)
	contradictory := b.report
	contradictory.Verdict = domain.VerdictCounterexample

	result, err := VerifyReport(context.Background(), contradictory, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "verdict", CheckFailed) {
		t.Fatalf("expected verdict failure: %+v", result.Checks)
	}
}

// TestVerifyReportUnprefixedLeafDigests verifies manifests whose Leaf.Digest
// uses the bare hex form, as internal/app/proof/service.go produces from
// domain.CASRef.Digest. Both prefixed and bare forms are valid CAS refs.
func TestVerifyReportUnprefixedLeafDigests(t *testing.T) {
	b := buildBundle(t)
	leaves := make([]Leaf, len(b.manifest.Leaves))
	for i, leaf := range b.manifest.Leaves {
		leaf.Digest = strings.TrimPrefix(leaf.Digest, "sha256:")
		leaves[i] = leaf
	}
	manifest, err := BuildManifest(leaves)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	report := b.signedWithManifest(t, manifest)
	result, err := VerifyReport(context.Background(), report, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusValidIntegrity {
		t.Fatalf("status = %s, want VALID_INTEGRITY for unprefixed leaf digests (issues: %v)", result.Status, result.Issues)
	}
	if !hasCheckStatus(result, "leaves", CheckOK) && !hasCheckStatus(result, "manifest", CheckOK) {
		t.Fatalf("expected manifest/leaf checks to pass: %+v", result.Checks)
	}
}

// TestVerifyReportReportsMissingLeaf checks that a missing referenced leaf is
// surfaced in the checks and issues rather than only as a bare INVALID status.
func TestVerifyReportReportsMissingLeaf(t *testing.T) {
	b := buildBundle(t)
	filtered := make([]Leaf, 0, len(b.manifest.Leaves))
	for _, leaf := range b.manifest.Leaves {
		if leaf.Path == "claims.json" {
			continue
		}
		filtered = append(filtered, leaf)
	}
	manifest, err := BuildManifest(filtered)
	if err != nil {
		t.Fatalf("build filtered manifest: %v", err)
	}
	report := b.signedWithManifest(t, manifest)
	result, err := VerifyReport(context.Background(), report, b.source, defaultVerifyOptions(b))
	if err != nil {
		t.Fatalf("verify report: %v", err)
	}
	if result.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", result.Status)
	}
	if !hasCheckStatus(result, "leaves", CheckFailed) {
		t.Fatalf("expected leaves failure check: %+v", result.Checks)
	}
	assertIssueContains(t, result, "missing")
}

func TestVerifyPolicyEnforcesMinimumEvidenceLevels(t *testing.T) {
	policy := domain.PolicyDocument{
		APIVersion: domain.PolicyAPIVersion,
		Kind:       "VerificationPolicy",
		Metadata:   domain.PolicyMetadata{Name: "minimum-levels"},
		Spec: domain.PolicySpec{
			Verification: domain.VerificationSpec{
				Required:              []string{"compile"},
				MinimumEvidenceLevels: map[string]string{"compile": string(domain.ClaimProven)},
			},
		},
	}
	digest, err := policy.Digest()
	if err != nil {
		t.Fatal(err)
	}
	report := domain.Report{
		Verdict:         domain.VerdictVerified,
		PolicyDigest:    digest,
		RequestedChecks: []string{"compile"},
		CompletedChecks: []string{"compile"},
		Claims: []domain.ClaimAssessment{{
			ClaimID: "compile", Kind: domain.ClaimKindCompile, Required: true,
			MinimumResult: domain.ClaimTested, Result: domain.ClaimTested,
		}},
	}

	var tested VerificationResult
	verifyPolicy(&tested, report, &policy)
	if !hasCheckStatus(tested, "policy", CheckFailed) {
		t.Fatalf("policy with TESTED evidence passed a PROVEN minimum: %+v", tested.Checks)
	}

	report.Claims[0].Result = domain.ClaimProven
	report.Claims[0].MinimumResult = domain.ClaimProven
	var proven VerificationResult
	verifyPolicy(&proven, report, &policy)
	if !hasCheckStatus(proven, "policy", CheckOK) {
		t.Fatalf("policy with PROVEN evidence failed: %+v", proven.Checks)
	}
}
