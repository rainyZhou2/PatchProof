package postgres

import (
	"context"

	"github.com/patchproof/patchproof/internal/domain"
)

// AppendAudit inserts an audit log entry.
func (s *Store) AppendAudit(ctx context.Context, entry domain.AuditEntry) error {
	if err := domain.RequireNonEmpty("audit_id", entry.ID); err != nil {
		return err
	}
	if err := domain.RequireNonEmpty("action", entry.Action); err != nil {
		return err
	}
	if entry.TenantID != "" {
		if err := entry.TenantID.Validate(); err != nil {
			return err
		}
	}
	at := normalizeTime(entry.At)
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_log (
		audit_id, tenant_id, actor, action, resource, resource_id, outcome, detail, created_at
	) VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7, $8, $9)`,
		entry.ID, entry.TenantID.String(), entry.Actor, entry.Action, entry.Resource,
		entry.ResourceID, entry.Outcome, entry.Detail, at,
	); err != nil {
		return mapError(err, "audit_log", entry.ID)
	}
	return nil
}
