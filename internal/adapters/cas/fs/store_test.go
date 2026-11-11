package fs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

func newTestStore(t *testing.T, maxBlobBytes int64) *Store {
	t.Helper()
	store, err := New(Config{Root: t.TempDir(), MaxBlobBytes: maxBlobBytes, CreateRoot: true})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

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
		t.Fatalf("open %s: %v", ref.URI(), err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		reader.Close()
		t.Fatalf("read %s: %v", ref.URI(), err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close %s: %v", ref.URI(), err)
	}
	return data
}

func TestPutOpenStatDelete(t *testing.T) {
	store := newTestStore(t, 0)
	scope := testScope()
	content := bytes.Repeat([]byte("patchproof-evidence"), 1024)

	ref, err := store.Put(context.Background(), scope, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	sum := sha256.Sum256(content)
	if ref.Algorithm != "sha256" || ref.Digest != hex.EncodeToString(sum[:]) || ref.SizeBytes != int64(len(content)) {
		t.Fatalf("unexpected ref: %+v", ref)
	}

	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		t.Fatalf("blob key: %v", err)
	}
	path := filepath.Join(store.root, filepath.FromSlash(key))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat stored blob: %v", err)
	}
	if info.Size() != int64(len(content)) {
		t.Fatalf("stored size = %d, want %d", info.Size(), len(content))
	}
	if info.Mode().Perm()&0o600 != 0o600 || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("stored file mode = %v, want owner read/write only", info.Mode())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat shard directory: %v", err)
	}
	if dirInfo.Mode().Perm()&0o700 != 0o700 {
		t.Fatalf("shard directory mode = %v, want owner rwx", dirInfo.Mode())
	}

	if got := readAll(t, store, scope, ref); !bytes.Equal(got, content) {
		t.Fatalf("roundtrip content mismatch")
	}

	stat, err := store.Stat(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Ref.Digest != ref.Digest || stat.Ref.SizeBytes != ref.SizeBytes {
		t.Fatalf("unexpected stat ref: %+v", stat.Ref)
	}
	if stat.Namespace != scope.Namespace() {
		t.Fatalf("namespace = %q, want %q", stat.Namespace, scope.Namespace())
	}
	if stat.CreatedAt.IsZero() {
		t.Fatal("expected non-zero creation time")
	}

	if err := store.Delete(context.Background(), scope, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Open(context.Background(), scope, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open deleted blob error = %v, want os.ErrNotExist", err)
	}
	if _, err := store.Stat(context.Background(), scope, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat deleted blob error = %v, want os.ErrNotExist", err)
	}
	if err := store.Delete(context.Background(), scope, ref); err != nil {
		t.Fatalf("delete missing blob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "cas")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty blob tree to be pruned, stat error = %v", err)
	}
	if err := store.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestOpenDetectsCorruption(t *testing.T) {
	store := newTestStore(t, 0)
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
	path := filepath.Join(store.root, filepath.FromSlash(key))
	if err := os.WriteFile(path, []byte("HELLO WORLD"), 0o600); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}

	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open corrupted blob: %v", err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("read corrupted blob error = %v, want ErrIntegrity", err)
	}
	if err := reader.Close(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("close corrupted blob error = %v, want ErrIntegrity", err)
	}

	wrongSize := ref
	wrongSize.SizeBytes++
	if _, err := store.Open(context.Background(), scope, wrongSize); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("open with wrong size error = %v, want ErrIntegrity", err)
	}
}

func TestOpenExactReadAllowsClose(t *testing.T) {
	store := newTestStore(t, 0)
	scope := testScope()
	content := []byte("exact read")
	ref, err := store.Put(context.Background(), scope, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buffer := make([]byte, len(content))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Fatalf("read exact size: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close after exact read: %v", err)
	}
}

