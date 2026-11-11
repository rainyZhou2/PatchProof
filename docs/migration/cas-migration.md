# CAS migration

CAS objects are immutable and content-addressed. A migration copies bytes,
verifies them, and only then changes the store configuration. It never rewrites
objects or changes a digest.

## Layout and addressing

- Logical references are `cas://sha256/<digest>`; the digest, not the object key,
  is the identity.
- `pkg/cas.BlobKey` lays out `cas/<tenant>/<trust-domain>/sha256/<ab>/<cd>/<digest>`.
- `pkg/cas.ReportKey` and `ReproducerKey` place report and reproducer objects
  under tenant and run prefixes.
- A `SecurityScope` is tenant plus trust domain. Two tenants never share a
  private blob namespace.
- The S3 adapter verifies size and digest on read; the filesystem adapter stores
  under a digest-derived path.

## fs → S3 or S3 → S3

1. Choose the target bucket and prefix. Keep the tenant and trust-domain
   hierarchy; do not flatten it.
2. Copy objects with the object store's server-side copy if the digest key is
   preserved, otherwise stream and re-hash.
3. Verify every copied object: size, SHA-256, and readability through the
   adapter. A count match is not verification.
4. Run a dual-read window: keep the old store available while the new one is
   primary for reads. Never write the same digest to both stores with different
   bytes; a digest collision is an integrity incident.
5. Switch configuration (`PATCHPROOF_CAS_DRIVER`, endpoint, bucket, prefix) and
   restart. Pending attempts that reference old objects must still resolve
   during the window.
6. After the retention window, remove the old copy. Keep blobs referenced by
   active runs, reproducers and retained reports.

## S3 → filesystem (local)

Only for trusted local or test environments. Copy into the layout expected by
`internal/adapters/cas/fs`, verify digests, and expect much lower capacity. Do
not use a local filesystem as a multi-tenant production CAS.

## Credentials and presigning

Use workload identity or short-lived credentials. A presigned URL is scoped to
one object and TTL; changing the store invalidates outstanding URLs. Quiesce or
re-issue URLs during cutover. Do not make the bucket public to simplify a
migration.

## GC and retention

Orphan blobs can be removed only when no run, evidence, report, reproducer or
legal hold references them. GC must be off during the copy window and must use
the reference index, not filesystem age alone. Deleting a blob that a stored
report references makes the report unverifiable.

## Invariants

- Blob bytes never change under a digest.
- Tenant and trust-domain scopes stay separate.
- A manifest is published only after all leaves exist and verify.
- S3 temporary spool files are plaintext and are not a migration mechanism.
- The target store must be reachable from every execution cluster that will read
  or write it; otherwise the migration is incomplete.
