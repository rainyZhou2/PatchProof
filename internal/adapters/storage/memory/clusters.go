package memory

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// UpsertCluster stores the latest heartbeat for a cluster.
func (s *Store) UpsertCluster(ctx context.Context, cluster domain.Cluster) error {
	if strings.TrimSpace(cluster.ID.String()) == "" {
		return &domain.ValidationError{Field: "cluster_id", Reason: "must not be empty"}
	}
	if err := domain.RequireNonEmpty("cluster.region", cluster.Region); err != nil {
		return err
	}
	stored := cloneCluster(cluster)
	if stored.LastHeartbeat.IsZero() {
		stored.LastHeartbeat = time.Now().UTC()
	} else {
		stored.LastHeartbeat = stored.LastHeartbeat.UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	s.clusters[stored.ID] = stored
	return nil
}

// GetCluster returns a cluster by ID.
func (s *Store) GetCluster(ctx context.Context, clusterID domain.ClusterID) (domain.Cluster, error) {
	if clusterID.String() == "" {
		return domain.Cluster{}, &domain.ValidationError{Field: "cluster_id", Reason: "must not be empty"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Cluster{}, err
	}
	cluster, ok := s.clusters[clusterID]
	if !ok {
		return domain.Cluster{}, &domain.NotFoundError{Resource: "cluster", ID: clusterID.String()}
	}
	return cloneCluster(cluster), nil
}

// ListClusters returns clusters in a region sorted by cluster ID.
func (s *Store) ListClusters(ctx context.Context, region string) ([]domain.Cluster, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return nil, err
	}
	out := make([]domain.Cluster, 0)
	for _, cluster := range s.clusters {
		if region != "" && !strings.EqualFold(cluster.Region, region) {
			continue
		}
		out = append(out, cloneCluster(cluster))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteCluster removes a cluster registration.
func (s *Store) DeleteCluster(ctx context.Context, clusterID domain.ClusterID) error {
	if clusterID.String() == "" {
		return &domain.ValidationError{Field: "cluster_id", Reason: "must not be empty"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	if _, ok := s.clusters[clusterID]; !ok {
		return &domain.NotFoundError{Resource: "cluster", ID: clusterID.String()}
	}
	delete(s.clusters, clusterID)
	return nil
}

// AcquireShardLeases claims free, expired or already-owned shards.
func (s *Store) AcquireShardLeases(ctx context.Context, shards []int, owner string, ttl time.Duration, now time.Time) ([]int, error) {
	if err := domain.RequireNonEmpty("shard_lease_owner", owner); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, &domain.ValidationError{Field: "shard_lease_ttl", Reason: "must be positive"}
	}
	for _, shard := range shards {
		if shard < 0 {
			return nil, &domain.ValidationError{Field: "shard", Reason: "must not be negative"}
		}
	}
	now = normalizeTime(now)
	expiresAt := now.Add(ttl)

	unique := make(map[int]struct{}, len(shards))
	for _, shard := range shards {
		unique[shard] = struct{}{}
	}
	requested := make([]int, 0, len(unique))
	for shard := range unique {
		requested = append(requested, shard)
	}
	sort.Ints(requested)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return nil, err
	}
	acquired := make([]int, 0, len(requested))
	for _, shard := range requested {
		existing, ok := s.shardLeases[shard]
		if ok && existing.Owner != owner && now.Before(existing.ExpiresAt) {
			continue
		}
		s.shardLeases[shard] = domain.ShardLease{Shard: shard, Owner: owner, ExpiresAt: expiresAt}
		acquired = append(acquired, shard)
	}
	return acquired, nil
}

// ReleaseShardLeases releases every shard held by an owner.
func (s *Store) ReleaseShardLeases(ctx context.Context, owner string) error {
	if err := domain.RequireNonEmpty("shard_lease_owner", owner); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	for shard, lease := range s.shardLeases {
		if lease.Owner == owner {
			delete(s.shardLeases, shard)
		}
	}
	return nil
}
