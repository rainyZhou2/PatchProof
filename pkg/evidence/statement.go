package evidence

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	// StatementSchemaV1 is retained for reference; signed reports emitted by
	// this version use v2, which binds the entire report body (findings,
	// limitations, checks, metrics and provenance) rather than a subset.
	StatementSchemaV1 = "patchproof.statement/v1"
	StatementSchemaV2 = "patchproof.statement/v2"
)

type Statement struct {
	SchemaVersion  string    `json:"schema_version"`
	ReportID       string    `json:"report_id"`
	RunID          string    `json:"run_id"`
	TenantID       string    `json:"tenant_id"`
	ProjectID      string    `json:"project_id"`
	Verdict        string    `json:"verdict"`
	BaseCommit     string    `json:"base_commit"`
	HeadCommit     string    `json:"head_commit"`
	SnapshotDigest string    `json:"snapshot_digest"`
	PolicyDigest   string    `json:"policy_digest"`
	ManifestDigest string    `json:"manifest_digest"`
	MerkleRoot     string    `json:"merkle_root"`
	ClaimsDigest   string    `json:"claims_digest"`
	ReportDigest   string    `json:"report_digest"`
	GeneratedAt    time.Time `json:"generated_at"`
}

func StatementFromReport(report domain.Report) (Statement, error) {
	claimsDigest, err := CanonicalDigest(report.Claims)
	if err != nil {
		return Statement{}, fmt.Errorf("claims digest: %w", err)
	}
	reportDigest, err := digestReportBody(report)
	if err != nil {
		return Statement{}, fmt.Errorf("report digest: %w", err)
	}
	return Statement{
		SchemaVersion:  StatementSchemaV2,
		ReportID:       report.ReportID.String(),
		RunID:          report.RunID.String(),
		TenantID:       report.TenantID.String(),
		ProjectID:      report.ProjectID.String(),
		Verdict:        string(report.Verdict),
		BaseCommit:     report.BaseCommit,
		HeadCommit:     report.HeadCommit,
		SnapshotDigest: report.SnapshotDigest,
		PolicyDigest:   report.PolicyDigest,
		ManifestDigest: report.Manifest.Digest,
		MerkleRoot:     report.MerkleRoot,
		ClaimsDigest:   claimsDigest,
		ReportDigest:   reportDigest,
		GeneratedAt:    report.GeneratedAt.UTC(),
	}, nil
}

// digestReportBody canonicalizes every report field except the signature
// envelope and attestation status. Attestation is normalized because
// SignReport flips it from PENDING_ATTESTATION to SIGNED after the statement
// has been produced. This binds findings, limitations, checks, metrics and
// provenance that were previously mutable under a valid signature.
func digestReportBody(report domain.Report) (string, error) {
	copy := report
	copy.Signatures = nil
	copy.Attestation = domain.AttestationPending
	copy.GeneratedAt = copy.GeneratedAt.UTC()
	if len(copy.Counterexamples) > 0 {
		copy.Counterexamples = append([]domain.Counterexample(nil), copy.Counterexamples...)
		for i := range copy.Counterexamples {
			copy.Counterexamples[i].CreatedAt = copy.Counterexamples[i].CreatedAt.UTC()
		}
	}
	return CanonicalDigest(copy)
}

func (s Statement) Bytes() ([]byte, error) {
	return Canonicalize(s)
}

func (s Statement) Digest() (string, error) {
	data, err := s.Bytes()
	if err != nil {
		return "", err
	}
	return DigestBytes(data), nil
}

type Signer interface {
	KeyID() string
	Algorithm() string
	Sign(ctx context.Context, payload []byte) (domain.Signature, error)
}

func SignReport(ctx context.Context, report domain.Report, signer Signer) (domain.Report, error) {
	statement, err := StatementFromReport(report)
	if err != nil {
		return report, err
	}
	payload, err := statement.Bytes()
	if err != nil {
		return report, err
	}
	signature, err := signer.Sign(ctx, payload)
	if err != nil {
		return report, fmt.Errorf("sign report: %w", err)
	}
	digest, err := statement.Digest()
	if err != nil {
		return report, err
	}
	if signature.Subject != "" && signature.Subject != digest {
		return report, fmt.Errorf("signer returned a signature for %q, expected %q", signature.Subject, digest)
	}
	signature.Subject = digest
	if signature.Algorithm == "" {
		signature.Algorithm = signer.Algorithm()
	}
	if signature.KeyID == "" {
		signature.KeyID = signer.KeyID()
	}
	if signature.SignedAt.IsZero() {
		signature.SignedAt = time.Now().UTC()
	}
	report.Signatures = append(report.Signatures, signature)
	report.Attestation = domain.AttestationSigned
	return report, nil
}
