package godifferential

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
	return ports.BlobInfo{Ref: ref, Namespace: "test", ContentType: "application/octet-stream"}, nil
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

func (m *memoryStore) content(t *testing.T, ref domain.CASRef) []byte {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[ref.Hex()]
	if !ok {
		t.Fatalf("CAS blob %s is missing", ref.Digest)
	}
	return data
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func writeFixtureFile(t *testing.T, path, contents string) {
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
	writeFixtureFile(t, filepath.Join(dir, "go.mod"), "module example.com/fixture\n\ngo 1.21\n")
	writeFixtureFile(t, filepath.Join(dir, "calc", "calc.go"), source)
}

func contractYAML(symbol, kind string, seeds string) string {
	if seeds == "" {
		seeds = "[3]"
	}
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
    seed: 11
  oracle:
    type: json-differential/v1
    observables: [return, panic]
    normalize: [canonical-json]
  seeds:
    - label: small
      args: %s
  budget:
    maxCases: 250
  assumptions:
    - the input is a signed 64-bit integer
`, symbol, kind, seeds)
}

func baseRequest(t *testing.T, baseDir, headDir, workspace, contractPath string, staged bool) engine.Request {
	t.Helper()
	stagedInputs := map[string]engine.StagedInput{
		"base": {Name: "base", Path: baseDir, Kind: "dir"},
		"head": {Name: "head", Path: headDir, Kind: "dir"},
	}
	if staged {
		stagedInputs["contract"] = engine.StagedInput{Name: "contract", Path: contractPath, Kind: "file"}
	}
	options := map[string]string{"claim.id": "calc.apply.behavior"}
	if !staged {
		options["contract.ref"] = contractPath
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
		Workspace: workspace,
		Staged:    stagedInputs,
		Store:     newMemoryStore(),
		Scope:     ports.SecurityScope{TenantID: domain.NewTenantID(), TrustDomain: "test"},
		Clock:     fixedClock{},
	}
}

func TestDifferentialBehaviorPreserving(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	headDir := filepath.Join(root, "head")
	workspace := filepath.Join(root, "workspace")
	writeModule(t, baseDir, "package calc\n\nfunc Apply(x int) int { return x*2 + 1 }\n")
	writeModule(t, headDir, "package calc\n\nfunc Apply(x int) int { return x + x + 1 }\n")
	writeFixtureFile(t, filepath.Join(baseDir, "contract.yaml"), contractYAML("example.com/fixture/calc.Apply", "behavior-preservation", "[3]"))

	request := baseRequest(t, baseDir, headDir, workspace, filepath.Join(baseDir, "contract.yaml"), true)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s: %s)", result.Outcome, result.ErrorClass, result.Message)
	}
	if len(result.Counterexamples) != 0 {
		t.Fatalf("expected no counterexamples, got %d", len(result.Counterexamples))
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("expected one evidence, got %d", len(result.Evidence))
	}
	evidence := result.Evidence[0]
	if evidence.Kind != domain.EvidenceDifferential {
		t.Fatalf("evidence kind = %s", evidence.Kind)
	}
	if evidence.Result != domain.ClaimTested {
		t.Fatalf("evidence result = %s, want TESTED_WITHIN_SCOPE (cases=%s)", evidence.Result, evidence.Scope.Details["cases"])
	}
	if evidence.Scope.GeneratedCases < 100 {
		t.Fatalf("generated cases = %d, want at least 100", evidence.Scope.GeneratedCases)
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}
	if result.Outputs["result_digest"] == "" {
		t.Fatal("successful result must carry a result_digest output")
	}
	if len(result.Artifacts) != 3 {
		t.Fatalf("artifact count = %d, want 3", len(result.Artifacts))
	}
}

func TestDifferentialCounterexample(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	headDir := filepath.Join(root, "head")
	workspace := filepath.Join(root, "workspace")
	writeModule(t, baseDir, "package calc\n\nfunc Apply(x int) int { return x*2 + 1 }\n")
	writeModule(t, headDir, "package calc\n\nfunc Apply(x int) int { return x*2 + 2 }\n")
	// contract.ref must resolve relative to the head snapshot root.
	writeFixtureFile(t, filepath.Join(headDir, "contracts", "calc.yaml"), contractYAML("example.com/fixture/calc.Apply", "behavior-preservation", "[3]"))

	request := baseRequest(t, baseDir, headDir, workspace, "contracts/calc.yaml", false)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s: %s)", result.Outcome, result.ErrorClass, result.Message)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Result != domain.ClaimViolated {
		t.Fatalf("expected VIOLATED evidence, got %+v", result.Evidence)
	}
	if len(result.Counterexamples) == 0 {
		t.Fatal("expected at least one counterexample")
	}
	if len(result.Counterexamples) > 3 {
		t.Fatalf("expected at most 3 counterexamples, got %d", len(result.Counterexamples))
	}
	store := request.Store.(*memoryStore)
	for _, counterexample := range result.Counterexamples {
		if !counterexample.Reproduced {
			t.Fatalf("counterexample %s was not reproduced", counterexample.CaseID)
		}
		if !counterexample.Minimized {
			t.Fatalf("counterexample %s was not minimized", counterexample.CaseID)
		}
		if err := counterexample.Validate(); err != nil {
			t.Fatalf("counterexample invalid: %v", err)
		}
		input := store.content(t, counterexample.InputRef)
		if !strings.HasPrefix(string(input), "[") {
			t.Fatalf("input artifact is not a JSON array: %s", input)
		}
	}
	first := store.content(t, result.Counterexamples[0].InputRef)
	if string(first) != "[0]" {
		t.Fatalf("expected the minimal input [0], got %s", first)
	}
	if result.Evidence[0].Scope.Details["distinct_counterexamples"] == "0" {
		t.Fatal("distinct counterexample count missing from evidence details")
	}
	if err := result.Evidence[0].Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}
}

func TestDifferentialContractParseError(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	headDir := filepath.Join(root, "head")
	workspace := filepath.Join(root, "workspace")
	writeModule(t, baseDir, "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeModule(t, headDir, "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeFixtureFile(t, filepath.Join(baseDir, "contract.yaml"), "apiVersion: patchproof.io/v1\nkind: Contract\nmetadata: {name: broken}\nspec:\n  unknownField: true\n")

	request := baseRequest(t, baseDir, headDir, workspace, filepath.Join(baseDir, "contract.yaml"), true)
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

func TestDifferentialInvalidSymbol(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	headDir := filepath.Join(root, "head")
	workspace := filepath.Join(root, "workspace")
	writeModule(t, baseDir, "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeModule(t, headDir, "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeFixtureFile(t, filepath.Join(baseDir, "contract.yaml"), contractYAML("example.com/fixture/calc.Missing", "behavior-preservation", "[1]"))

	request := baseRequest(t, baseDir, headDir, workspace, filepath.Join(baseDir, "contract.yaml"), true)
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
	if len(result.Evidence) != 0 {
		t.Fatal("unsupported target must not produce evidence")
	}
}

func TestDifferentialCompositeTypes(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	headDir := filepath.Join(root, "head")
	workspace := filepath.Join(root, "workspace")
	source := `package calc

import "time"

type Config struct {
	Name  string
	Count int
}

func Score(flag bool, text string, values []int, index map[string]int, ptr *int, cfg Config, ratio float64, wait time.Duration) int {
	total := len(text)
	for _, value := range values {
		total += value
	}
	for _, value := range index {
		total += value
	}
	if ptr != nil {
		total += *ptr
	}
	if flag {
		total += cfg.Count
	}
	if ratio > 0 {
		total += int(ratio)
	}
	return total + int(wait)
}
`
	writeModule(t, baseDir, source)
	writeModule(t, headDir, source)
	contract := `apiVersion: patchproof.io/v1
kind: Contract
metadata:
  name: calc.score
spec:
  symbol: example.com/fixture/calc.Score
  language: go
  kind: behavior-preservation
  domain:
    description: heterogeneous parameter domain
  oracle:
    type: json-differential/v1
  budget:
    maxCases: 300
`
	writeFixtureFile(t, filepath.Join(baseDir, "contract.yaml"), contract)

	request := baseRequest(t, baseDir, headDir, workspace, filepath.Join(baseDir, "contract.yaml"), true)
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
	validCases := result.Evidence[0].Scope.GeneratedCases
	if validCases < 100 {
		t.Fatalf("valid cases = %d, want at least 100", validCases)
	}
	invalid := result.Evidence[0].Scope.Details["invalid_cases"]
	if invalid == "" {
		t.Fatal("invalid case accounting missing from evidence details")
	}
	if len(result.Counterexamples) != 0 {
		t.Fatalf("expected no counterexamples, got %d", len(result.Counterexamples))
	}
}

func TestDifferentialUnknownWhenBelowMinimum(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	headDir := filepath.Join(root, "head")
	workspace := filepath.Join(root, "workspace")
	writeModule(t, baseDir, "package calc\n\nfunc Apply(x int) int { return x }\n")
	writeModule(t, headDir, "package calc\n\nfunc Apply(x int) int { return x }\n")
	contract := `apiVersion: patchproof.io/v1
kind: Contract
metadata:
  name: calc.apply
spec:
  symbol: example.com/fixture/calc.Apply
  language: go
  kind: behavior-preservation
  domain:
    description: arbitrary signed integers
  oracle:
    type: json-differential/v1
  budget:
    maxCases: 1
`
	writeFixtureFile(t, filepath.Join(baseDir, "contract.yaml"), contract)

	request := baseRequest(t, baseDir, headDir, workspace, filepath.Join(baseDir, "contract.yaml"), true)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s: %s)", result.Outcome, result.ErrorClass, result.Message)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Result != domain.ClaimUnknown {
		t.Fatalf("expected UNKNOWN evidence below the minimum case count, got %+v", result.Evidence)
	}
	if err := result.Evidence[0].Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}
	if len(result.Counterexamples) != 0 {
		t.Fatalf("expected no counterexamples, got %d", len(result.Counterexamples))
	}
}
