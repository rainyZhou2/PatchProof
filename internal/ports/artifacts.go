package ports

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type SecurityScope struct {
	TenantID    domain.TenantID
	TrustDomain string
	Region      string
}

func (s SecurityScope) Validate() error {
	if err := s.TenantID.Validate(); err != nil {
		return err
	}
	if s.TrustDomain == "" {
		return &domain.ValidationError{Field: "trust_domain", Reason: "must not be empty"}
	}
	return nil
}

func (s SecurityScope) Namespace() string {
	return string(s.TenantID) + "/" + s.TrustDomain
}

type BlobInfo struct {
	Ref         domain.CASRef     `json:"ref"`
	Namespace   string            `json:"namespace"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type ArtifactStore interface {
	Put(ctx context.Context, scope SecurityScope, r io.Reader) (domain.CASRef, error)
	Open(ctx context.Context, scope SecurityScope, ref domain.CASRef) (io.ReadCloser, error)
	Stat(ctx context.Context, scope SecurityScope, ref domain.CASRef) (BlobInfo, error)
	Delete(ctx context.Context, scope SecurityScope, ref domain.CASRef) error
	Health(ctx context.Context) error
}

type UploadTicket struct {
	URL       string        `json:"url"`
	Method    string        `json:"method"`
	Headers   http.Header   `json:"headers"`
	Key       domain.CASRef `json:"cas_ref"`
	ExpiresAt time.Time     `json:"expires_at"`
}

type Presigner interface {
	PresignGet(ctx context.Context, scope SecurityScope, ref domain.CASRef, ttl time.Duration) (string, http.Header, error)
	PresignPut(ctx context.Context, scope SecurityScope, ref domain.CASRef, ttl time.Duration) (UploadTicket, error)
}

type SnapshotRequest struct {
	Repository domain.RepositoryRef
	TenantID   domain.TenantID
	Budget     domain.Budget
}

type SnapshotResult struct {
	Manifest   domain.SnapshotManifest
	Archive    domain.CASRef
	BasePoints []string
	ChangedAt  time.Time
}

type Snapshotter interface {
	Snapshot(ctx context.Context, request SnapshotRequest) (SnapshotResult, error)
	Health(ctx context.Context) error
}
