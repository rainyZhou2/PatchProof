package evidence

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func statementReport() domain.Report {
	return domain.Report{
		SchemaVersion:  domain.ReportSchemaV1,
		ReportID:       evReportID,
		RunID:          evRunID,
		TenantID:       evTenantID,
		ProjectID:      evProjectID,
		Verdict:        domain.VerdictVerified,
		BaseCommit:     evBaseCommit,
		HeadCommit:     evHeadCommit,
		SnapshotDigest: evSnapshotDigest,
		PolicyDigest:   evPolicyDigest,
		Claims: []domain.ClaimAssessment{
			{
				ClaimID:       evClaimID,
				Kind:          domain.ClaimKindCompile,
				Title:         "Base and head compile",
				Required:      true,
				MinimumResult: domain.ClaimTested,
				Result:        domain.ClaimProven,
			},
		},
		Manifest:    domain.NewCASRef(evDigestHexA, 10),
		MerkleRoot:  "sha256:" + evDigestHexB,
		GeneratedAt: evGeneratedAt,
	}
}

func TestStatementFromReportDigestStability(t *testing.T) {
	first := statementReport()
	second := first
	otherZone := time.FixedZone("UTC+8", 8*60*60)
	second.GeneratedAt = evGeneratedAt.In(otherZone)

	firstStatement, err := StatementFromReport(first)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	secondStatement, err := StatementFromReport(second)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	firstDigest, err := firstStatement.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	secondDigest, err := secondStatement.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("statement digest depends on time zone: %s vs %s", firstDigest, secondDigest)
	}
	if firstStatement.GeneratedAt.Location() != time.UTC {
		t.Fatalf("statement generated at location = %s, want UTC", firstStatement.GeneratedAt.Location())
	}
	if firstStatement.SchemaVersion != StatementSchemaV1 {
		t.Fatalf("schema version = %q", firstStatement.SchemaVersion)
	}
	if firstStatement.ReportID != evReportID.String() || firstStatement.RunID != evRunID.String() {
		t.Fatalf("statement ids = %s/%s", firstStatement.ReportID, firstStatement.RunID)
	}
	if firstStatement.ManifestDigest != digestOfRef(t) || firstStatement.MerkleRoot != first.MerkleRoot {
		t.Fatalf("statement integrity fields = %+v", firstStatement)
	}
	if firstStatement.ClaimsDigest == "" || !strings.HasPrefix(firstStatement.ClaimsDigest, "sha256:") {
		t.Fatalf("claims digest = %q", firstStatement.ClaimsDigest)
	}
}

func digestOfRef(t *testing.T) string {
	t.Helper()
	return domain.NewCASRef(evDigestHexA, 10).Digest
}

func TestStatementFromReportClaimsAffectDigest(t *testing.T) {
	first := statementReport()
	second := first
	second.Claims = append([]domain.ClaimAssessment(nil), first.Claims...)
	second.Claims[0].Result = domain.ClaimViolated

	firstStatement, _ := StatementFromReport(first)
	secondStatement, _ := StatementFromReport(second)
	firstDigest, err := firstStatement.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	secondDigest, err := secondStatement.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if firstDigest == secondDigest {
		t.Fatal("claim changes did not change the statement digest")
	}
	if firstStatement.ClaimsDigest == secondStatement.ClaimsDigest {
		t.Fatal("claim changes did not change the claims digest")
	}
}

func TestStatementBytesAreCanonical(t *testing.T) {
	statement, err := StatementFromReport(statementReport())
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	data, err := statement.Bytes()
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	canonical, err := Canonicalize(statement)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if string(data) != string(canonical) {
		t.Fatal("statement bytes are not canonical")
	}
}

func TestSignReportWithDeterministicSigner(t *testing.T) {
	signer := newSeedSigner(7)
	report := statementReport()
	signed, err := SignReport(context.Background(), report, signer)
	if err != nil {
		t.Fatalf("sign report: %v", err)
	}
	if signed.Attestation != domain.AttestationSigned {
		t.Fatalf("attestation = %s, want SIGNED", signed.Attestation)
	}
	if len(signed.Signatures) != 1 {
		t.Fatalf("signatures = %d, want 1", len(signed.Signatures))
	}
	signature := signed.Signatures[0]
	if signature.KeyID != signer.KeyID() {
		t.Fatalf("key id = %q", signature.KeyID)
	}
	if signature.Algorithm != "ed25519" {
		t.Fatalf("algorithm = %q", signature.Algorithm)
	}
	statement, err := StatementFromReport(report)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	digest, err := statement.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if signature.Subject != digest {
		t.Fatalf("signature subject = %q, want %q", signature.Subject, digest)
	}
	payload, err := statement.Bytes()
	if err != nil {
		t.Fatalf("statement bytes: %v", err)
	}
	if !ed25519.Verify(signer.publicKey(), payload, signature.Signature) {
		t.Fatal("signature does not verify against the statement payload")
	}
	if !signature.SignedAt.Equal(evGeneratedAt) {
		t.Fatalf("signed at = %s, want %s", signature.SignedAt, evGeneratedAt)
	}
}

type failingSigner struct {
	signature domain.Signature
	err       error
}

func (s failingSigner) KeyID() string     { return "failing-key" }
func (s failingSigner) Algorithm() string { return "ed25519" }
func (s failingSigner) Sign(context.Context, []byte) (domain.Signature, error) {
	return s.signature, s.err
}

func TestSignReportRejectsWrongSubject(t *testing.T) {
	signer := failingSigner{signature: domain.Signature{Subject: "sha256:" + evDigestHexC}}
	if _, err := SignReport(context.Background(), statementReport(), signer); err == nil {
		t.Fatal("signer returning a mismatched subject accepted")
	}
}

func TestSignReportPropagatesSignerError(t *testing.T) {
	sentinel := errors.New("hsm unavailable")
	signer := failingSigner{err: sentinel}
	_, err := SignReport(context.Background(), statementReport(), signer)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrapped %v", err, sentinel)
	}
}

func TestSignReportFillsMissingSignatureMetadata(t *testing.T) {
	signer := failingSigner{signature: domain.Signature{Signature: []byte("opaque")}}
	signed, err := SignReport(context.Background(), statementReport(), signer)
	if err != nil {
		t.Fatalf("sign report: %v", err)
	}
	signature := signed.Signatures[0]
	if signature.KeyID != signer.KeyID() {
		t.Fatalf("key id = %q, want %q", signature.KeyID, signer.KeyID())
	}
	if signature.Algorithm != signer.Algorithm() {
		t.Fatalf("algorithm = %q, want %q", signature.Algorithm, signer.Algorithm())
	}
	if signature.SignedAt.IsZero() {
		t.Fatal("signed at was not filled")
	}
}
