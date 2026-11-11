package memory

import (
	"context"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// AppendAudit appends an audit entry to the in-memory audit log.
func (s *Store) AppendAudit(ctx context.Context, entry domain.AuditEntry) error {
	if err := domain.RequireNonEmpty("audit_id", entry.ID); err != nil {
		return err
	}
	if err := domain.RequireNonEmpty("audit_action", entry.Action); err != nil {
		return err
	}
	if entry.TenantID != "" {
		if err := entry.TenantID.Validate(); err != nil {
			return err
		}
	}
	stored := cloneAuditEntry(entry)
	if stored.At.IsZero() {
		stored.At = time.Now().UTC()
	} else {
		stored.At = stored.At.UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	s.audit = append(s.audit, stored)
	return nil
}
