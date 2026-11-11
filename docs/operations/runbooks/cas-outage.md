# Runbook: CAS outage

The content-addressed store holds source snapshots, engine artifacts, evidence
blobs and report manifests. A CAS failure prevents steps from starting or
completing. It must never produce a passing verdict.

## Symptoms

- Steps fail or enter `RETRY_WAIT` with `CAS_5XX`, `UPLOAD_FAILED` or
  `CAS_DIGEST_MISMATCH`.
- Workers log `stage inputs`, `artifact digest mismatch`, `download`, or upload
  errors.
- `GET /readyz` returns `503` because the artifact store health check fails.
- Reports are missing or reference artifacts that cannot be read.
- Pre-signed URL requests fail with expired or denied responses.

## Signals and dashboards

- `patchproof_cas_transfer_bytes_total{direction,region}` drops or spikes.
- `patchproof_step_retries_total{error_class="CAS_5XX"}` and
  `{error_class="UPLOAD_FAILED"}` rise.
- `patchproof_engine_errors_total` for upload-related engine failures.
- S3/MinIO request errors, 5xx rate, throttling, bucket capacity, TLS errors.
- Worker logs for digest mismatch versus connectivity.

## Diagnosis

1. Distinguish availability from integrity. `CAS_5XX` and timeouts are
   availability; `CAS_DIGEST_MISMATCH` is an integrity or corruption signal and
   should be treated as a security event.
2. Check endpoint, region, bucket and path-style settings. A MinIO endpoint
   usually requires `forcePathStyle: true`.
3. Check credentials and bucket policy. The worker or controller may have a
   valid key that lacks `GetObject`/`PutObject` on the prefix.
4. Check presigned URL TTL (`presignTtl`). A URL generated before a long queue
   wait can expire before the worker uses it.
5. Check NetworkPolicy egress to the object store port (443 or the MinIO port).
6. Check free space and object count limits on the store.

## Remediation

1. Restore the object store or the network path. Retryable step classes are
   rescheduled by the attempt state machine with bounded backoff.
2. If credentials rotated, update the referenced Secret and restart the
   components that read it. Do not bake keys into images.
3. If a presigned URL expired, re-issue it; do not extend the object's public
   access.
4. If the bucket was recreated empty, do not fabricate manifests. Runs that
   depend on missing blobs are inconclusive or infrastructure-failed, not
   verified.
5. For digest mismatch, stop accepting writes from the affected scope, preserve
   the object and access logs, and investigate cross-tenant or cache poisoning.
6. Orphan blobs require a reviewed reference-index-based cleanup; this tree does
   not implement automatic CAS garbage collection. Never delete blobs referenced
   by an active run, a reproducer or a retained report.

## Invariants not to break

- Every blob is verified against its digest before use.
- Objects are immutable; a corrected artifact gets a new digest.
- Tenant and trust-domain scopes are never mixed.
- A manifest is published only after all referenced blobs exist and verify.
- Missing or invalid evidence cannot support `VERIFIED_WITHIN_SCOPE`.
