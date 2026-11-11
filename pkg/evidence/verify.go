package evidence

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type VerificationStatus string

const (
	StatusValidIntegrity VerificationStatus = "VALID_INTEGRITY"
	StatusInvalid        VerificationStatus = "INVALID"
	StatusUnverifiable   VerificationStatus = "UNVERIFIABLE"
)

type BlobSource interface {
	Open(ctx context.Context, ref domain.CASRef) (io.ReadCloser, error)
}

type TrustedKey struct {
	KeyID     string
	Identity  string
	PublicKey ed25519.PublicKey
	RevokedAt *time.Time
}

type TrustStore struct {
	keys map[string]TrustedKey
}

func NewTrustStore() *TrustStore {
	return &TrustStore{keys: make(map[string]TrustedKey)}
}

func (t *TrustStore) Add(key TrustedKey) error {
	if key.KeyID == "" {
		return fmt.Errorf("trusted key: key id is required")
	}
	if len(key.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted key %s: invalid ed25519 public key", key.KeyID)
	}
	t.keys[key.KeyID] = key
	return nil
}

func (t *TrustStore) Lookup(keyID string) (TrustedKey, bool) {
	if t == nil {
		return TrustedKey{}, false
	}
	key, ok := t.keys[keyID]
	return key, ok
}

func (t *TrustStore) Len() int {
	if t == nil {
		return 0
	}
	return len(t.keys)
}

type VerifyOptions struct {
	Trust            *TrustStore
	Now              time.Time
	RequireSignature bool
	MaxReportAge     time.Duration
	Policy           *domain.PolicyDocument
	Deep             bool
}

type CheckStatus string

const (
	CheckOK           CheckStatus = "OK"
	CheckFailed       CheckStatus = "FAILED"
	CheckUnverifiable CheckStatus = "UNVERIFIABLE"
	CheckSkipped      CheckStatus = "SKIPPED"
)

type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

type VerificationResult struct {
	Status VerificationStatus `json:"status"`
	RunID  string             `json:"run_id"`
	Checks []Check            `json:"checks"`
	Issues []string           `json:"issues,omitempty"`
}

func (r *VerificationResult) failed(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: CheckFailed, Detail: detail})
	r.Issues = append(r.Issues, name+": "+detail)
}

func (r *VerificationResult) unverifiable(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: CheckUnverifiable, Detail: detail})
	r.Issues = append(r.Issues, name+": "+detail)
}

func (r *VerificationResult) ok(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: CheckOK, Detail: detail})
}

func (r *VerificationResult) skipped(name, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: CheckSkipped, Detail: detail})
}

type RunAttestation struct {
	RunID           string   `json:"run_id"`
	TenantID        string   `json:"tenant_id"`
	ProjectID       string   `json:"project_id"`
	BaseCommit      string   `json:"base_commit"`
	HeadCommit      string   `json:"head_commit"`
	SnapshotDigest  string   `json:"snapshot_digest"`
	PolicyDigest    string   `json:"policy_digest"`
	RequestedChecks []string `json:"requested_checks"`
}