func TestOpenCloseBeforeEOF(t *testing.T) {
	store := newTestStore(t, 0)
	scope := testScope()
	ref, err := store.Put(context.Background(), scope, strings.NewReader("partial content"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buffer := make([]byte, 3)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	if err := reader.Close(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("close before EOF error = %v, want ErrIntegrity", err)
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

func TestPutReaderFailureCleansUp(t *testing.T) {
	store := newTestStore(t, 0)
	scope := testScope()
	if _, err := store.Put(context.Background(), scope, &failingReader{}); err == nil {
		t.Fatal("expected put error for failing reader")
	}
	assertNoBlobFiles(t, store)
}

func TestPutConcurrentSameContent(t *testing.T) {
	store := newTestStore(t, 0)
	scope := testScope()
	content := []byte("concurrent content")
	const workers = 16
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
	for i, err := range errs {
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		if refs[i] != refs[0] {
			t.Fatalf("put %d returned %+v, want %+v", i, refs[i], refs[0])
		}
	}
	if got := readAll(t, store, scope, refs[0]); !bytes.Equal(got, content) {
		t.Fatal("stored content mismatch after concurrent puts")
	}
	if count := countBlobFiles(t, store); count != 1 {
		t.Fatalf("stored blob count = %d, want 1", count)
	}
}

func TestPutRejectsPathTraversal(t *testing.T) {
	store := newTestStore(t, 0)
	base := testScope()
	badScopes := []ports.SecurityScope{
		{TenantID: base.TenantID, TrustDomain: "../escape"},
		{TenantID: base.TenantID, TrustDomain: "a/b"},
		{TenantID: domain.TenantID("../../etc"), TrustDomain: base.TrustDomain},
	}
	for _, scope := range badScopes {
		if _, err := store.Put(context.Background(), scope, strings.NewReader("payload")); err == nil {
			t.Fatalf("expected put to reject scope %+v", scope)
		}
	}
	assertNoBlobFiles(t, store)

	for _, key := range []string{"../escape", "/etc/passwd", "cas/../../escape", "cas//blob"} {
		if _, err := store.resolve(key); err == nil {
			t.Fatalf("expected resolve to reject key %q", key)
		}
	}

	badRef := domain.CASRef{Algorithm: "sha256", Digest: "../../escape", SizeBytes: 1}
	if _, err := store.Open(context.Background(), base, badRef); err == nil {
		t.Fatal("expected open to reject invalid digest")
	}
}

func TestPutMaxBlobBytes(t *testing.T) {
	store := newTestStore(t, 4)
	scope := testScope()
	if _, err := store.Put(context.Background(), scope, strings.NewReader("abcd")); err != nil {
		t.Fatalf("put at limit: %v", err)
	}
	if _, err := store.Put(context.Background(), scope, strings.NewReader("abcde")); !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("put over limit error = %v, want ErrBlobTooLarge", err)
	}
	if count := countBlobFiles(t, store); count != 1 {
		t.Fatalf("stored blob count = %d, want 1", count)
	}
	entries, err := os.ReadDir(store.tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir contains %d leftover entries", len(entries))
	}
}

func TestPutContextCancelled(t *testing.T) {
	store := newTestStore(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, testScope(), strings.NewReader("payload")); !errors.Is(err, context.Canceled) {
		t.Fatalf("put error = %v, want context.Canceled", err)
	}
}

func countBlobFiles(t *testing.T, store *Store) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(store.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Dir(path) == store.tempDir {
			return nil
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk store root: %v", err)
	}
	return count
}

func assertNoBlobFiles(t *testing.T, store *Store) {
	t.Helper()
	if count := countBlobFiles(t, store); count != 0 {
		t.Fatalf("expected no stored blobs, found %d", count)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty root")
	}
	if _, err := New(Config{Root: t.TempDir(), MaxBlobBytes: -1}); err == nil {
		t.Fatal("expected error for negative max blob bytes")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := New(Config{Root: file}); err == nil {
		t.Fatal("expected error for non-directory root")
	}
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("expected error for missing root without CreateRoot")
	}
}

func TestMissingBlobErrorWrapsNotExist(t *testing.T) {
	store := newTestStore(t, 0)
	scope := testScope()
	ref := domain.NewCASRef(strings.Repeat("a", 64), 3)
	_, err := store.Open(context.Background(), scope, ref)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open error = %v, want os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), ref.URI()) {
		t.Fatalf("error %q does not mention %s", err, ref.URI())
	}
}
