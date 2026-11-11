# Region migration invariants

A tenant is served by one home region with one active writer epoch. The code
contains the fields and compare-and-swap needed to enforce that rule
(`domain.Tenant`, `UpdateTenantEpoch`), but it does not implement a global
directory, cross-region replication or an automated migration controller. The
procedure below is an operational contract for a manual, rehearsed migration.

## Scope and preconditions

- Source and target run compatible schema, protocol and report versions. See
  `version-compatibility.md`.
- Tenant policy allows the target region (`allowed_regions`, residency,
  classification, artifact export).
- Target has enough PostgreSQL, NATS, CAS and execution capacity before any
  traffic is moved.
- A rollback window and backup/PITR plan exist for both sides.

## Data to move

- PostgreSQL rows for the tenant: projects, policies, runs, steps, attempts,
  evidence, counterexamples, reports, audit entries, request keys and relevant
  outbox state.
- CAS objects referenced by active or retained runs, reproducers and reports.
- The tenant row itself: `home_region`, `status`, `active_writer_epoch`, quota.
  The public API in this version has no tenant-update endpoint; these fields
  are changed with controlled database/tooling operations under review.
- Signing public keys and trust metadata. Private signing keys do not move with
  tenant data; they are regional or KMS-backed.

## Invariants

1. **Single writer.** At most one region may write a tenant at a given
   `active_writer_epoch`. Dual-write is never enabled, even temporarily.
2. **Monotonic epoch.** Promotion increments the epoch with an expected-epoch
   compare-and-swap. A failed CAS means someone else wrote; stop and re-read.
3. **Stable identity.** Tenant id, project id, run id, request key, idempotency
   fingerprint and evidence digest do not change across the move.
4. **Fenced attempts.** Old-region attempts cannot commit in the target. Their
   generation and attempt tokens are region-local and expire.
5. **No process migration.** Running processes are not moved. In-flight work is
   finished, checkpointed or cancelled and re-run; only task semantics survive.
6. **Artifact integrity.** Every copied CAS object is verified by size and
   SHA-256 before it is referenced in the target.
7. **Residency.** Data that policy forbids exporting is not copied; the tenant
   stays in a compliant region or the migration is refused.
8. **Audit continuity.** Audit entries are preserved and ordered; the target
   records the cutover actor and epoch.

## Procedure

1. Inspect and validate compatibility, capacity and policy.
2. Pre-copy CAS and historical metadata. Verify digests, counts and ACLs.
3. Start a read-only shadow in the target; do not accept writes.
4. Mark the tenant `MIGRATING` in the source. The current code rejects new
   submissions with `403 POLICY_REJECTED` (detail `TENANT_NOT_ACTIVE`); a retryable
   queue is not implemented.
5. Drain, checkpoint or cancel in-flight attempts. Record which runs must be
   re-executed.
6. Freeze source writes. Copy the final delta. Prove the source is frozen.
7. Promote the target: increment `active_writer_epoch` and set `home_region` in
   one compare-and-swap; update routing.
8. Resume submissions. Verify a test run end to end, including a report and
   offline signature verification.
9. Observe error rates, queue depth and evidence consistency through the rollback
   window.
10. After approval, remove the source copy under retention and legal-hold rules.

## Rollback

Before any new writes in the target, rollback is a routing change plus a source
unfreeze. After new writes, rollback requires reverse synchronization and a new
epoch promotion; it is a second migration, not a DNS switch. Never attempt
rollback by making both regions writable.

## Disaster recovery

Region failover for an unrecoverable source is a separate decision. If the
tenant policy allows the target and the source cannot be fenced, do not promote:
a split brain is worse than unavailability. RPO/RTO targets depend on the actual
replication and rehearsal; they are not guaranteed by this repository.
