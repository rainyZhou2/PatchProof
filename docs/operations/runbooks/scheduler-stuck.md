# Runbook: scheduler stuck

The scheduler leases ready steps for the shards it owns. A stuck scheduler shows
runs that plan successfully but never dispatch work.

## Symptoms

- Runs remain `PLANNED` or `QUEUED`; steps remain `READY` or `BLOCKED`.
- No Kubernetes Jobs or local executions are created.
- `patchproof_scheduler_backlog` stays high while
  `patchproof_inflight_steps` is zero.
- Controller logs repeat `initial shard ownership failed`, `lease batch failed`,
  `no eligible cluster`, or `shard lease` errors.
- One controller replica owns all shards, or no replica owns them.

## Signals and dashboards

- `patchproof_scheduler_backlog{region,priority,pool}`.
- `patchproof_active_steps`, `patchproof_inflight_steps{cluster}`.
- `patchproof_lease_expired_total`.
- `patchproof_worker_utilization{cluster,pool,resource}`.
- `shard_leases` table: shard, owner, expires_at.
- `clusters` table: healthy, maintenance, last_heartbeat, available resources.

## Diagnosis

1. Confirm every controller replica has the same `scheduler.shardCount` (or
   `PATCHPROOF_SHARD_COUNT`). A mismatch means replicas disagree about shard
   identity and will not cover the full range.
2. Check `shard_leases`. If all leases are held by dead owners and not expired,
   the database clock or lease TTL is wrong. If leases are free but unclaimed,
   `AcquireShardLeases` is failing.
3. Check the cluster registry. A cluster is unusable when `healthy=false`,
   `maintenance=true`, its heartbeat is older than `clusterStaleAfter`, it lacks
   the step's isolation tier, language, engine, architecture or runtime class, or
   its active attempts reached `maxConcurrentSteps`.
4. Check placement (`scheduler.placement`); an unknown value prevents startup.
5. Check whether the step's `shard` was computed with a different shard count
   than the scheduler is using. Changing `shardCount` requires re-planning or a
   documented reindex; it is not a live tuning knob.
6. Check controller clocks. Lease acquisition and expiry use database-side
   timestamps; large clock skew still breaks freshness checks.

## Remediation

1. Fix the configuration mismatch and roll the controller Deployment. Shard
   leases expire after the shard lease TTL and are re-acquired by the new
   replicas.
2. Restore at least one eligible execution cluster, or reduce `maxConcurrent`
   pressure on existing clusters. Scaling the controller does not create
   capacity.
3. If the cluster registry is stale, fix the cluster agent and let heartbeats
   refresh it. Do not mark a cluster healthy by hand unless the agent is actually
   serving.
4. If steps are stuck because a previous attempt holds a lease, wait for lease
   expiry. Do not force status changes.
5. After recovery, verify backlog drains, Jobs appear, and attempts advance
   generation exactly once per retry.

## Invariants not to break

- A shard has at most one owner at a time.
- Step generation is monotonic; a retry creates a new attempt with a higher
  generation.
- Only the scheduler leases steps; workers cannot claim work.
- Do not reset `shard_leases` while the owning controller is alive.
- Do not edit `steps.status` or `runs.status` directly.
- Changing `shardCount` requires a migration plan, not a rolling restart.
