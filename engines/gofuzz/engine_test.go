package gofuzz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
)

type memoryStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMemoryStore() *memoryStore {
	return &memoryStore{blobs: make(map[string][]byte)}
}

func (m *memoryStore) Put(ctx context.Context, scope ports.SecurityScope, reader io.Reader) (domain.CASRef, error) {
	_ = ctx
	_ = scope
	data, err := io.ReadAll(io.LimitReader(reader, 32<<20))
	if err != nil {
		return domain.CASRef{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	m.mu.Lock()
	m.blobs[digest] = data
	m.mu.Unlock()
	return domain.NewCASRef(digest, int64(len(data))), nil
}

func (m *memoryStore) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	_ = ctx
	_ = scope
	m.mu.Lock()
	data, ok := m.blobs[ref.Hex()]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("blob %s not found", ref.Digest)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (m *memoryStore) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	_ = ctx
	_ = scope
	m.mu.Lock()
	_, ok := m.blobs[ref.Hex()]
	m.mu.Unlock()
	if !ok {
		return ports.BlobInfo{}, fmt.Errorf("blob %s not found", ref.Digest)
	}
	return ports.BlobInfo{Ref: ref, Namespace: "test"}, nil
}

func (m *memoryStore) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	_ = ctx
	_ = scope
	m.mu.Lock()
	delete(m.blobs, ref.Hex())
	m.mu.Unlock()
	return nil
}

func (m *memoryStore) Health(ctx context.Context) error {
	_ = ctx
	return nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeModule(t *testing.T, dir, source string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/fixture\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "calc", "calc.go"), source)
}

func contractYAML(symbol, kind, seedArgs string) string {
	return fmt.Sprintf(`apiVersion: patchproof.io/v1
kind: Contract
metadata:
  name: calc.apply
spec:
  symbol: %s
  language: go
  kind: %s
  domain:
    description: arbitrary signed integers
  oracle:
    type: json-differential/v1
  seeds:
    - label: seed
      args: %s
  budget:
    maxDurationSeconds: 3
`, symbol, kind, seedArgs)
}

func newRequest(t *testing.T, root, contractRef string, stagedContract bool) engine.Request {
	t.Helper()
	headDir := filepath.Join(root, "head")
	baseDir := filepath.Join(root, "base")
	staged := map[string]engine.StagedInput{
		"base": {Name: "base", Path: baseDir, Kind: "dir"},
		"head": {Name: "head", Path: headDir, Kind: "dir"},
	}
	options := map[string]string{"claim.id": "calc.apply.fuzz"}
	contractPath := ""
	if stagedContract {
		contractPath = filepath.Join(headDir, "contract.yaml")
		staged["contract"] = engine.StagedInput{Name: "contract", Path: contractPath, Kind: "file"}
	} else {
		options["contract.ref"] = contractRef
	}
	return engine.Request{
		Spec: domain.StepSpec{
			RunID:    domain.NewRunID(),
			StepID:   domain.NewStepID(),
			TenantID: domain.NewTenantID(),
			Kind:     domain.StepKindDifferential,
			Language: "go",
			Engine:   EngineName,
			Options:  options,
		},
		TenantID:  domain.NewTenantID(),
		Workspace: filepath.Join(root, "workspace"),
		Staged:    staged,
		Store:     newMemoryStore(),
		Scope:     ports.SecurityScope{TenantID: domain.NewTenantID(), TrustDomain: "test"},
		Clock:     fixedClock{},
	}
}

