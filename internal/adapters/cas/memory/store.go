// Package memory provides an in-memory content-addressed artifact store for
// tests and local single-process operation.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

const copyBufferSize = 128 << 10

// ErrIntegrity reports a blob whose bytes do not match its content address.
var ErrIntegrity = errors.New("artifact integrity check failed")

// Store keeps blobs in process memory. It is safe for concurrent use.
type Store struct {
	mu    sync.RWMutex
	blobs map[string]blob
}

type blob struct {
	data      []byte
	createdAt time.Time
}

var _ ports.ArtifactStore = (*Store)(nil)

// New creates an empty in-memory artifact store.
func New() *Store {
	return &Store{blobs: make(map[string]blob)}
}

// Put buffers and copies r, then stores it under its SHA-256 content address.
func (s *Store) Put(ctx context.Context, scope ports.SecurityScope, r io.Reader) (domain.CASRef, error) {
	if err := scope.Validate(); err != nil {
		return domain.CASRef{}, fmt.Errorf("memory cas put: %w", err)
	}
	if r == nil {
		return domain.CASRef{}, errors.New("memory cas put: reader must not be nil")
	}
	digest := sha256.New()
	var buffer bytes.Buffer
	written, err := copyBlob(ctx, io.MultiWriter(digest, &buffer), r)
	if err != nil {
		return domain.CASRef{}, err
	}
	hexDigest := hex.EncodeToString(digest.Sum(nil))
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, hexDigest)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("memory cas put: %w", err)
	}
	ref := domain.NewCASRef(hexDigest, written)
	data := make([]byte, buffer.Len())
	copy(data, buffer.Bytes())

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.blobs[key]; ok {
		if int64(len(existing.data)) != written {
			return domain.CASRef{}, fmt.Errorf("%w: blob %s has size %d, expected %d", ErrIntegrity, ref.URI(), len(existing.data), written)
		}
		return ref, nil
	}
	s.blobs[key] = blob{data: data, createdAt: time.Now().UTC()}
	return ref, nil
}

// Open returns a reader over a private copy of the stored bytes after
// verifying their digest and size.
func (s *Store) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	entry, err := s.lookup(scope, ref)
	if err != nil {
		return nil, fmt.Errorf("memory cas open: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(entry.data)
	if actual := hex.EncodeToString(sum[:]); actual != ref.Hex() {
		return nil, fmt.Errorf("%w: blob %s has digest %s", ErrIntegrity, ref.URI(), actual)
	}
	view := make([]byte, len(entry.data))
	copy(view, entry.data)
	return io.NopCloser(bytes.NewReader(view)), nil
}

// Stat returns metadata for a stored blob.
func (s *Store) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	entry, err := s.lookup(scope, ref)
	if err != nil {
		return ports.BlobInfo{}, fmt.Errorf("memory cas stat: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ports.BlobInfo{}, err
	}
	return ports.BlobInfo{
		Ref:       domain.NewCASRef(ref.Hex(), int64(len(entry.data))),
		Namespace: scope.Namespace(),
		CreatedAt: entry.createdAt,
	}, nil
}

// Delete removes a blob. Deleting a missing blob succeeds.
func (s *Store) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("memory cas delete: %w", err)
	}
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("memory cas delete: %w", err)
	}
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		return fmt.Errorf("memory cas delete: %w", err)
	}
	s.mu.Lock()
	delete(s.blobs, key)
	s.mu.Unlock()
	return nil
}

// Health reports whether the store is usable. The in-memory store is always
// healthy while the context is active.
func (s *Store) Health(ctx context.Context) error {
	return ctx.Err()
}

func (s *Store) lookup(scope ports.SecurityScope, ref domain.CASRef) (blob, error) {
	if err := scope.Validate(); err != nil {
		return blob{}, err
	}
	if err := ref.Validate(); err != nil {
		return blob{}, err
	}
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		return blob{}, err
	}
	s.mu.RLock()
	entry, ok := s.blobs[key]
	s.mu.RUnlock()
	if !ok {
		return blob{}, fmt.Errorf("blob %s: %w", ref.URI(), os.ErrNotExist)
	}
	if ref.SizeBytes != 0 && int64(len(entry.data)) != ref.SizeBytes {
		return blob{}, fmt.Errorf("%w: blob %s has size %d, expected %d", ErrIntegrity, ref.URI(), len(entry.data), ref.SizeBytes)
	}
	return entry, nil
}

func copyBlob(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written += int64(n)
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return written, fmt.Errorf("write blob: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, fmt.Errorf("read blob: %w", readErr)
		}
	}
}
