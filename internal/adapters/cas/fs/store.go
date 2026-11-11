// Package fs provides a content-addressed artifact store backed by a local
// filesystem. Blobs are addressed by SHA-256 digest and laid out using
// pkg/cas.BlobKey inside a single root directory.
package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

const (
	directoryPerm  = 0o750
	tempDirName    = ".tmp"
	copyBufferSize = 128 << 10
)

var (
	// ErrIntegrity reports a blob whose stored bytes no longer match its
	// content address or declared size.
	ErrIntegrity = errors.New("artifact integrity check failed")
	// ErrBlobTooLarge reports a blob that exceeds Config.MaxBlobBytes.
	ErrBlobTooLarge = errors.New("artifact exceeds maximum blob size")
)

// Config configures a filesystem artifact store.
type Config struct {
	// Root is the directory that contains all blobs.
	Root string
	// MaxBlobBytes limits a single blob when greater than zero.
	MaxBlobBytes int64
	// CreateRoot creates Root when it does not exist.
	CreateRoot bool
}

// Store is a filesystem-backed content-addressed artifact store.
type Store struct {
	root         string
	tempDir      string
	maxBlobBytes int64
}

var _ ports.ArtifactStore = (*Store)(nil)

// New creates a filesystem artifact store rooted at cfg.Root.
func New(cfg Config) (*Store, error) {
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		return nil, errors.New("fs cas: root must not be empty")
	}
	if cfg.MaxBlobBytes < 0 {
		return nil, errors.New("fs cas: max blob bytes must not be negative")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fs cas: resolve root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, fmt.Errorf("fs cas: root %s is not a directory", absolute)
		}
	case errors.Is(err, iofs.ErrNotExist):
		if !cfg.CreateRoot {
			return nil, fmt.Errorf("fs cas: root %s: %w", absolute, err)
		}
		if err := os.MkdirAll(absolute, directoryPerm); err != nil {
			return nil, fmt.Errorf("fs cas: create root %s: %w", absolute, err)
		}
	default:
		return nil, fmt.Errorf("fs cas: stat root %s: %w", absolute, err)
	}
	tempDir := filepath.Join(absolute, tempDirName)
	if err := os.MkdirAll(tempDir, directoryPerm); err != nil {
		return nil, fmt.Errorf("fs cas: create temp directory: %w", err)
	}
	return &Store{root: absolute, tempDir: tempDir, maxBlobBytes: cfg.MaxBlobBytes}, nil
}

