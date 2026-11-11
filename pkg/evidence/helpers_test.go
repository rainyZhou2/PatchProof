package evidence

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"os"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	evTenantID   = domain.TenantID("11111111-1111-4111-8111-111111111111")
	evProjectID  = domain.ProjectID("22222222-2222-4222-8222-222222222222")
	evRunID      = domain.RunID("33333333-3333-4333-8333-333333333333")
	evReportID   = domain.ReportID("44444444-4444-4444-8444-444444444444")
	evStepID     = domain.StepID("55555555-5555-4555-8555-555555555555")
	evEvidenceID = domain.EvidenceID("66666666-6666-4666-8666-666666666666")
	evClaimID    = domain.ClaimID("compile")

	evBaseCommit     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	evHeadCommit     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	evSnapshotDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	evPolicyDigest   = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	evDigestHexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	evDigestHexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	evDigestHexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

var evGeneratedAt = time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)

// seedSigner is a deterministic ed25519 signer. Reusing the same seed makes
// signature bytes reproducible across runs.
type seedSigner struct {
	keyID    string
	key      ed25519.PrivateKey
	signedAt time.Time
}

func newSeedSigner(seed byte) *seedSigner {
	raw := make([]byte, ed25519.SeedSize)
	for i := range raw {
		raw[i] = seed
	}
	return &seedSigner{
		keyID:    "test-key-1",
		key:      ed25519.NewKeyFromSeed(raw),
		signedAt: evGeneratedAt,
	}
}

func (s *seedSigner) KeyID() string     { return s.keyID }
func (s *seedSigner) Algorithm() string { return "ed25519" }

func (s *seedSigner) Sign(_ context.Context, payload []byte) (domain.Signature, error) {
	return domain.Signature{
		ID:        domain.NewSignatureID(),
		KeyID:     s.keyID,
		Algorithm: "ed25519",
		Signature: ed25519.Sign(s.key, payload),
		SignedAt:  s.signedAt,
	}, nil
}

func (s *seedSigner) publicKey() ed25519.PublicKey {
	return s.key.Public().(ed25519.PublicKey)
}

func (s *seedSigner) trustStore() *TrustStore {
	store := NewTrustStore()
	_ = store.Add(TrustedKey{KeyID: s.keyID, Identity: "test signer", PublicKey: s.publicKey()})
	return store
}

// memorySource is an in-memory BlobSource keyed by the hex portion of the
// sha256 digest.
type memorySource struct {
	blobs map[string][]byte
}

func newMemorySource() *memorySource {
	return &memorySource{blobs: make(map[string][]byte)}
}

func (m *memorySource) put(content []byte) domain.CASRef {
	ref := domain.NewCASRef(DigestBytes(content), int64(len(content)))
	m.blobs[ref.Hex()] = append([]byte(nil), content...)
	return ref
}

func (m *memorySource) Open(_ context.Context, ref domain.CASRef) (io.ReadCloser, error) {
	data, ok := m.blobs[ref.Hex()]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// replaceBlob overwrites the bytes stored under an existing digest, simulating
// storage tampering.
func (m *memorySource) replaceBlob(ref domain.CASRef, content []byte) {
	m.blobs[ref.Hex()] = append([]byte(nil), content...)
}
