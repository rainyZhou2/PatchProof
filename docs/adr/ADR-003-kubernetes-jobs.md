# ADR-003: Kubernetes Jobs are the execution unit

- Status: accepted
- Date: 2026-11-11

## Context

One attempt must be isolated, bounded and independently retryable. A long-lived
worker deployment would mix attempts, complicate credential scoping and make
retry ownership ambiguous.

## Decision

The Kubernetes executor creates one Job per attempt
(`internal/adapters/executors/kubernetes`):

- `backoffLimit: 0`, `completions: 1`, `parallelism: 1`, `restartPolicy: Never`,
- `activeDeadlineSeconds` and `ttlSecondsAfterFinished`,
- runtime class from the execution config,
- non-root UID 65532, read-only root filesystem, all capabilities dropped,
  `RuntimeDefault` seccomp,
- no service account token by default (opt-in for workload identity), no service links,
- a step spec ConfigMap and a per-attempt token Secret,
- CPU, memory and ephemeral-storage requests/limits from the step spec.

PatchProof owns retries. Kubernetes restarts are disabled; a lost or failed
attempt is expired or failed in the database and retried with a new generation.
The chart does not deploy workers; only the control plane is installed.

## Consequences

- Job startup overhead is paid per attempt; batching would be faster but weaker
  isolation.
- The execution cluster needs the RuntimeClass, node pool and RBAC prepared
  before work can run.
- The Job template injects non-secret worker configuration (role, region,
  metadata/event drivers, CAS driver and endpoint/bucket, work directory) plus
  the attempt token, callback URL, cluster id, spec file and engine image.
  Static S3 keys are not injected; workload identity or an explicitly enabled
  service account token is the credential path.
- Node selectors and tolerations are configuration, not chart-created node
  pools. The operator must provision and taint the execution pool.
- The cluster agent is optional for clusters that run the controller's direct
  Kubernetes executor.