func TestFuzzSafetyPanic(t *testing.T) {
	root := t.TempDir()
	writeModule(t, filepath.Join(root, "base"), "package calc\n\nfunc Apply(x int) int { return x*2 + 1 }\n")
	writeModule(t, filepath.Join(root, "head"), "package calc\n\nfunc Apply(x int) int {\n\tif x < 0 {\n\t\tpanic(\"negative input\")\n\t}\n\treturn x*2 + 1\n}\n")
	contract := contractYAML("example.com/fixture/calc.Apply", "safety", "[-1]")
	writeFile(t, filepath.Join(root, "head", "contract.yaml"), contract)

	request := newRequest(t, root, "", true)
	request.Spec.Options["fuzz.seconds"] = "1"
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s: %s)", result.Outcome, result.ErrorClass, result.Message)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("expected one evidence, got %d", len(result.Evidence))
	}
	evidence := result.Evidence[0]
	if evidence.Kind != domain.EvidenceFuzz {
		t.Fatalf("evidence kind = %s", evidence.Kind)
	}
	if evidence.Result != domain.ClaimViolated {
		t.Fatalf("evidence result = %s, want VIOLATED (details=%v limits=%v)", evidence.Result, evidence.Scope.Details, evidence.Limits)
	}
	if len(evidence.Artifacts) == 0 {
		t.Fatal("violation evidence must carry artifacts")
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}
	if result.Outputs["result_digest"] == "" {
		t.Fatal("successful result must carry a result_digest output")
	}
	foundLimit := false
	for _, limit := range evidence.Limits {
		if strings.Contains(limit, "does not prove differential equivalence") {
			foundLimit = true
		}
	}
	if !foundLimit {
		t.Fatal("evidence must state that native fuzzing does not prove differential equivalence")
	}
}

func TestFuzzBehaviorPreserving(t *testing.T) {
	root := t.TempDir()
	writeModule(t, filepath.Join(root, "base"), "package calc\n\nfunc Apply(x int) int { return x*2 + 1 }\n")
	writeModule(t, filepath.Join(root, "head"), "package calc\n\nfunc Apply(x int) int { return x + x + 1 }\n")
	// contract.ref must resolve relative to the head snapshot root.
	writeFile(t, filepath.Join(root, "head", "contracts", "calc.yaml"), contractYAML("example.com/fixture/calc.Apply", "behavior-preservation", "[3]"))

	request := newRequest(t, root, "contracts/calc.yaml", false)
	request.Spec.Options["fuzz.seconds"] = "1"
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s: %s)", result.Outcome, result.ErrorClass, result.Message)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Result != domain.ClaimTested {
		t.Fatalf("expected TESTED_WITHIN_SCOPE evidence, got %+v", result.Evidence)
	}
	if err := result.Evidence[0].Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}
	if result.Evidence[0].Scope.Details["corpus_size"] == "" {
		t.Fatal("corpus size missing from fuzz evidence details")
	}
	if len(result.Artifacts) == 0 {
		t.Fatal("expected the fuzz log artifact")
	}
}

func TestFuzzContractParseError(t *testing.T) {
	root := t.TempDir()
	writeModule(t, filepath.Join(root, "base"), "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeModule(t, filepath.Join(root, "head"), "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeFile(t, filepath.Join(root, "head", "contract.yaml"), "apiVersion: patchproof.io/v1\nkind: Contract\nmetadata: {name: broken}\nspec:\n  unknownField: true\n")

	request := newRequest(t, root, "", true)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrEngineUnsupported {
		t.Fatalf("outcome = %s class = %s, want FAILED/ENGINE_UNSUPPORTED", result.Outcome, result.ErrorClass)
	}
	if result.Retryable {
		t.Fatal("invalid contract must not be retryable")
	}
}

func TestFuzzInvalidSymbol(t *testing.T) {
	root := t.TempDir()
	writeModule(t, filepath.Join(root, "base"), "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeModule(t, filepath.Join(root, "head"), "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeFile(t, filepath.Join(root, "head", "contract.yaml"), contractYAML("example.com/fixture/calc.Missing", "safety", "[1]"))

	request := newRequest(t, root, "", true)
	request.Spec.Options["fuzz.seconds"] = "1"
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrEngineUnsupported {
		t.Fatalf("outcome = %s class = %s, want FAILED/ENGINE_UNSUPPORTED", result.Outcome, result.ErrorClass)
	}
	if result.Retryable {
		t.Fatal("unsupported symbol must not be retryable")
	}
	if len(result.Artifacts) == 0 {
		t.Fatal("expected the compiler log as an artifact")
	}
}
