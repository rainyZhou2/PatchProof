package signing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return public, private
}

func TestSignerSignAndVerify(t *testing.T) {
	t.Parallel()
	public, private := newKeyPair(t)
	signer, err := NewEd25519Signer("key-1", private)
	if err != nil {
		t.Fatalf("NewEd25519Signer: %v", err)
	}
	if signer.KeyID() != "key-1" || signer.Algorithm() != "ed25519" {
		t.Fatalf("signer metadata = %q/%q, want key-1/ed25519", signer.KeyID(), signer.Algorithm())
	}
	if !public.Equal(signer.PublicKey()) {
		t.Fatal("PublicKey does not match the generated public key")
	}

	payload := []byte(`{"report":"patchproof"}`)
	signature, err := signer.Sign(context.Background(), payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if signature.ID == "" {
		t.Fatal("signature ID is empty")
	}
	if signature.KeyID != "key-1" || signature.Algorithm != "ed25519" {
		t.Fatalf("signature metadata = %q/%q", signature.KeyID, signature.Algorithm)
	}
	if len(signature.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature length = %d, want %d", len(signature.Signature), ed25519.SignatureSize)
	}
	if signature.SignedAt.IsZero() || signature.SignedAt.Location() != time.UTC {
		t.Fatalf("SignedAt = %v, want non-zero UTC time", signature.SignedAt)
	}
	if !ed25519.Verify(public, payload, signature.Signature) {
		t.Fatal("valid signature did not verify")
	}

	tampered := []byte(`{"report":"patchproof!"}`)
	if ed25519.Verify(public, tampered, signature.Signature) {
		t.Fatal("signature verified after payload tampering")
	}

	otherPublic, _ := newKeyPair(t)
	if ed25519.Verify(otherPublic, payload, signature.Signature) {
		t.Fatal("signature verified with the wrong public key")
	}

	// Signing twice produces distinct signature IDs but both verify.
	second, err := signer.Sign(context.Background(), payload)
	if err != nil {
		t.Fatalf("second Sign: %v", err)
	}
	if second.ID == signature.ID {
		t.Fatal("two signatures share the same ID")
	}
	if !ed25519.Verify(public, payload, second.Signature) {
		t.Fatal("second signature did not verify")
	}
}

func TestNewEd25519SignerRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	_, private := newKeyPair(t)
	if _, err := NewEd25519Signer("", private); err == nil {
		t.Fatal("expected empty key ID to be rejected")
	}
	if _, err := NewEd25519Signer("key-1", nil); err == nil {
		t.Fatal("expected nil private key to be rejected")
	}
	if _, err := NewEd25519Signer("key-1", private[:ed25519.SeedSize]); err == nil {
		t.Fatal("expected short private key to be rejected")
	}
}

func TestPEMRoundTrip(t *testing.T) {
	t.Parallel()
	public, private := newKeyPair(t)

	privatePEM, err := EncodePrivateKeyPEM(private)
	if err != nil {
		t.Fatalf("EncodePrivateKeyPEM: %v", err)
	}
	block, _ := pem.Decode(privatePEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("private PEM block = %+v, want PRIVATE KEY", block)
	}
	parsedPrivate, err := ParsePrivateKeyPEM(privatePEM)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM: %v", err)
	}
	if !private.Equal(parsedPrivate) {
		t.Fatal("private key did not survive PEM round trip")
	}

	publicPEM, err := EncodePublicKeyPEM(public)
	if err != nil {
		t.Fatalf("EncodePublicKeyPEM: %v", err)
	}
	block, _ = pem.Decode(publicPEM)
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("public PEM block = %+v, want PUBLIC KEY", block)
	}
	parsedPublic, err := ParsePublicKeyPEM(publicPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}
	if !public.Equal(parsedPublic) {
		t.Fatal("public key did not survive PEM round trip")
	}

	// Raw ed25519 PEM encodings (seed and expanded key) are also accepted.
	for name, raw := range map[string][]byte{
		"seed":     private.Seed(),
		"expanded": []byte(private),
	} {
		raw := raw
		t.Run("raw private "+name, func(t *testing.T) {
			t.Parallel()
			encoded := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: raw})
			decoded, err := ParsePrivateKeyPEM(encoded)
			if err != nil {
				t.Fatalf("ParsePrivateKeyPEM: %v", err)
			}
			if !private.Equal(decoded) {
				t.Fatal("raw ed25519 private key did not round trip")
			}
		})
	}
	rawPublic := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PUBLIC KEY", Bytes: []byte(public)})
	decodedPublic, err := ParsePublicKeyPEM(rawPublic)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM raw: %v", err)
	}
	if !public.Equal(decodedPublic) {
		t.Fatal("raw ed25519 public key did not round trip")
	}
}

func TestPEMRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	_, private := newKeyPair(t)
	public, _ := newKeyPair(t)

	if _, err := ParsePrivateKeyPEM([]byte("not pem")); err == nil {
		t.Fatal("expected non-PEM private key to be rejected")
	}
	if _, err := ParsePublicKeyPEM([]byte("not pem")); err == nil {
		t.Fatal("expected non-PEM public key to be rejected")
	}
	badBlock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})
	if _, err := ParsePrivateKeyPEM(badBlock); err == nil {
		t.Fatal("expected unsupported private key type to be rejected")
	}
	if _, err := ParsePublicKeyPEM(badBlock); err == nil {
		t.Fatal("expected unsupported public key type to be rejected")
	}
	shortRaw := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: []byte("short")})
	if _, err := ParsePrivateKeyPEM(shortRaw); err == nil {
		t.Fatal("expected invalid raw private key length to be rejected")
	}
	shortRawPublic := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PUBLIC KEY", Bytes: []byte("short")})
	if _, err := ParsePublicKeyPEM(shortRawPublic); err == nil {
		t.Fatal("expected invalid raw public key length to be rejected")
	}

	publicDER, err := EncodePublicKeyPEM(public)
	if err != nil {
		t.Fatalf("EncodePublicKeyPEM: %v", err)
	}
	// A public key must not be accepted where a private key is required.
	if _, err := ParsePrivateKeyPEM(publicDER); err == nil {
		t.Fatal("expected public key PEM to be rejected as a private key")
	}
	privateDER, err := EncodePrivateKeyPEM(private)
	if err != nil {
		t.Fatalf("EncodePrivateKeyPEM: %v", err)
	}
	if _, err := ParsePublicKeyPEM(privateDER); err == nil {
		t.Fatal("expected private key PEM to be rejected as a public key")
	}
}

func TestLoadKeyFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	public, private := newKeyPair(t)
	privatePath := filepath.Join(dir, "signing.pem")
	publicPath := filepath.Join(dir, "signing.pub.pem")
	privatePEM, err := EncodePrivateKeyPEM(private)
	if err != nil {
		t.Fatalf("EncodePrivateKeyPEM: %v", err)
	}
	publicPEM, err := EncodePublicKeyPEM(public)
	if err != nil {
		t.Fatalf("EncodePublicKeyPEM: %v", err)
	}
	if err := os.WriteFile(privatePath, privatePEM, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	if err := os.WriteFile(publicPath, publicPEM, 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	loadedPrivate, err := LoadPrivateKey(privatePath)
	if err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
	if !private.Equal(loadedPrivate) {
		t.Fatal("loaded private key differs")
	}
	loadedPublic, err := LoadPublicKey(publicPath)
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}
	if !public.Equal(loadedPublic) {
		t.Fatal("loaded public key differs")
	}

	if _, err := LoadPrivateKey(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("expected missing private key error")
	}
	if _, err := LoadPublicKey(filepath.Join(dir, "missing.pub.pem")); err == nil {
		t.Fatal("expected missing public key error")
	}
}

func TestLoadOrGenerateCreatesAndReusesKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "dir", "signing.pem")

	signer, err := LoadOrGenerate(path, "generated-key")
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat generated key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("generated key permissions = %04o, want 0600", got)
	}
	public := signer.PublicKey()

	// A second call must reuse the same key material rather than overwrite it.
	reused, err := LoadOrGenerate(path, "generated-key")
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	if !public.Equal(reused.PublicKey()) {
		t.Fatal("LoadOrGenerate generated a new key instead of reusing the existing one")
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat reused key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("reused key permissions = %04o, want 0600", got)
	}

	payload := []byte("deterministic payload")
	signature, err := signer.Sign(context.Background(), payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(reused.PublicKey(), payload, signature.Signature) {
		t.Fatal("signature from first signer did not verify with reused key")
	}

	// The persisted key must be parseable and match the in-memory key.
	onDisk, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatalf("LoadPrivateKey generated: %v", err)
	}
	if !public.Equal(onDisk.Public().(ed25519.PublicKey)) {
		t.Fatal("on-disk key does not match generated signer")
	}
}

func TestLoadOrGenerateRejectsCorruptFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "signing.pem")
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("write corrupt key: %v", err)
	}
	if _, err := LoadOrGenerate(path, "key-1"); err == nil {
		t.Fatal("expected corrupt key file to be rejected")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corrupt key: %v", err)
	}
	if !bytes.Equal(raw, []byte("corrupt")) {
		t.Fatalf("corrupt key was overwritten: %q", raw)
	}
	if !strings.Contains(string(raw), "corrupt") {
		t.Fatalf("unexpected corrupt key content %q", raw)
	}
}
