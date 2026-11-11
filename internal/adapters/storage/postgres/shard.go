package postgres

import (
	"context"
	"sort"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// AcquireShardLeases claims the requested shards when their lease is missing,
// expired, or already owned by the caller.
func (s *Store) AcquireShardLeases(ctx context.Context, shards []int, owner string, ttl time.Duration, now time.Time, limit int) ([]int, error) {
	if err := domain.RequireNonEmpty("owner", owner); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, &domain.ValidationError{Field: "ttl", Reason: "must be positive"}
	}
	if limit < 0 {
		return nil, &domain.ValidationError{Field: "shard_lease_limit", Reason: "must not be negative"}
	}
	if len(shards) == 0 {
		return []int{}, nil
	}
	seen := make(map[int]struct{}, len(shards))
	unique := make([]int32, 0, len(shards))
	for _, shard := range shards {
		if shard < 0 {
			return nil, &domain.ValidationError{Field: "shard", Reason: "must not be negative"}
		}
		if _, duplicate := seen[shard]; duplicate {
			continue
		}
		seen[shard] = struct{}{}
		unique = append(unique, int32(shard))
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	deadline := now.Add(ttl)
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
		SELECT shard FROM (SELECT DISTINCT unnest($1::int4[]) AS shard) AS requested
		WHERE NOT EXISTS (
			SELECT 1 FROM shard_leases AS held
			WHERE held.shard = requested.shard AND held.expires_at > $4 AND held.owner <> $2
		)
		ORDER BY shard
		LIMIT CASE WHEN $5 > 0 THEN $5 ELSE 2147483647 END
	)
	INSERT INTO shard_leases (shard, owner, expires_at)
	SELECT shard, $2, $3 FROM candidates
	ON CONFLICT (shard) DO UPDATE SET
		owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
	WHERE shard_leases.expires_at <= $4 OR shard_leases.owner = EXCLUDED.owner
	RETURNING shard`, unique, owner, deadline, now, limit)
	if err != nil {
		return nil, mapError(err, "shard_lease", owner)
	}
	defer rows.Close()
	claimed := make([]int, 0, len(unique))
	for rows.Next() {
		var shard int
		if err := rows.Scan(&shard); err != nil {
			return nil, err
		}
		claimed = append(claimed, shard)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Ints(claimed)
	return claimed, nil
}

// ReleaseShardLeases releases every shard lease owned by the caller.
func (s *Store) ReleaseShardLeases(ctx context.Context, owner string) error {
	if err := domain.RequireNonEmpty("owner", owner); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM shard_leases WHERE owner = $1", owner); err != nil {
		return mapError(err, "shard_lease", owner)
	}
	return nil
}