// Put streams r into the store and returns the content address of the bytes
// that were written. The blob becomes visible atomically once fully synced.
func (s *Store) Put(ctx context.Context, scope ports.SecurityScope, r io.Reader) (domain.CASRef, error) {
	if err := scope.Validate(); err != nil {
		return domain.CASRef{}, fmt.Errorf("fs cas put: %w", err)
	}
	if r == nil {
		return domain.CASRef{}, errors.New("fs cas put: reader must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return domain.CASRef{}, err
	}
	if err := os.MkdirAll(s.tempDir, directoryPerm); err != nil {
		return domain.CASRef{}, fmt.Errorf("fs cas put: create temp directory: %w", err)
	}
	temp, err := os.CreateTemp(s.tempDir, "put-*.tmp")
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("fs cas put: create temp file: %w", err)
	}
	tempName := temp.Name()
	discard := func() {
		temp.Close()
		os.Remove(tempName)
	}

	digest := sha256.New()
	written, err := copyBlob(ctx, io.MultiWriter(digest, temp), r, s.maxBlobBytes)
	if err != nil {
		discard()
		return domain.CASRef{}, err
	}
	if err := temp.Sync(); err != nil {
		discard()
		return domain.CASRef{}, fmt.Errorf("fs cas put: sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return domain.CASRef{}, fmt.Errorf("fs cas put: close temp file: %w", err)
	}

	hexDigest := hex.EncodeToString(digest.Sum(nil))
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, hexDigest)
	if err != nil {
		os.Remove(tempName)
		return domain.CASRef{}, fmt.Errorf("fs cas put: %w", err)
	}
	target, err := s.resolve(key)
	if err != nil {
		os.Remove(tempName)
		return domain.CASRef{}, fmt.Errorf("fs cas put: %w", err)
	}
	ref := domain.NewCASRef(hexDigest, written)

	existing, statErr := os.Stat(target)
	switch {
	case statErr == nil:
		os.Remove(tempName)
		if existing.Size() != written {
			return domain.CASRef{}, fmt.Errorf("%w: blob %s has size %d, expected %d", ErrIntegrity, ref.URI(), existing.Size(), written)
		}
		return ref, nil
	case errors.Is(statErr, iofs.ErrNotExist):
	default:
		os.Remove(tempName)
		return domain.CASRef{}, fmt.Errorf("fs cas put: stat blob %s: %w", ref.URI(), statErr)
	}

	if err := os.MkdirAll(filepath.Dir(target), directoryPerm); err != nil {
		os.Remove(tempName)
		return domain.CASRef{}, fmt.Errorf("fs cas put: create blob directory: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		os.Remove(tempName)
		if info, statErr := os.Stat(target); statErr == nil && info.Size() == written {
			return ref, nil
		}
		return domain.CASRef{}, fmt.Errorf("fs cas put: publish blob %s: %w", ref.URI(), err)
	}
	return ref, nil
}

// Open returns a reader for the blob. The reader verifies the digest and size
// while streaming and reports ErrIntegrity on the read that reaches EOF.
func (s *Store) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	path, err := s.blobPath(scope, ref)
	if err != nil {
		return nil, fmt.Errorf("fs cas open: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fs cas open %s: %w", ref.URI(), err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("fs cas open %s: stat: %w", ref.URI(), err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("fs cas open %s: not a regular file", ref.URI())
	}
	if info.Size() != ref.SizeBytes {
		file.Close()
		return nil, fmt.Errorf("%w: blob %s has size %d, expected %d", ErrIntegrity, ref.URI(), info.Size(), ref.SizeBytes)
	}
	return newVerifyingReader(file, ref.Hex(), ref.SizeBytes), nil
}

// Stat returns metadata for a stored blob.
func (s *Store) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	path, err := s.blobPath(scope, ref)
	if err != nil {
		return ports.BlobInfo{}, fmt.Errorf("fs cas stat: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ports.BlobInfo{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return ports.BlobInfo{}, fmt.Errorf("fs cas stat %s: %w", ref.URI(), err)
	}
	if !info.Mode().IsRegular() {
		return ports.BlobInfo{}, fmt.Errorf("fs cas stat %s: not a regular file", ref.URI())
	}
	return ports.BlobInfo{
		Ref:       domain.NewCASRef(ref.Hex(), info.Size()),
		Namespace: scope.Namespace(),
		CreatedAt: info.ModTime().UTC(),
	}, nil
}

// Delete removes a blob and any empty parent directories. Deleting a missing
// blob succeeds.
func (s *Store) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	path, err := s.blobPath(scope, ref)
	if err != nil {
		return fmt.Errorf("fs cas delete: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("fs cas delete %s: %w", ref.URI(), err)
	}
	s.pruneEmptyParents(filepath.Dir(path))
	return nil
}

// Health verifies that the root is usable by writing, reading and deleting a
// probe file.
func (s *Store) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("fs cas health: stat root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("fs cas health: root %s is not a directory", s.root)
	}
	if err := os.MkdirAll(s.tempDir, directoryPerm); err != nil {
		return fmt.Errorf("fs cas health: create temp directory: %w", err)
	}
	probe, err := os.CreateTemp(s.tempDir, "health-*.tmp")
	if err != nil {
		return fmt.Errorf("fs cas health: create probe: %w", err)
	}
	name := probe.Name()
	defer os.Remove(name)
	defer probe.Close()

	payload := []byte("patchproof-cas-health")
	if _, err := probe.Write(payload); err != nil {
		return fmt.Errorf("fs cas health: write probe: %w", err)
	}
	if err := probe.Sync(); err != nil {
		return fmt.Errorf("fs cas health: sync probe: %w", err)
	}
	if _, err := probe.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("fs cas health: seek probe: %w", err)
	}
	read, err := io.ReadAll(io.LimitReader(probe, int64(len(payload))))
	if err != nil {
		return fmt.Errorf("fs cas health: read probe: %w", err)
	}
	if string(read) != string(payload) {
		return fmt.Errorf("fs cas health: probe content mismatch")
	}
	return nil
}

func (s *Store) blobPath(scope ports.SecurityScope, ref domain.CASRef) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		return "", err
	}
	digest, err := cas.DigestFromBlobKey(key)
	if err != nil {
		return "", err
	}
	if digest != ref.Hex() {
		return "", fmt.Errorf("blob key does not match digest %s", ref.URI())
	}
	return s.resolve(key)
}

// resolve converts a validated, slash-separated CAS key into an absolute path
// and rejects anything that would escape the root.
func (s *Store) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("blob key must not be empty")
	}
	if filepath.IsAbs(filepath.FromSlash(key)) {
		return "", fmt.Errorf("blob key %q must be relative", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("blob key %q contains an invalid path segment", key)
		}
	}
	full := filepath.Join(s.root, filepath.FromSlash(key))
	relative, err := filepath.Rel(s.root, full)
	if err != nil {
		return "", fmt.Errorf("blob key %q: %w", key, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("blob key %q escapes the store root", key)
	}
	return full, nil
}

func (s *Store) pruneEmptyParents(dir string) {
	for dir != s.root && dir != s.tempDir {
		relative, err := filepath.Rel(s.root, dir)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func copyBlob(ctx context.Context, dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written += int64(n)
			if maxBytes > 0 && written > maxBytes {
				return written, fmt.Errorf("%w: limit is %d bytes", ErrBlobTooLarge, maxBytes)
			}
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
