# Operations

## Runbooks

- [PostgreSQL failover](runbooks/postgres-failover.md)
- [NATS outage](runbooks/nats-outage.md)
- [CAS outage](runbooks/cas-outage.md)
- [Scheduler stuck or backlog](runbooks/scheduler-stuck.md)
- [Worker failures](runbooks/worker-failures.md)
- [Attestation failure](runbooks/attestation-failure.md)
- [Region migration](runbooks/region-migration.md)

## Golden signals

| Signal | Metric |
|---|---|
| API health | `patchproof_http_requests_total`, request latency, 5xx ratio |
| Queue wait | `patchproof_step_queue_wait_seconds` P95 per pool |
| Backlog | `patchproof_scheduler_backlog`, oldest READY step age |
| Execution | `patchproof_step_duration_seconds`, `patchproof_step_retries_total` |
| Leases | `patchproof_lease_expired_total`, attempt generation increments |
| Outbox | `patchproof_outbox_published_total`, `patchproof_outbox_failures_total` |
| Storage | `patchproof_cas_transfer_bytes_total`, CAS 5xx and digest failures |
| Reuse | `patchproof_cache_hit_total{cache_kind="action"}` |
| Verdicts | `patchproof_run_verdicts_total`, `patchproof_evidence_invalid_total` |

## Invariants

1. A confirmed run is never deleted or lost; PostgreSQL is authoritative.
2. A stale attempt can never commit over a newer generation.
3. No cross-tenant read path exists in the API, CAS namespace or evidence
   bundle.
4. A report is never marked `SIGNED` without a verified signature; a failed
   signer yields `PENDING_ATTESTATION`.
5. Isolation is never silently downgraded when a RuntimeClass is missing.

## Change windows

- Schema changes use expand/contract and must be compatible with controller
  replicas at version N-1.
- Scheduler shard count changes require a full controller restart; shards are
  leased, not resized in place.
- Worker image changes roll forward per attempt; in-flight attempts finish on
  their original generation.
