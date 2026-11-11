package proofverify

import (
	"context"
	"fmt"
	"io"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type MultiScopeSource struct {
	Store       ports.ArtifactStore
	TenantID    domain.TenantID
	Region      string
	TrustScopes []string
}

func (s *MultiScopeSource) Open(ctx context.Context, ref domain.CASRef) (io.ReadCloser, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("proofverify: artifact store is not configured")
	}
	scopes := s.TrustScopes
	if len(scopes) == 0 {
		scopes = []string{"report", "run", "snapshot"}
	}
	var lastErr error
	for _, trustDomain := range scopes {
		scope := ports.SecurityScope{TenantID: s.TenantID, TrustDomain: trustDomain, Region: s.Region}
		if err := scope.Validate(); err != nil {
			return nil, err
		}
		reader, err := s.Store.Open(ctx, scope, ref)
		if err == nil {
			return reader, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