func VerifyReport(ctx context.Context, report domain.Report, source BlobSource, options VerifyOptions) (VerificationResult, error) {
	result := VerificationResult{RunID: report.RunID.String()}
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result.ok("schema", "report schema "+report.SchemaVersion)
	if err := report.Validate(); err != nil {
		result.failed("schema", err.Error())
		result.Status = StatusInvalid
		return result, nil
	}
	if source == nil {
		result.unverifiable("blob_source", "no artifact source configured")
		result.Status = StatusUnverifiable
		return result, nil
	}
	if options.MaxReportAge > 0 && now.Sub(report.GeneratedAt) > options.MaxReportAge {
		result.failed("freshness", fmt.Sprintf("report is older than %s", options.MaxReportAge))
	}

	statement, err := StatementFromReport(report)
	if err != nil {
		result.failed("statement", err.Error())
		result.Status = StatusInvalid
		return result, nil
	}
	statementDigest, err := statement.Digest()
	if err != nil {
		return result, err
	}
	if options.RequireSignature || report.Attestation == domain.AttestationSigned {
		verifySignatures(&result, report, statement, statementDigest, options.Trust, now)
	} else {
		result.skipped("signatures", "report is not attested and signature verification was not required")
	}

	manifest, err := loadManifest(ctx, source, report.Manifest)
	if err != nil {
		result.unverifiable("manifest", err.Error())
		result.Status = StatusUnverifiable
		return result, nil
	}
	if err := verifyManifest(&result, manifest, report); err != nil {
		result.Status = StatusInvalid
		return result, nil
	}

	leaves, err := verifyLeaves(ctx, source, manifest, options.Deep, report)
	if err != nil {
		result.failed("leaves", err.Error())
		result.Status = StatusInvalid
		return result, nil
	}
	verifyClaims(ctx, source, &result, leaves, report)
	verifyRun(ctx, source, &result, leaves, report)
	verifyEvidence(ctx, source, &result, leaves, report)
	verifyCounterexamples(ctx, source, &result, leaves, report)
	verifyPolicy(&result, report, options.Policy)
	verifyVerdict(&result, report)

	result.Status = StatusValidIntegrity
	for _, check := range result.Checks {
		if check.Status == CheckFailed {
			result.Status = StatusInvalid
			break
		}
		if check.Status == CheckUnverifiable && result.Status == StatusValidIntegrity {
			result.Status = StatusUnverifiable
		}
	}
	return result, nil
}

func verifySignatures(result *VerificationResult, report domain.Report, statement Statement, statementDigest string, trust *TrustStore, now time.Time) {
	if len(report.Signatures) == 0 {
		result.failed("signatures", "no signatures present")
		return
	}
	if trust == nil || trust.Len() == 0 {
		result.unverifiable("signatures", "no trusted keys configured")
		return
	}
	payload, err := statement.Bytes()
	if err != nil {
		result.failed("signatures", err.Error())
		return
	}
	valid := 0
	for _, signature := range report.Signatures {
		if signature.Subject != statementDigest {
			result.failed("signatures", fmt.Sprintf("signature %s is bound to a different statement", signature.ID))
			continue
		}
		if signature.Algorithm != "ed25519" {
			result.unverifiable("signatures", fmt.Sprintf("signature %s uses unsupported algorithm %q", signature.ID, signature.Algorithm))
			continue
		}
		key, ok := trust.Lookup(signature.KeyID)
		if !ok {
			result.unverifiable("signatures", fmt.Sprintf("signature %s references unknown key %q", signature.ID, signature.KeyID))
			continue
		}
		if key.RevokedAt != nil && !signature.SignedAt.After(*key.RevokedAt) {
			result.failed("signatures", fmt.Sprintf("signature %s was produced by a revoked key", signature.ID))
			continue
		}
		if signature.SignedAt.IsZero() || signature.SignedAt.After(now.Add(5*time.Minute)) {
			result.failed("signatures", fmt.Sprintf("signature %s has an invalid signing time", signature.ID))
			continue
		}
		if !ed25519.Verify(key.PublicKey, payload, signature.Signature) {
			result.failed("signatures", fmt.Sprintf("signature %s does not verify", signature.ID))
			continue
		}
		valid++
	}
	if valid == 0 {
		result.failed("signatures", "no valid trusted signature")
		return
	}
	result.ok("signatures", fmt.Sprintf("%d valid signature(s)", valid))
}

