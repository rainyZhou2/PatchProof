package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/pkg/contract"
)

func TestExamplePolicyAndContractParse(t *testing.T) {
	policyRaw, err := os.ReadFile(filepath.Join("..", "..", "examples", "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := domain.ParsePolicyDocument(policyRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Digest(); err != nil {
		t.Fatal(err)
	}
	if len(document.Spec.Verification.Contracts) != 1 {
		t.Fatalf("expected one contract, got %d", len(document.Spec.Verification.Contracts))
	}
	contractPath := filepath.Join("..", "..", "examples", document.Spec.Verification.Contracts[0].DomainRef)
	parsed, err := contract.LoadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.SymbolRef(); err != nil {
		t.Fatal(err)
	}
}
