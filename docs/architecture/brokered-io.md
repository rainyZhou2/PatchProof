# Brokered offline artifact IO

In brokered mode the executing sandbox has no network and no platform
credentials. A trusted broker performs the two networked halves on a
per-attempt volume.

```text
remote CAS ── stage-in (broker, network) ──▶ PVC /work
                                             │
                                  execute Job (no network, no token)
                                             │  local CAS + result.json
                                             ▼
remote CAS ◀── stage-out (broker, network) ── PVC /work
                       │
                       └── callback API (attempt token) ──▶ PostgreSQL
```

## Kubernetes sequence

1. The cluster agent creates a PVC `<attempt>-ws` using
   `execution.brokeredStorageClass` and `execution.workspaceSizeGiB`.
2. Stage-in Job: broker image, broker service account, CAS/workload identity,
   spec ConfigMap, mounts the PVC at `/work`, downloads `NamedInputs` into a
   local content-addressed store, writes `input-manifest.json`.
3. Execute Job: worker image, worker service account, **no attempt-token Secret,
   no CAS/AWS environment, no callback**, mounts the PVC. The pod carries
   `patchproof.io/phase=execute`, which the chart NetworkPolicy selects for a
   full ingress/egress deny. Engines write artifacts into the local CAS and the
   worker writes `result.json`.
4. Stage-out Job: broker image, broker service account, token Secret and
   callback URL, uploads every local blob after digest verification, then calls
   `Start`/`Complete` (or `Fail`) with the attempt token.
5. The agent deletes the PVC after stage-out succeeds.

## Invariants

- The execute container never possesses CAS credentials, an attempt token or
  network egress.
- Only blobs whose digest matches their path are uploaded.
- A missing or corrupt `result.json` is a stage-out failure, never a success.
- Stage and execute pods share only the attempt volume; names are deterministic
  and owned by the attempt.

## Limits

- The volume must support ReadWriteOnce and be mountable sequentially by the
  three Jobs; a storage class that cannot detach promptly will delay phases.
- Brokered mode requires the broker image and a separate broker service account
  with workload identity. These are configured by
  `execution.brokerImage`, `execution.brokerServiceAccount` and the chart's
  `worker.brokerServiceAccount`.
- The cluster must enforce the `phase=execute` NetworkPolicy; the executor
  cannot enforce network isolation from inside a pod.
