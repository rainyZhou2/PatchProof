// Package localfs provides an attempt-scoped, content-addressed artifact
// store rooted at a local directory. It uses the same key layout as the
// platform CAS (pkg/cas.BlobKey) so a broker can stage blobs into a shared
// volume and the offline worker can consume them without any network access.
//
// The store deliberately has no remote support and no credentials. Every read
// is verified against the content address while streaming, which makes a
// corrupted or tampered staging volume visible to the caller instead of
// silently uploading wrong bytes during stage-out.
package localfs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	casfs "github.com/patchproof/patchproof/internal/adapters/cas/fs"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

// ErrIntegrity reports a blob whose stored bytes no longer match its content
// address or declared size. It aliases the fs adapter's error so callers can
// treat both local adapters uniformly.
var ErrIntegrity = casfs.ErrIntegrity

const (
	directoryPerm = 0o750
)

// Store is a local, attempt-scoped content-addressed artifact store.
type Store struct {
	*casfs.Store
	root string
}

var _ ports.ArtifactStore = (*Store)(nil)

// New creates a store rooted at root, creating the directory when needed. The
// root must be writable; the caller owns its lifetime and cleanup.
func New(root string) (*Store, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return nil, errors.New("local cas: root must not be empty")
	}
	inner, err := casfs.New(casfs.Config{Root: trimmed, CreateRoot: true})
	if err != nil {
		return nil, fmt.Errorf("local cas: %w", err)
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("local cas: resolve root %q: %w", trimmed, err)
	}
	return &Store{Store: inner, root: filepath.Clean(absolute)}, nil
}

// Root returns the absolute root directory of the store.
func (s *Store) Root() string { return s.root }

// Blob describes a blob discovered under the store root.
type Blob struct {
	Ref  domain.CASRef
	Path string
}

// Blobs walks the CAS key tree and returns every stored blob in deterministic
// digest order. Files outside the cas/ subtree (for example result.json and
// input-manifest.json) are ignored. Path formatting is validated against the
// canonical pkg/cas key layout, and non-regular entries are rejected so a
// symlink cannot smuggle content into a stage-out upload.
func (s *Store) Blobs(ctx context.Context) ([]Blob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(s.root, "cas")
	info, err := os.Stat(root)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, fmt.Errorf("local cas: %s is not a directory", root)
		}
	case errors.Is(err, iofs.ErrNotExist):
		return nil, nil
	default:
		return nil, fmt.Errorf("local cas: stat blob root: %w", err)
	}

	var blobs []Blob
	walkErr := filepath.WalkDir(root, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("local cas: blob %s is not a regular file", path)
		}
		relative, err := filepath.Rel(s.root, path)
		if err != nil {
			return fmt.Errorf("local cas: resolve blob path %s: %w", path, err)
		}
		key := filepath.ToSlash(relative)
		ref, err := refForBlobKey(key)
		if err != nil {
			return err
		}
		stat, err := entry.Info()
		if err != nil {
			return fmt.Errorf("local cas: stat blob %s: %w", key, err)
		}
		if stat.Size() < 0 {
			return fmt.Errorf("local cas: blob %s has a negative size", key)
		}
		ref.SizeBytes = stat.Size()
		blobs = append(blobs, Blob{Ref: ref, Path: path})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].Ref.Digest < blobs[j].Ref.Digest
	})
	return blobs, nil
}

// refForBlobKey validates a canonical CAS key and returns the content address
// encoded by it. Any key that is not exactly the layout produced by
// pkg/cas.BlobKey is rejected.
func refForBlobKey(key string) (domain.CASRef, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 7 || parts[0] != "cas" || parts[3] != "sha256" {
		return domain.CASRef{}, fmt.Errorf("local cas: %q is not a CAS blob key", key)
	}
	tenantID, trustDomain, digest := parts[1], parts[2], parts[6]
	expected, err := cas.BlobKey(tenantID, trustDomain, digest)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("local cas: invalid blob key %q: %w", key, err)
	}
	if expected != key {
		return domain.CASRef{}, fmt.Errorf("local cas: blob key %q is not canonical (want %q)", key, expected)
	}
	return domain.NewCASRef(digest, 0), nil
}
