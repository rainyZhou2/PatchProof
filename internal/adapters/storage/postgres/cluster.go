package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/patchproof/patchproof/internal/domain"
)

// UpsertCluster inserts or refreshes a cluster capability snapshot. A newer
// heartbeat never moves backwards.
func (s *Store) UpsertCluster(ctx context.Context, cluster domain.Cluster) error {
	if err := domain.RequireNonEmpty("cluster_id", cluster.ID.String()); err != nil {
		return err
	}
	if err := domain.RequireNonEmpty("region", cluster.Region); err != nil {
		return err
	}
	tiers := make([]string, 0, len(cluster.IsolationTiers))
	for _, tier := range cluster.IsolationTiers {
		if !tier.Valid() {
			return &domain.ValidationError{Field: "isolation_tiers", Reason: "unknown isolation tier"}
		}
		tiers = append(tiers, string(tier))
	}
	architectures, err := encodeJSON(cluster.Architectures)
	if err != nil {
		return err
	}
	languages, err := encodeJSON(cluster.Languages)
	if err != nil {
		return err
	}
	engines, err := encodeJSON(cluster.Engines)
	if err != nil {
		return err
	}
	encodedTiers, err := encodeJSON(tiers)
	if err != nil {
		return err
	}
	runtimeClasses, err := encodeJSON(cluster.RuntimeClasses)
	if err != nil {
		return err
	}
	labels := cluster.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	encodedLabels, err := encodeJSON(labels)
	if err != nil {
		return err
	}
	heartbeat := cluster.LastHeartbeat
	if heartbeat.IsZero() {
		heartbeat = time.Now().UTC()
	} else {
		heartbeat = heartbeat.UTC()
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO clusters (
		cluster_id, region, availability_zone, executor_kind, endpoint, namespace,
		architectures, languages, engines, isolation_tiers, runtime_classes,
		available_cpu_milli, available_memory_mib, queue_depth, active_attempts,
		max_concurrent_steps, cost_tier, cas_region, healthy, maintenance, version,
		labels, last_heartbeat
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
		$17, $18, $19, $20, $21, $22, $23
	) ON CONFLICT (cluster_id) DO UPDATE SET
		region = EXCLUDED.region,
		availability_zone = EXCLUDED.availability_zone,
		executor_kind = EXCLUDED.executor_kind,
		endpoint = EXCLUDED.endpoint,
		namespace = EXCLUDED.namespace,
		architectures = EXCLUDED.architectures,
		languages = EXCLUDED.languages,
		engines = EXCLUDED.engines,
		isolation_tiers = EXCLUDED.isolation_tiers,
		runtime_classes = EXCLUDED.runtime_classes,
		available_cpu_milli = EXCLUDED.available_cpu_milli,
		available_memory_mib = EXCLUDED.available_memory_mib,
		queue_depth = EXCLUDED.queue_depth,
		active_attempts = EXCLUDED.active_attempts,
		max_concurrent_steps = EXCLUDED.max_concurrent_steps,
		cost_tier = EXCLUDED.cost_tier,
		cas_region = EXCLUDED.cas_region,
		healthy = EXCLUDED.healthy,
		maintenance = EXCLUDED.maintenance,
		version = EXCLUDED.version,
		labels = EXCLUDED.labels,
		last_heartbeat = GREATEST(clusters.last_heartbeat, EXCLUDED.last_heartbeat)`,
		cluster.ID.String(), cluster.Region, cluster.AvailabilityZone, cluster.ExecutorKind,
		cluster.Endpoint, cluster.Namespace, architectures, languages, engines, encodedTiers,
		runtimeClasses, cluster.AvailableCPUMilli, cluster.AvailableMemoryMiB,
		cluster.QueueDepth, cluster.ActiveAttempts, cluster.MaxConcurrentSteps,
		cluster.CostTier, cluster.CasRegion, cluster.Healthy, cluster.Maintenance,
		cluster.Version, encodedLabels, heartbeat,
	)
	if err != nil {
		return mapError(err, "cluster", cluster.ID.String())
	}
	return nil
}

// GetCluster loads a cluster by id.
func (s *Store) GetCluster(ctx context.Context, clusterID domain.ClusterID) (domain.Cluster, error) {
	if err := domain.RequireNonEmpty("cluster_id", clusterID.String()); err != nil {
		return domain.Cluster{}, err
	}
	cluster, err := scanCluster(s.pool.QueryRow(ctx,
		"SELECT "+clusterColumns+" FROM clusters WHERE cluster_id = $1", clusterID.String()))
	if err != nil {
		return domain.Cluster{}, mapError(err, "cluster", clusterID.String())
	}
	return cluster, nil
}

// ListClusters returns known clusters, optionally restricted to a region.
func (s *Store) ListClusters(ctx context.Context, region string) ([]domain.Cluster, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if region == "" {
		rows, err = s.pool.Query(ctx, "SELECT "+clusterColumns+" FROM clusters ORDER BY cluster_id")
	} else {
		rows, err = s.pool.Query(ctx,
			"SELECT "+clusterColumns+" FROM clusters WHERE lower(region) = lower($1) ORDER BY cluster_id", region)
	}
	if err != nil {
		return nil, mapError(err, "cluster", region)
	}
	return collectClusters(rows)
}

// DeleteCluster removes a cluster row.
func (s *Store) DeleteCluster(ctx context.Context, clusterID domain.ClusterID) error {
	if err := domain.RequireNonEmpty("cluster_id", clusterID.String()); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, "DELETE FROM clusters WHERE cluster_id = $1", clusterID.String())
	if err != nil {
		return mapError(err, "cluster", clusterID.String())
	}
	if tag.RowsAffected() == 0 {
		return notFound("cluster", clusterID.String())
	}
	return nil
}
