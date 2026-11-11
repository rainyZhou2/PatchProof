package gobuild

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
	if _, ok := s.blobs[ref.Hex()]; !ok {
		return ports.BlobInfo{}, fmt.Errorf("blob %s not found", ref.Hex())
	}
	return ports.BlobInfo{Ref: ref, Namespace: scope.Namespace()}, nil
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

// stagedDir builds a staged head snapshot input.
func stagedDir(path string) engine.StagedInput {
	return engine.StagedInput{
		Name: "head",
		Path: path,
		Ref:  domain.NewCASRef(fmt.Sprintf("%064x", 1), 1024),
		Kind: "tar.gz",
	}
}

// testRequest builds a valid go-build request.
func testRequest(t *testing.T, kind domain.StepKind, staged map[string]engine.StagedInput, options map[string]string, deadline time.Time) (engine.Request, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	if deadline.IsZero() {
		deadline = time.Now().Add(3 * time.Minute)
	}
	request := engine.Request{
		Spec: domain.StepSpec{
			RunID:    testRunID,
			StepID:   testStepID,
			TenantID: testTenantID,
			Kind:     kind,
			Language: "go",
			Engine:   EngineName,
			Options:  options,
			Deadline: deadline,
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

// artifactText returns a text artifact referenced by a result output.
func artifactText(t *testing.T, store *memoryStore, result engine.Result, outputKey string) string {
	t.Helper()
	digest := result.Outputs[outputKey]
	if digest == "" {
		t.Fatalf("result output %q is empty", outputKey)
	}
	raw := store.blob(digest)
	if raw == nil {
		t.Fatalf("artifact %s for output %q was not stored", digest, outputKey)
	}
	return string(raw)
}

// decodeTestResults decodes the test-results.json artifact.
func decodeTestResults(t *testing.T, store *memoryStore, result engine.Result) TestResultsBundle {
	t.Helper()
	raw := artifactText(t, store, result, "results.digest")
	var bundle TestResultsBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		t.Fatalf("decode test results: %v", err)
	}
	return bundle
}

// moduleWith writes a simple Go module and returns its root.
func moduleWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	base := map[string]string{"go.mod": "module example.com/buildtest\n\ngo 1.21\n"}
	for path, content := range files {
		base[path] = content
	}
	writeTree(t, root, base)
	return root
}
