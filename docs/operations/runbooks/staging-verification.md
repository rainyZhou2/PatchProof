# Runbook: staging cluster acceptance

Use this after every release candidate and before promoting to production.

## Preconditions

- Helm release is `deployed`; API `/readyz` returns 200.
- Cluster agent has registered with `ExecutorKind=remote`, a non-empty endpoint
  and `healthy=true`.
- The execution namespace has the `patchproof-worker` service account, the
  `gvisor` RuntimeClass and the tainted node pool.
- The S3 bucket is reachable from the stage-in/out path.

## Checks

### 1. Direct mode

```sh
patchproof verify --api "$API" --token "$TOKEN" --project "$PROJECT" \
  --repo ./fixture --base HEAD~1 --head HEAD --policy deploy/staging/policy-direct.yaml
patchproof proof verify --report report.json --cas-root ./cas \
  --key staging=/etc/patchproof/signing.pub --policy deploy/staging/policy-direct.yaml
```

Expected: `VERIFIED_WITHIN_SCOPE`, `VALID_INTEGRITY`. Inspect the worker Job:

```sh
kubectl -n patchproof-execution get job -l patchproof.io/step-id -o yaml
```

The container must be non-root, `readOnlyRootFilesystem: true`, all
capabilities dropped, `seccompProfile: RuntimeDefault`, and
`automountServiceAccountToken` must be false unless workload identity was
explicitly enabled.

### 2. Brokered mode

Submit with `deploy/staging/policy-brokered.yaml`. While it runs:

```sh
kubectl -n patchproof-execution get pvc,jobs,pods \
  -l patchproof.io/attempt-id -o wide
```

Assertions:

- stage-in Job completes before the execute Job is created;
- the execute Job has label `patchproof.io/phase=execute`, no attempt-token
  Secret, no `PATCHPROOF_S3_*`/`AWS_*`/`PATCHPROOF_ATTEMPT_TOKEN` environment,
  and `automountServiceAccountToken: false`;
- network policy denies egress from `phase=execute`; a deliberate outbound
  request from the worker must fail;
- stage-out Job uploads and commits; the PVC is deleted after completion.

### 3. Missing RuntimeClass

Temporarily remove the RuntimeClass or set `execution.runtimeClass=""` in a test
release, then request `untrusted-gvisor`. The Job must not be created and the
step must fail with `SANDBOX_UNAVAILABLE`; there must be no runc fallback.
Restore the class immediately.

### 4. Action cache

Submit the same base/head/policy twice with different `Idempotency-Key` values.
In the second run, analyze/impact/compile steps should complete without new
worker Jobs, and `patchproof_cache_hit_total{cache_kind="action"}` must increase.
Bump `scheduler.cacheEpoch` and confirm the next run executes again.

### 5. Node loss

Cordon and drain a node while a step runs. Expected: attempt is lost, lease
expires, a new generation is scheduled on another node, and only the new
generation can commit. Check `patchproof_lease_expired_total` and the step's
`generation` in `GET /api/v1/runs/{id}/steps`.

### 6. Plugin engine (optional)

Configure `execution.plugins` with a test plugin image and a policy
`verification.engines.analyze: {name: test-plugin, language: go}`. The step must
use the plugin engine; evidence `engine.protocol` must be `stdio`. Remove the
plugin registration afterwards.

## Evidence to retain

- report bundle and Merkle root,
- signature key id and image digests,
- Kubernetes version, RuntimeClass, storage class and node pool,
- the complete acceptance matrix result with deviations.

Do not promote a release if any check is unverified; document the gap and
downgrade the claimed capability instead.
