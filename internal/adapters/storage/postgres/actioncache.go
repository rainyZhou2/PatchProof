package postgres

import (
	"context"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// GetActionCache returns a non-expired cache entry for a tenant and key.
func (s *Store) GetActionCache(ctx context.Context, tenantID domain.TenantID, actionKey string, now time.Time) (domain.ActionCacheEntry, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.ActionCacheEntry{}, err
	}
	if actionKey == "" {
		return domain.ActionCacheEntry{}, &domain.ValidationError{Field: "action_key", Reason: "must not be empty"}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var (
		entry      domain.ActionCacheEntry
		stepKind   string
		resultJSON []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT tenant_id, action_key, step_kind, engine, engine_version, result, created_at, expires_at
		FROM action_cache
		WHERE tenant_id = $1::uuid AND action_key = $2 AND expires_at > $3`,
		tenantID.String(), actionKey, now.UTC(),
	).Scan(&entry.TenantID, &entry.ActionKey, &stepKind, &entry.Engine, &entry.EngineVersion, &resultJSON, &entry.CreatedAt, &entry.ExpiresAt)
	if err != nil {
		return domain.ActionCacheEntry{}, mapError(err, "action_cache", actionKey)
	}
	entry.StepKind = domain.StepKind(stepKind)
	if err := decodeJSON(resultJSON, &entry.Result); err != nil {
		return domain.ActionCacheEntry{}, err
	}
	return entry, nil
}

// PutActionCache inserts or replaces a cache entry.
func (s *Store) PutActionCache(ctx context.Context, entry domain.ActionCacheEntry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	raw, err := encodeJSON(entry.Result)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO action_cache (
		tenant_id, action_key, step_kind, engine, engine_version, result, created_at, expires_at
	) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (tenant_id, action_key) DO UPDATE SET
		step_kind = EXCLUDED.step_kind,
		engine = EXCLUDED.engine,
		engine_version = EXCLUDED.engine_version,
		result = EXCLUDED.result,
		created_at = EXCLUDED.created_at,
		expires_at = EXCLUDED.expires_at`,
		entry.TenantID.String(), entry.ActionKey, string(entry.StepKind), entry.Engine, entry.EngineVersion,
		raw, entry.CreatedAt.UTC(), entry.ExpiresAt.UTC(),
	)
	if err != nil {
		return mapError(err, "action_cache", entry.ActionKey)
	}
	return nil
}

// PurgeActionCache removes up to limit entries that expired at or before the
// supplied time.
func (s *Store) PurgeActionCache(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM action_cache WHERE (tenant_id, action_key) IN (
		SELECT tenant_id, action_key FROM action_cache WHERE expires_at <= $1 LIMIT $2
	)`, before.UTC(), limit)
	if err != nil {
		return 0, mapError(err, "action_cache", "purge")
	}
	return int(tag.RowsAffected()), nil
}
