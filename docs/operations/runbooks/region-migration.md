# Runbook: region migration

A tenant has one home region and one active writer epoch. Moving a tenant is a
controlled single-writer cutover, not a live active-active replication. The
repository implements the tenant `home_region`, `status` and
`active_writer_epoch` fields and an expected-epoch compare-and-swap
(`UpdateTenantEpoch`); it does not implement an automated migration controller.

Read [docs/migration/region-migration.md](../../migration/region-migration.md)
for the invariants and data procedure before executing this runbook.

## Symptoms

This runbook is used for a planned migration, or when a region is being retired
or failed over. Signals that a migration is in progress or stuck:

- The tenant status is `MIGRATING`.
- New submissions return `403 POLICY_REJECTED` (the detail names `TENANT_NOT_ACTIVE`)
  while the tenant is `MIGRATING`; the code does not implement a staging queue.
- The old and new region disagree about `active_writer_epoch`.
- CAS copies exist but digest verification has not completed.
- In-flight attempts continue to report to the old region.

## Signals and dashboards

- Tenant record: `home_region`, `status`, `active_writer_epoch`.
- API requests by region and tenant, including `503`/`429`.
- PostgreSQL replication or logical export progress.
- CAS object counts and digest-verification failures.
- `patchproof_active_runs`, `patchproof_active_steps` per region.
- `patchproof_outbox_*` per region.
- Audit log continuity across the cutover.

## Diagnosis

1. Confirm source and target run compatible versions: schema migration head,
   protocol major, report/evidence schema, and configuration keys.
2. Confirm the tenant is allowed to move: `allowed_regions`, data residency,
   classification, and artifact export permission.
3. Confirm no second writer is active. A tenant with two epochs writing is a
   split-brain; stop one side before continuing.
4. Confirm CAS pre-copy is complete and every copied object verifies. A count
   match without digest verification is insufficient.
5. Confirm in-flight work is either finished or intentionally cancelled. Process
   memory cannot be migrated; only task semantics can.

## Remediation

1. Freeze new submissions for the tenant (set `MIGRATING` through the reviewed
   database/tooling path; the public API has no tenant-update endpoint). The
   current code rejects them with `403 POLICY_REJECTED`, so clients must retry
   after the cutover.
2. Pre-copy CAS and metadata. Verify digests and ACLs on the target.
3. Run the target in read-only shadow mode; do not dual-write.
4. Drain or checkpoint in-flight attempts. Long fuzz work may be cancelled and
   re-run later.
5. Stop writes in the source, copy the final delta, and prove that the source is
   frozen.
6. Promote the target with a single epoch increment and update `home_region` in
   one compare-and-swap. Reject old-epoch requests at the edge.
7. Resume submissions, verify a test run, and watch evidence/report consistency.
8. Keep the rollback window. After new writes occur, rollback requires reverse
   synchronization, not a DNS change.

## Invariants not to break

- At most one `active_writer_epoch` may write a tenant at any time.
- Never enable dual-write "temporarily".
- Never move data across a residency boundary that the tenant policy forbids;
  degrade availability instead.
- Run ids, request keys, idempotency fingerprints and evidence digests remain
  stable across the move.
- Old attempts are fenced; a late result from the source region cannot commit in
  the target.
- Do not delete the source until retention, legal hold and rollback windows are
  satisfied.
