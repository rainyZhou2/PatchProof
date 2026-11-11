# Runbook: worker failures

Workers run one attempt each. The controller, not Kubernetes, owns retries:
Jobs are created with `backoffLimit: 0` and `restartPolicy: Never`, and an
attempt that is lost is expired and retried with a new generation.

## Symptoms

- Jobs stay `Pending`, show `ImagePullBackOff`, `CreateContainerConfigError`,
  `OOMKilled`, `Evicted` or disappear with the node.
- Attempts become `LOST`, `EXPIRED` or `FAILED_INFRA`; steps enter
  `RETRY_WAIT` and later `FAILED` or `TIMED_OUT`.
- Runs end `INFRA_FAILED` when the failure is classified as infrastructure.
- Workers log a spec read error, input staging error, digest mismatch, engine
  panic or callback error.

## Signals and dashboards

- `patchproof_step_retries_total{error_class,region}`.
- `patchproof_engine_errors_total{engine,error_class}`.
- `patchproof_worker_utilization{cluster,pool,resource}`.
- `patchproof_inflight_steps{cluster}` and `patchproof_active_steps`.
- `patchproof_lease_expired_total`.
- Kubernetes: `kubectl -n <execution-ns> get jobs,pods`, events, container
  termination reason, node conditions.

## Diagnosis

1. `Pending`: insufficient CPU/memory, unsatisfied node selector/affinity, a
   taint without a matching toleration, or a missing RuntimeClass. The executor
   applies `execution.nodeSelector` and `execution.tolerations`; a missing
   gVisor RuntimeClass is a hard failure and the controller does not fall back
   to `runc`.
2. `ImagePullBackOff`: wrong digest, missing imagePullSecret, or registry
   unreachable from the execution node pool.
3. `OOMKilled` or `Evicted`: the step's declared memory/ephemeral disk is too
   small, the node is under pressure, or the workload exceeded its budget.
4. Spec read failure: `/run/spec/step.json` is missing or the mounted ConfigMap
   is wrong. The worker requires `PATCHPROOF_SPEC_FILE`.
5. Input staging failure: S3 reachability, bucket policy, presigned URL expiry,
   or digest mismatch. Digest mismatch is an integrity event.
6. Callback failure: `PATCHPROOF_CALLBACK_URL` must be reachable from the
   execution cluster. An empty callback URL sends the worker into offline mode,
   which requires `PATCHPROOF_WORKER_OFFLINE_OUTPUT_DIR`.
7. Worker configuration: the executor injects the non-secret role, region,
   in-memory metadata store, disabled event bus, CAS driver/endpoint/bucket and
   work directory. Static S3 keys are not injected. If staging or uploading
   fails with credentials errors, fix workload identity (IRSA or equivalent) or
   set `execution.automountWorkerToken=true` and scope the worker service
   account. The shipped image sets `TMPDIR=/scratch`; a custom image that
   spools to `/tmp` needs a writable mount.

## Remediation

1. Install the required RuntimeClass and dedicated node pool, then verify a
   smoke Job can start.
2. Correct the image digest and pull secret. Do not replace a digest with a tag
   in production values.
3. Raise the step budget or policy limits only after confirming the workload is
   legitimate; an unbounded fuzz target is a denial-of-service risk.
4. If a node was lost, let the lease expire and the attempt retry. Do not reuse
   an attempt id or lower a generation.
5. If the worker image lacks config or CAS access, fix the image or the executor
   Job template before retrying production runs.
6. If retries are exhausted, the step becomes terminal. Investigate the root
   cause; do not reset `attempt_count` in the database.

## Invariants not to break

- Workers receive no database credentials or cloud long-lived keys. The
  Kubernetes service account token is not mounted unless
  `execution.automountWorkerToken=true` is set for workload identity, and that
  identity must be bucket/prefix scoped.
- The root filesystem is read-only; only `/workspace` and `/scratch` are
  writable, plus `/tmp` only if the Job template explicitly mounts it.
- Job retries are disabled; PatchProof attempt retries are the only retry path.
- A stale generation cannot commit a result, even if its Job is still running.
- Evidence and counterexample artifacts are uploaded before the result is
  committed; a missing blob is not a success.
