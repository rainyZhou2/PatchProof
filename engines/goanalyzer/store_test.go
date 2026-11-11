package goanalyzer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
)

// memoryStore is an in-memory artifact store for engine tests.
type memoryStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMemoryStore() *memoryStore {
	return &memoryStore{blobs: make(map[string][]byte)}
}

func (s *memoryStore) Put(ctx context.Context, scope ports.SecurityScope, reader io.Reader) (domain.CASRef, error) {
	if err := ctx.Err(); err != nil {
		return domain.CASRef{}, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, 256<<20))
	if err != nil {
		return domain.CASRef{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	s.mu.Lock()
	s.blobs[digest] = data
	s.mu.Unlock()
	return domain.NewCASRef(digest, int64(len(data))), nil
}

func (s *memoryStore) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.blobs[ref.Hex()]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", ref.Hex())
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (s *memoryStore) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.blobs[ref.Hex()]
	if !ok {
		return ports.BlobInfo{}, fmt.Errorf("blob %s not found", ref.Hex())
	}
	return ports.BlobInfo{Ref: ref, Namespace: scope.Namespace(), ContentType: "application/json", Metadata: map[string]string{"size": fmt.Sprintf("%d", len(data))}}, nil
}

func (s *memoryStore) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blobs, ref.Hex())
	return nil
}

func (s *memoryStore) Health(ctx context.Context) error { return ctx.Err() }

func (s *memoryStore) blob(digest string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.blobs[digest]
	if !ok {
		return nil
	}
	return append([]byte(nil), data...)
}

const (
	testTenantID = domain.TenantID("11111111-1111-4111-8111-111111111111")
	testRunID    = domain.RunID("22222222-2222-4222-8222-222222222222")
	testStepID   = domain.StepID("33333333-3333-4333-8333-333333333333")
)

// writeTree materializes a map of relative paths to file contents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// testRequest builds a valid analyzer request for staged snapshots.
func testRequest(t *testing.T, staged map[string]engine.StagedInput, options map[string]string) (engine.Request, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	request := engine.Request{
		Spec: domain.StepSpec{
			RunID:    testRunID,
			StepID:   testStepID,
			TenantID: testTenantID,
			Kind:     domain.StepKindAnalyze,
			Language: "go",
			Engine:   EngineName,
			Options:  options,
			Deadline: time.Now().Add(2 * time.Minute),
			Requirements: domain.ResourceRequirements{
				Architecture: "arm64",
				Isolation:    domain.IsolationContainer,
			},
		},
		TenantID:  testTenantID,
		Workspace: t.TempDir(),
		Staged:    staged,
		Store:     store,
		Scope:     ports.SecurityScope{TenantID: testTenantID, TrustDomain: "test"},
	}
	return request, store
}

// stagedDir builds a staged input for a snapshot directory.
func stagedDir(path, digestSeed string) engine.StagedInput {
	return engine.StagedInput{
		Name: filepath.Base(path),
		Path: path,
		Ref:  domain.NewCASRef(digestSeed, 1024),
		Kind: "tar.gz",
	}
}

// decodeArtifact decodes a JSON artifact referenced by result outputs.
func decodeArtifact[T any](t *testing.T, store *memoryStore, result engine.Result, outputKey string) T {
	t.Helper()
	digest := result.Outputs[outputKey]
	if digest == "" {
		t.Fatalf("result output %q is empty", outputKey)
	}
	raw := store.blob(digest)
	if raw == nil {
		t.Fatalf("artifact %s for output %q was not stored", digest, outputKey)
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s artifact: %v", outputKey, err)
	}
	return value
}

// hasSymbol reports whether the IR contains an exact symbol ID.
func hasSymbol(ir IRBundle, id string) bool {
	for _, symbol := range ir.Symbols {
		if symbol.ID == id {
			return true
		}
	}
	return false
}

// hasEdge reports whether the IR contains a call edge with the given resolution.
func hasEdge(ir IRBundle, caller, callee, resolution string) bool {
	for _, edge := range ir.CallEdges {
		if edge.Caller == caller && edge.Callee == callee && edge.Resolution == resolution {
			return true
		}
	}
	return false
}

// hasString reports whether values contains target.
func hasString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
