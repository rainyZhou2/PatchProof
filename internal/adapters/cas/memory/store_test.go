package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

func testScope() ports.SecurityScope {
	return ports.SecurityScope{
		TenantID:    domain.NewTenantID(),
		TrustDomain: "test.patchproof.example",
		Region:      "region-a",
	}
}

func readAll(t *testing.T, store *Store, scope ports.SecurityScope, ref domain.CASRef) []byte {
	t.Helper()
	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return data
}

func TestPutOpenStatDelete(t *testing.T) {
	store := New()
	scope := testScope()
	for _, content := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("evidence"), 4096)} {
		ref, err := store.Put(context.Background(), scope, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		sum := sha256.Sum256(content)
		if ref.Digest != hex.EncodeToString(sum[:]) || ref.SizeBytes != int64(len(content)) {
			t.Fatalf("unexpected ref %+v for %d bytes", ref, len(content))
		}
		if got := readAll(t, store, scope, ref); !bytes.Equal(got, content) {
			t.Fatalf("roundtrip mismatch for %d bytes", len(content))
		}
		info, err := store.Stat(context.Background(), scope, ref)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Ref.Digest != ref.Digest || info.Ref.SizeBytes != ref.SizeBytes {
			t.Fatalf("unexpected stat %+v", info)
		}
		if info.Namespace != scope.Namespace() {
			t.Fatalf("namespace = %q, want %q", info.Namespace, scope.Namespace())
		}
		if info.CreatedAt.IsZero() {
			t.Fatal("expected creation time")
		}
		if err := store.Delete(context.Background(), scope, ref); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := store.Open(context.Background(), scope, ref); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("open deleted error = %v, want os.ErrNotExist", err)
		}
		if err := store.Delete(context.Background(), scope, ref); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
	}
	if err := store.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestPutCopiesInput(t *testing.T) {
	store := New()
	scope := testScope()
	source := []byte("mutable source")
	ref, err := store.Put(context.Background(), scope, bytes.NewReader(source))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	source[0] = 'M'
	if got := readAll(t, store, scope, ref); string(got) != "mutable source" {
		t.Fatalf("stored content = %q, want original input", got)
	}

	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buffer := make([]byte, len(source))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Fatalf("read: %v", err)
	}
	reader.Close()
	buffer[0] = 'X'
	if got := readAll(t, store, scope, ref); string(got) != "mutable source" {
		t.Fatalf("stored content changed after reader mutation: %q", got)
	}
}

func TestOpenDetectsCorruption(t *testing.T) {
	store := New()
	scope := testScope()
	content := []byte("hello world")
	ref, err := store.Put(context.Background(), scope, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		t.Fatalf("blob key: %v", err)
	}
	store.mu.Lock()
	entry := store.blobs[key]
	entry.data[0] = 'H'
	store.blobs[key] = entry
	store.mu.Unlock()

	if _, err := store.Open(context.Background(), scope, ref); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("open corrupted blob error = %v, want ErrIntegrity", err)
	}
	wrongSize := ref
	wrongSize.SizeBytes++
	if _, err := store.Open(context.Background(), scope, wrongSize); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("open wrong size error = %v, want ErrIntegrity", err)
	}
}

type failingReader struct {
	read int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.read == 0 {
		f.read++
		return copy(p, "partial"), nil
	}
	return 0, errors.New("input failed")
}

func TestPutReaderFailure(t *testing.T) {
	store := New()
	if _, err := store.Put(context.Background(), testScope(), &failingReader{}); err == nil {
		t.Fatal("expected put error")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.blobs) != 0 {
		t.Fatalf("store contains %d blobs after failed put", len(store.blobs))
	}
}

func TestInvalidScopeAndRef(t *testing.T) {
	store := New()
	valid := testScope()
	badScope := valid
	badScope.TrustDomain = "../escape"
	if _, err := store.Put(context.Background(), badScope, bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("expected put to reject invalid scope")
	}
	badRef := domain.CASRef{Algorithm: "sha256", Digest: "not-a-digest", SizeBytes: 1}
	if _, err := store.Open(context.Background(), valid, badRef); err == nil {
		t.Fatal("expected open to reject invalid ref")
	}
	if _, err := store.Stat(context.Background(), valid, badRef); err == nil {
		t.Fatal("expected stat to reject invalid ref")
	}
	if err := store.Delete(context.Background(), valid, badRef); err == nil {
		t.Fatal("expected delete to reject invalid ref")
	}
}

func TestConcurrentPut(t *testing.T) {
	store := New()
	scope := testScope()
	content := []byte("concurrent")
	const workers = 32
	refs := make([]domain.CASRef, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			refs[index], errs[index] = store.Put(context.Background(), scope, bytes.NewReader(content))
		}(i)
	}
	group.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("put %d: %v", i, errs[i])
		}
		if refs[i] != refs[0] {
			t.Fatalf("put %d ref = %+v, want %+v", i, refs[i], refs[0])
		}
	}
	if got := readAll(t, store, scope, refs[0]); !bytes.Equal(got, content) {
		t.Fatal("content mismatch")
	}
}

func TestContextCancelled(t *testing.T) {
	store := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, testScope(), bytes.NewReader([]byte("x"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("put error = %v, want context.Canceled", err)
	}
	if err := store.Health(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("health error = %v, want context.Canceled", err)
	}
}
