package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type Ed25519Signer struct {
	keyID string
	key   ed25519.PrivateKey
	now   func() time.Time
}

func NewEd25519Signer(keyID string, key ed25519.PrivateKey) (*Ed25519Signer, error) {
	if keyID == "" {
		return nil, fmt.Errorf("signer key id is required")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key")
	}
	return &Ed25519Signer{keyID: keyID, key: key, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Ed25519Signer) KeyID() string     { return s.keyID }
func (s *Ed25519Signer) Algorithm() string { return "ed25519" }

func (s *Ed25519Signer) Sign(_ context.Context, payload []byte) (domain.Signature, error) {
	signature := ed25519.Sign(s.key, payload)
	return domain.Signature{
		ID:        domain.NewSignatureID(),
		KeyID:     s.keyID,
		Algorithm: "ed25519",
		Signature: signature,
		SignedAt:  s.now(),
	}, nil
}

func (s *Ed25519Signer) PublicKey() ed25519.PublicKey {
	return s.key.Public().(ed25519.PublicKey)
}

func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	return ParsePrivateKeyPEM(raw)
}

func ParsePrivateKeyPEM(raw []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("signing key is not PEM encoded")
	}
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse pkcs8 key: %w", err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("signing key is not ed25519")
		}
		return key, nil
	case "ED25519 PRIVATE KEY":
		if len(block.Bytes) == ed25519.PrivateKeySize {
			return ed25519.PrivateKey(block.Bytes), nil
		}
		if len(block.Bytes) == ed25519.SeedSize {
			return ed25519.NewKeyFromSeed(block.Bytes), nil
		}
		return nil, fmt.Errorf("invalid ed25519 key length")
	default:
		return nil, fmt.Errorf("unsupported private key type %q", block.Type)
	}
}

func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	return ParsePublicKeyPEM(raw)
}

func ParsePublicKeyPEM(raw []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("public key is not PEM encoded")
	}
	switch block.Type {
	case "PUBLIC KEY":
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is not ed25519")
		}
		return key, nil
	case "ED25519 PUBLIC KEY":
		if len(block.Bytes) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid ed25519 public key length")
		}
		return ed25519.PublicKey(block.Bytes), nil
	default:
		return nil, fmt.Errorf("unsupported public key type %q", block.Type)
	}
}

func EncodePrivateKeyPEM(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func EncodePublicKeyPEM(key ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func LoadOrGenerate(path, keyID string) (*Ed25519Signer, error) {
	if _, err := os.Stat(path); err == nil {
		key, err := LoadPrivateKey(path)
		if err != nil {
			return nil, err
		}
		return NewEd25519Signer(keyID, key)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	encoded, err := EncodePrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("write signing key: %w", err)
	}
	return NewEd25519Signer(keyID, key)
}
