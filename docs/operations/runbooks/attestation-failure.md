# Runbook: attestation failure

Report signing proves which key produced a report and that the report bytes and
referenced evidence were not modified. It does not prove that the verdict is
semantically correct. An unsigned report must not be presented as signed.

## Symptoms

- A report has `attestation_status: PENDING_ATTESTATION` or `FAILED` and no
  `signatures` entry.
- `patchproof proof verify --require-signature` returns `INVALID` or
  `UNVERIFIABLE` with a signature or trust-root issue.
- Report findings include `ATTESTATION_PENDING` or `ATTESTATION_REQUIRED`;
  controller startup logs may report a signing key that cannot be loaded.
- Reports rendered through the API carry `X-PatchProof-Attestation` with a
  non-`SIGNED` value.

## Signals and dashboards

- `patchproof_run_verdicts_total` continues to move, so the issue is signing,
  not the run itself.
- Controller logs around report aggregation.
- Key/secret mount status on the controller pod.
- Offline verifier output: check names, status and issues.
- Audit log entries for report creation.

## Diagnosis

1. Confirm the controller has `signing.privateKeyFile` and that the file exists
   and is readable by UID 65532. A read-only Secret mount must contain both the
   private and public key files.
2. Confirm the key is Ed25519 in PKCS#8 PEM and matches `signing.keyId`.
3. Confirm `signing.require`. When true, startup fails without a signer. When
   false, a report can legitimately be stored unsigned.
4. Verify the report's Merkle root and manifest against CAS. A signature failure
   with a digest mismatch is an integrity incident, not a key problem.
5. Verify the verifier's trust store: the same key ID must map to the expected
   public key, and the report must not be older than `--max-age` when that flag
   is used.
6. The built-in signer is local. This tree does not implement a KMS signer; a
   configured KMS key URI is not used by the current bootstrap.

## Remediation

1. Fix the Secret or key path, restart the controller, and confirm a new report
   is signed.
2. The API has no re-sign endpoint. Do not edit a stored report or append a
   signature by hand. Re-run verification after fixing signing so aggregation
   produces a new signed report.
3. If a key is compromised, revoke it, publish the new public key, and treat
   reports signed by the old key as untrusted after the revocation time.
4. If the report is already distributed unsigned, label it as unsigned in every
   downstream system.
5. For a verifier trust failure, update the trust store; never disable signature
   verification for production evidence.

## Invariants not to break

- A report is `SIGNED` only when at least one valid signature is present.
- Never mutate a stored report, its manifest or its Merkle root.
- Key rotation changes the key ID mapping; it does not change evidence digests.
- The verifier checks digest, size, manifest completeness and trust before it
  reports `VALID_INTEGRITY`.
- A signing failure must not turn an inconclusive or failed verification into a
  passing one.