func loadManifest(ctx context.Context, source BlobSource, ref domain.CASRef) (Manifest, error) {
	if err := ref.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("manifest reference: %w", err)
	}
	data, err := readBlob(ctx, source, ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	if int64(len(data)) != ref.SizeBytes || "sha256:"+sha256Sum(data) != "sha256:"+ref.Hex() {
		return Manifest{}, fmt.Errorf("manifest blob does not match its digest")
	}
	canonical, err := ReadAllCanonical(bytes.NewReader(data))
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest is not valid canonical JSON: %w", err)
	}
	if !bytes.Equal(canonical, data) {
		return Manifest{}, fmt.Errorf("manifest is not canonically encoded")
	}
	var manifest Manifest
	if err := decodeCanonical(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

func verifyManifest(result *VerificationResult, manifest Manifest, report domain.Report) error {
	if err := manifest.Verify(); err != nil {
		result.failed("manifest", err.Error())
		return err
	}
	if manifest.Root != report.MerkleRoot {
		err := fmt.Errorf("manifest root %s does not match report root %s", manifest.Root, report.MerkleRoot)
		result.failed("manifest", err.Error())
		return err
	}
	result.ok("manifest", fmt.Sprintf("%d leaves, root %s", len(manifest.Leaves), manifest.Root))
	return nil
}

func verifyLeaves(ctx context.Context, source BlobSource, manifest Manifest, deep bool, report domain.Report) (map[string]Leaf, error) {
	leaves := make(map[string]Leaf, len(manifest.Leaves))
	required := make([]string, 0, len(report.Claims)+2)
	required = append(required, "claims.json", "run.json")
	for _, claim := range report.Claims {
		for _, evidenceID := range claim.EvidenceIDs {
			required = append(required, "evidence/"+evidenceID.String()+".json")
		}
	}
	for _, counterexample := range report.Counterexamples {
		required = append(required, "counterexamples/"+counterexample.CaseID+".json")
	}
	sort.Strings(required)
	for _, leaf := range manifest.Leaves {
		leaves[leaf.Path] = leaf
		if !deep {
			continue
		}
		if err := verifyLeafBlob(ctx, source, leaf); err != nil {
			return nil, err
		}
	}
	missing := make([]string, 0)
	for _, path := range required {
		if _, ok := leaves[path]; !ok {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		return leaves, fmt.Errorf("evidence manifest is missing %d referenced leaves: %v", len(missing), missing)
	}
	return leaves, nil
}

func verifyLeafBlob(ctx context.Context, source BlobSource, leaf Leaf) error {
	ref := domain.NewCASRef(leaf.Digest, leaf.Size)
	reader, err := source.Open(ctx, ref)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("leaf %q blob is missing", leaf.Path)
		}
		return fmt.Errorf("open leaf %q: %w", leaf.Path, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read leaf %q: %w", leaf.Path, err)
	}
	if int64(len(data)) != leaf.Size {
		return fmt.Errorf("leaf %q size mismatch: manifest %d, actual %d", leaf.Path, leaf.Size, len(data))
	}
	if sha256Sum(data) != trimSHA256(leaf.Digest) {
		return fmt.Errorf("leaf %q digest mismatch", leaf.Path)
	}
	return nil
}

func verifyClaims(ctx context.Context, source BlobSource, result *VerificationResult, leaves map[string]Leaf, report domain.Report) {
	leaf, ok := leaves["claims.json"]
	if !ok {
		result.failed("claims", "claims.json leaf is missing")
		return
	}
	data, err := readLeaf(ctx, source, result, leaf)
	if err != nil {
		return
	}
	expected, err := Canonicalize(report.Claims)
	if err != nil {
		result.failed("claims", err.Error())
		return
	}
	if !bytes.Equal(expected, data) {
		result.failed("claims", "claims.json does not match the signed claim assessments")
		return
	}
	result.ok("claims", fmt.Sprintf("%d claim assessments", len(report.Claims)))
}

func verifyRun(ctx context.Context, source BlobSource, result *VerificationResult, leaves map[string]Leaf, report domain.Report) {
	leaf, ok := leaves["run.json"]
	if !ok {
		result.failed("run", "run.json leaf is missing")
		return
	}
	data, err := readLeaf(ctx, source, result, leaf)
	if err != nil {
		return
	}
	var attestation RunAttestation
	if err := decodeCanonical(data, &attestation); err != nil {
		result.failed("run", err.Error())
		return
	}
	checks := []struct {
		name     string
		expected string
		actual   string
	}{
		{"run_id", report.RunID.String(), attestation.RunID},
		{"tenant_id", report.TenantID.String(), attestation.TenantID},
		{"project_id", report.ProjectID.String(), attestation.ProjectID},
		{"base_commit", report.BaseCommit, attestation.BaseCommit},
		{"head_commit", report.HeadCommit, attestation.HeadCommit},
		{"snapshot_digest", report.SnapshotDigest, attestation.SnapshotDigest},
		{"policy_digest", report.PolicyDigest, attestation.PolicyDigest},
	}
	for _, check := range checks {
		if check.actual != check.expected {
			result.failed("run", fmt.Sprintf("%s mismatch: report %q, attestation %q", check.name, check.expected, check.actual))
			return
		}
	}
	result.ok("run", "run attestation matches the report")
}

func verifyEvidence(ctx context.Context, source BlobSource, result *VerificationResult, leaves map[string]Leaf, report domain.Report) {
	count := 0
	for _, claim := range report.Claims {
		for _, evidenceID := range claim.EvidenceIDs {
			path := "evidence/" + evidenceID.String() + ".json"
			leaf, ok := leaves[path]
			if !ok {
				result.failed("evidence", "manifest is missing "+path)
				return
			}
			data, err := readLeaf(ctx, source, result, leaf)
			if err != nil {
				return
			}
			var evidence domain.Evidence
			if err := decodeCanonical(data, &evidence); err != nil {
				result.failed("evidence", fmt.Sprintf("%s: %v", path, err))
				return
			}
			if err := evidence.Validate(); err != nil {
				result.failed("evidence", fmt.Sprintf("%s: %v", path, err))
				return
			}
			if evidence.RunID != report.RunID || evidence.TenantID != report.TenantID {
				result.failed("evidence", fmt.Sprintf("%s belongs to another run or tenant", path))
				return
			}
			if evidence.ClaimID != claim.ClaimID {
				result.failed("evidence", fmt.Sprintf("%s references claim %s, assessment is %s", path, evidence.ClaimID, claim.ClaimID))
				return
			}
			count++
		}
	}
	result.ok("evidence", fmt.Sprintf("%d evidence documents verified", count))
}

func verifyCounterexamples(ctx context.Context, source BlobSource, result *VerificationResult, leaves map[string]Leaf, report domain.Report) {
	for _, counterexample := range report.Counterexamples {
		path := "counterexamples/" + counterexample.CaseID + ".json"
		leaf, ok := leaves[path]
		if !ok {
			result.failed("counterexamples", "manifest is missing "+path)
			return
		}
		data, err := readLeaf(ctx, source, result, leaf)
		if err != nil {
			return
		}
		var stored domain.Counterexample
		if err := decodeCanonical(data, &stored); err != nil {
			result.failed("counterexamples", fmt.Sprintf("%s: %v", path, err))
			return
		}
		if err := stored.Validate(); err != nil {
			result.failed("counterexamples", fmt.Sprintf("%s: %v", path, err))
			return
		}
		if stored.CaseID != counterexample.CaseID || stored.InputRef.Digest != counterexample.InputRef.Digest {
			result.failed("counterexamples", fmt.Sprintf("%s does not match the report entry", path))
			return
		}
	}
	result.ok("counterexamples", fmt.Sprintf("%d counterexamples verified", len(report.Counterexamples)))
}

func verifyPolicy(result *VerificationResult, report domain.Report, policy *domain.PolicyDocument) {
	if policy == nil {
		result.skipped("policy", "no trusted policy supplied")
		return
	}
	digest, err := policy.Digest()
	if err != nil {
		result.failed("policy", err.Error())
		return
	}
	if digest != report.PolicyDigest {
		result.failed("policy", fmt.Sprintf("report policy digest %s does not match trusted policy %s", report.PolicyDigest, digest))
		return
	}
	required := policy.RequiredChecks()
	requested := toSet(report.RequestedChecks)
	for _, check := range required {
		if _, ok := requested[check]; !ok {
			result.failed("policy", fmt.Sprintf("required check %q is absent from the report", check))
			return
		}
	}
	if report.Verdict == domain.VerdictVerified {
		completed := toSet(report.CompletedChecks)
		for _, check := range required {
			if _, ok := completed[check]; !ok {
				result.failed("policy", fmt.Sprintf("required check %q did not complete", check))
				return
			}
		}
		for _, check := range required {
			claims := claimsForCheck(*policy, report, check)
			if len(claims) == 0 {
				result.failed("policy", fmt.Sprintf("required check %q has no required claim in the report", check))
				return
			}
			minimum := policy.MinimumLevel(check)
			for _, claim := range claims {
				if !claim.Result.Satisfies(minimum) {
					result.failed("policy", fmt.Sprintf("claim %s reached %s but policy requires %s for check %q", claim.ClaimID, claim.Result, minimum, check))
					return
				}
				if !claim.MinimumResult.Satisfies(minimum) {
					result.failed("policy", fmt.Sprintf("claim %s declares minimum %s below policy minimum %s for check %q", claim.ClaimID, claim.MinimumResult, minimum, check))
					return
				}
			}
		}
	}
	result.ok("policy", "trusted policy requirements are reflected in the report")
}

func claimsForCheck(policy domain.PolicyDocument, report domain.Report, check string) []domain.ClaimAssessment {
	ids := make(map[domain.ClaimID]struct{})
	switch domain.NormalizeCheckName(check) {
	case "compile":
		ids["compile"] = struct{}{}
	case "regression-test":
		ids["regression"] = struct{}{}
	case "security-scan":
		ids["security-scan"] = struct{}{}
	case "protected-assets":
		ids["protected-assets"] = struct{}{}
	case "bounded-smt":
		ids["bounded-smt"] = struct{}{}
	case "native-fuzz":
		ids["native-fuzz"] = struct{}{}
	case "differential-fuzz":
		for _, contract := range policy.Spec.Verification.Contracts {
			ids[domain.ClaimID(contract.ID)] = struct{}{}
		}
	default:
		return nil
	}
	out := make([]domain.ClaimAssessment, 0, len(ids))
	for _, claim := range report.Claims {
		if !claim.Required {
			continue
		}
		if _, ok := ids[claim.ClaimID]; ok {
			out = append(out, claim)
		}
	}
	return out
}

func verifyVerdict(result *VerificationResult, report domain.Report) {
	hasRequiredViolation := false
	hasInvalid := false
	for _, claim := range report.Claims {
		if !claim.Required {
			continue
		}
		switch claim.Result {
		case domain.ClaimViolated:
			hasRequiredViolation = true
		case domain.ClaimInvalidEvidence:
			hasInvalid = true
		}
	}
	reproduced := 0
	for _, counterexample := range report.Counterexamples {
		if counterexample.Reproduced {
			reproduced++
		}
	}
	switch report.Verdict {
	case domain.VerdictVerified:
		if hasRequiredViolation || hasInvalid {
			result.failed("verdict", "VERIFIED_WITHIN_SCOPE contradicts claim results")
			return
		}
	case domain.VerdictCounterexample:
		if !hasRequiredViolation || reproduced == 0 {
			result.failed("verdict", "COUNTEREXAMPLE_FOUND requires a violated required claim and a reproduced counterexample")
			return
		}
	}
	result.ok("verdict", string(report.Verdict))
}

func readLeaf(ctx context.Context, source BlobSource, result *VerificationResult, leaf Leaf) ([]byte, error) {
	ref := domain.NewCASRef(leaf.Digest, leaf.Size)
	data, err := readBlob(ctx, source, ref)
	if err != nil {
		result.failed("leaves", fmt.Sprintf("cannot read leaf %q: %v", leaf.Path, err))
		return nil, err
	}
	if int64(len(data)) != leaf.Size {
		result.failed("leaves", fmt.Sprintf("leaf %q size mismatch", leaf.Path))
		return nil, fmt.Errorf("leaf %q size mismatch", leaf.Path)
	}
	if sha256Sum(data) != trimSHA256(leaf.Digest) {
		result.failed("leaves", fmt.Sprintf("leaf %q digest mismatch", leaf.Path))
		return nil, fmt.Errorf("leaf %q digest mismatch", leaf.Path)
	}
	return data, nil
}

func readBlob(ctx context.Context, source BlobSource, ref domain.CASRef) ([]byte, error) {
	if source == nil {
		return nil, fmt.Errorf("blob source is required")
	}
	reader, err := source.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func toSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
