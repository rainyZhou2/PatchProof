# Runbook: PostgreSQL failover

PostgreSQL is the only authority for run, step, attempt, evidence, report,
outbox and shard-lease state. A failover is a correctness event, not only an
availability event.

## Symptoms

- `GET /readyz` returns `503 NOT_READY`; the detail points at the metadata store.
- New submissions and reads return `500`; the API uses the primary pool.
- Controller logs repeat `initial shard ownership failed`, `lease batch failed`,
  `outbox drain failed` or `commit result` errors.
- Runs remain `QUEUED`; steps remain `READY`; no new Jobs appear.
- Worker callbacks return `5xx` and attempts eventually expire.

## Signals and dashboards

- `patchproof_http_requests_total{status="5xx"}` rises; readiness is failing.
- `patchproof_active_runs` and `patchproof_active_steps` stop changing.
- `patchproof_outbox_failures_total` rises if the API can still write outbox rows,
  or stops entirely if writes fail.
- PostgreSQL exporter: primary availability, replication lag, connection
  saturation, long transactions, lock waits.
- Logs: `ping postgres`, `shard ownership failed`, `lease batch failed`,
  `commit result`, `migrate schema`.

## Diagnosis

1. Confirm the failure is the database and not the secret or network:
   `psql "$PATCHPROOF_POSTGRES_DSN" -c 'select 1'` from the API namespace.
2. Check whether the primary is down, read-only, out of connections, or waiting
   on locks. `statement_timeout` is configured; a timeout is not a dead primary.
3. Check `schema_migrations` for the latest version. A failed migration leaves
   the schema behind the binary.
4. Check whether a second writer was promoted without fencing the old one.
   Two primaries accepting writes is a split-brain; stop one before proceeding.
5. Check the shard lease table for leases held by controllers that no longer
   exist. Expiry is time-based; do not delete live leases.

## Remediation

1. If the primary is lost, promote the standby using the platform's database
   tooling. Keep the old primary fenced or shut down.
2. If the DSN changed, update the referenced Secret. Do not put a DSN in a
   ConfigMap.
3. Restart API and controller pods after the new primary is writable. The
   scheduler re-acquires expired shard leases; in-flight attempts are fenced and
   retried by the attempt state machine.
4. If the schema is behind, apply migrations with the chart hook or
   `make migrate`, or start one process with `database.migrateOnStart=true`.
   The migration uses an advisory lock, so concurrent starts serialize.
5. Verify `GET /readyz` returns `200`, a test run reaches `PLANNED`, and the
   controller logs show `lease batch` progress.

## Invariants not to break

- Never point two active regions or two primary databases at the same tenant
  epoch.
- Never delete or rewrite `runs`, `steps`, `attempts`, `evidence` or `reports` to
  make a run progress.
- Never reuse an attempt id or lower a step generation.
- Preserve `request_key` and fingerprint rows; idempotency depends on them.
- Preserve pending outbox rows. Replaying them is safe; deleting them loses
  notifications.
- Do not manually mark a step `SUCCEEDED` or a run `COMPLETED`.
