package localfs

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
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	return store
}

func testScope() ports.SecurityScope {
	return ports.SecurityScope{
		TenantID:    domain.NewTenantID(),
		TrustDomain: "run",
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

func blobPath(t *testing.T, store *Store, scope ports.SecurityScope, ref domain.CASRef) string {
	t.Helper()
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		t.Fatalf("blob key: %v", err)
	}
	return filepath.Join(store.Root(), filepath.FromSlash(key))
}

func TestRoundtrip(t *testing.T) {
	store := newTestStore(t)
	scope := testScope()
	content := bytes.Repeat([]byte("offline-artifact"), 256)

	ref, err := store.Put(context.Background(), scope, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	sum := sha256.Sum256(content)
	if ref.Digest != hex.EncodeToString(sum[:]) || ref.SizeBytes != int64(len(content)) {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	if got := readAll(t, store, scope, ref); !bytes.Equal(got, content) {
		t.Fatal("roundtrip content mismatch")
	}

	stat, err := store.Stat(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Ref.Digest != ref.Digest || stat.Ref.SizeBytes != ref.SizeBytes {
		t.Fatalf("stat = %+v", stat)
	}
	if stat.Namespace != scope.Namespace() {
		t.Fatalf("namespace = %q, want %q", stat.Namespace, scope.Namespace())
	}

	blobs, err := store.Blobs(context.Background())
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	if len(blobs) != 1 || blobs[0].Ref.Digest != ref.Digest || blobs[0].Ref.SizeBytes != ref.SizeBytes {
		t.Fatalf("blobs = %+v", blobs)
	}

	if err := store.Delete(context.Background(), scope, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Open(context.Background(), scope, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open deleted blob error = %v, want os.ErrNotExist", err)
	}
	if err := store.Delete(context.Background(), scope, ref); err != nil {
		t.Fatalf("delete missing blob: %v", err)
	}
	if err := store.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestOpenDetectsForgedDigest(t *testing.T) {
	store := newTestStore(t)
	scope := testScope()
	ref, err := store.Put(context.Background(), scope, strings.NewReader("trusted bytes"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	path := blobPath(t, store, scope, ref)
	if err := os.WriteFile(path, []byte("forged! bytes"), 0o600); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}

	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("read tampered blob error = %v, want ErrIntegrity", err)
	}
	if err := reader.Close(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("close tampered blob error = %v, want ErrIntegrity", err)
	}

	forged := ref
	forged.SizeBytes++
	if _, err := store.Open(context.Background(), scope, forged); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("open with forged size error = %v, want ErrIntegrity", err)
	}

	forgedAlgorithm := ref
	forgedAlgorithm.Algorithm = "sha512"
	if _, err := store.Open(context.Background(), scope, forgedAlgorithm); err == nil {
		t.Fatal("expected open to reject a non-sha256 ref")
	}
}

func TestWrongScope(t *testing.T) {
	store := newTestStore(t)
	owner := testScope()
	ref, err := store.Put(context.Background(), owner, strings.NewReader("scoped"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	other := testScope()
	if _, err := store.Open(context.Background(), other, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open with wrong scope error = %v, want os.ErrNotExist", err)
	}
	if _, err := store.Stat(context.Background(), other, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat with wrong scope error = %v, want os.ErrNotExist", err)
	}

	for _, scope := range []ports.SecurityScope{
		{TrustDomain: "run"},
		{TenantID: owner.TenantID, TrustDomain: "../escape"},
		{TenantID: domain.TenantID("../../etc"), TrustDomain: "run"},
	} {
		if _, err := store.Put(context.Background(), scope, strings.NewReader("payload")); err == nil {
			t.Fatalf("expected put to reject scope %+v", scope)
		}
	}
}

func TestMissingBlob(t *testing.T) {
	store := newTestStore(t)
	ref := domain.NewCASRef(strings.Repeat("a", 64), 3)
	_, err := store.Open(context.Background(), testScope(), ref)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open missing blob error = %v, want os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), ref.URI()) {
		t.Fatalf("error %q does not mention %s", err, ref.URI())
	}
	if _, err := store.Stat(context.Background(), testScope(), ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat missing blob error = %v, want os.ErrNotExist", err)
	}
}

func TestBlobsIgnoresNonCASFiles(t *testing.T) {
	store := newTestStore(t)
	scope := testScope()
	ref, err := store.Put(context.Background(), scope, strings.NewReader("artifact"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	for name, content := range map[string]string{
		"result.json":         `{"result":{}}`,
		"input-manifest.json": `{"inputs":{}}`,
	} {
		if err := os.WriteFile(filepath.Join(store.Root(), name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(store.Root(), ".tmp", "scratch"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	blobs, err := store.Blobs(context.Background())
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	if len(blobs) != 1 || blobs[0].Ref.Digest != ref.Digest {
		t.Fatalf("blobs = %+v", blobs)
	}

	// A non-canonical fan-out directory must not be treated as a blob.
	digest := strings.Repeat("b", 64)
	badDir := filepath.Join(store.Root(), "cas", string(scope.TenantID), scope.TrustDomain, "sha256", "ff", "ff")
	if err := os.MkdirAll(badDir, 0o750); err != nil {
		t.Fatalf("mkdir bad dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, digest), []byte("not canonical"), 0o600); err != nil {
		t.Fatalf("write bad blob: %v", err)
	}
	if _, err := store.Blobs(context.Background()); err == nil {
		t.Fatal("expected a non-canonical blob key to be rejected")
	}
}

func TestBlobsEmptyRoot(t *testing.T) {
	store := newTestStore(t)
	blobs, err := store.Blobs(context.Background())
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	if len(blobs) != 0 {
		t.Fatalf("blobs = %+v, want none", blobs)
	}
	if err := os.RemoveAll(filepath.Join(store.Root(), "cas")); err != nil {
		t.Fatalf("remove cas tree: %v", err)
	}
	blobs, err = store.Blobs(context.Background())
	if err != nil || len(blobs) != 0 {
		t.Fatalf("blobs after removal = %+v, %v", blobs, err)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("expected error for empty root")
	}
	if _, err := New("   "); err == nil {
		t.Fatal("expected error for blank root")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := New(file); err == nil {
		t.Fatal("expected error for non-directory root")
	}
}
