# Schema expand and contract

PostgreSQL is the metadata authority. Schema changes follow expand → migrate →
contract so that old and new binaries can run during a rolling upgrade.

## Migration mechanics

- Files live in `migrations/postgres` and are embedded by
  `migrations/postgres/embed.go`.
- `internal/adapters/storage/postgres.Store.Migrate` applies files in filename
  order inside one transaction, records each version in `schema_migrations`, and
  takes `pg_advisory_xact_lock` so concurrent starts serialize.
- Versions are the numeric prefix (`0001` → `1`, `0002` → `2`).
- After the transaction, the store calls `patchproof_ensure_partitions(3)` to
  create the current and next quarterly partitions for partitioned tables.
- The Helm chart runs the same SQL from `deploy/helm/patchproof/files/migrations`
  in a pre-install/pre-upgrade Job. `make sync-helm-migrations` refreshes the
  copies and `make helm-migrations-check` (also in CI) fails on drift.
- `database.migrateOnStart=true` is available for development and Compose. It
  uses the same advisory lock.

`migrations/postgres/0001_init.sql` creates the partitioned `runs`, `steps`,
`attempts`, `evidence_documents`, `outbox_events` and `audit_log` tables,
`run_request_keys`, reports, clusters, shard leases and the partition helper.
`0002_step_options.sql` adds `steps.options`.

## Expand

Add new nullable columns, new tables or new indexes without changing the meaning
of existing fields. Deploy the binary that writes both the old and new shape
when needed. Do not add a `NOT NULL` column without a default to a large table
in the same step.

## Migrate

Backfill in bounded batches outside the migration transaction. Record progress.
Make the new field authoritative only after the backfill and verification.
Dual-read only when the old binary still needs the old field.

## Contract

Stop writing the old field, wait for the compatibility window, then drop it in a
later release. A drop is never shipped in the same release that stopped writing
the field.

## Rollback and backups

- An expand migration is safe to leave in place if a binary rolls back.
- A contract migration is not reversible without a backup. Take a PostgreSQL
  base backup and verify PITR before any contract step.
- Test migrations on a staging copy with the same PostgreSQL major version and
  realistic table sizes.
- Long index builds should use `CREATE INDEX CONCURRENTLY` outside the
  transaction; the current migration runner uses a transaction, so such a change
  needs a dedicated migration path.
- Never edit `schema_migrations` by hand. If a migration fails, fix forward or
  restore; do not mark it applied without applying the SQL.

## Partitioning

Time-partitioned tables use `created_at` and a `*_default` partition. The helper
creates quarterly partitions three months ahead. Partition creation is
idempotent. Dropping old partitions is a retention decision; it must preserve
active runs, reproducers and retained reports.
