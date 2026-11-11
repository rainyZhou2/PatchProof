# Staging verification

This directory contains the assets for a real-cluster acceptance run. It is not
a substitute for a security review; it verifies that the deployed control plane
and execution plane behave as designed.

## Prerequisites

- Kubernetes 1.30+ with a `gvisor` RuntimeClass installed and a dedicated,
  tainted node pool (`pp.io/pool=untrusted-gvisor`).
- A storage class that supports ReadWriteOnce for brokered attempt volumes.
- PostgreSQL, NATS JetStream and an S3-compatible bucket reachable from the
  control plane and, for stage-in/out, from the execution cluster.
- OIDC issuer/audience/JWKS and the secrets referenced by `values.staging.yaml`.
- Image digests for API, controller, cluster agent, worker and broker. Never
  deploy tags in staging.
- Workload identity (IRSA or equivalent) for the control-plane service account
  (CAS access) and the broker service account (stage-in/out). The worker
  service account is intentionally separate; it does not receive CAS keys.

## Install

```sh
export PATCHPROOF_API_DIGEST=sha256:...
export PATCHPROOF_CONTROLLER_DIGEST=sha256:...
export PATCHPROOF_AGENT_DIGEST=sha256:...
export PATCHPROOF_WORKER_DIGEST=sha256:...
export PATCHPROOF_BROKER_DIGEST=sha256:...

helm upgrade --install patchproof deploy/helm/patchproof \
  --namespace patchproof-system --create-namespace \
  -f deploy/staging/values.staging.yaml \
  --set images.api.digest="$PATCHPROOF_API_DIGEST" \
  --set images.controller.digest="$PATCHPROOF_CONTROLLER_DIGEST" \
  --set images.clusterAgent.digest="$PATCHPROOF_AGENT_DIGEST" \
  --set execution.workerImage.digest="$PATCHPROOF_WORKER_DIGEST" \
  --set execution.brokerImage.digest="$PATCHPROOF_BROKER_DIGEST"
```

Create the referenced secrets before installing:

```sh
kubectl -n patchproof-system create secret generic patchproof-postgres --from-literal=dsn='...'
kubectl -n patchproof-system create secret generic patchproof-s3 --from-literal=access-key='...' --from-literal=secret-key='...'
kubectl -n patchproof-system create secret generic patchproof-attempt-token --from-literal=key="$(openssl rand -hex 32)"
kubectl -n patchproof-system create secret generic patchproof-signing \
  --from-file=signing.pem=./signing.pem --from-file=signing.pub=./signing.pub
```

## Runner

With a CLI, API token and an existing project:

```sh
PATCHPROOF_API=https://patchproof.staging.example.org PATCHPROOF_TOKEN=... PATCHPROOF_PROJECT=... PATCHPROOF_REPO=/path/to/repo PATCHPROOF_BASE=HEAD~1 PATCHPROOF_HEAD=HEAD PATCHPROOF_POLICY=deploy/staging/policy-direct.yaml PATCHPROOF_PUBLIC_KEY=/etc/patchproof/signing.pub PATCHPROOF_CLI=patchproof deploy/staging/verify.sh
```

Set `PATCHPROOF_BROKERED=1` to run the brokered policy as well, and
`PATCHPROOF_EXECUTION_NAMESPACE=patchproof-execution` to assert that execute Jobs
carry no credentials and do not automount a service-account token.

## Acceptance matrix

Run the checklist in [docs/operations/runbooks/staging-verification.md](../../docs/operations/runbooks/staging-verification.md):

1. Direct mode run: gVisor Job, no service-account token by default, signed
   report verifies offline.
2. Brokered mode run: PVC + stage-in + execute + stage-out; the execute Job has
   no attempt token, no CAS credentials and label `patchproof.io/phase=execute`
   so the NetworkPolicy denies egress.
3. Missing RuntimeClass: a step requesting `untrusted-gvisor` must fail with
   `SANDBOX_UNAVAILABLE`, never silently fall back to runc.
4. Action cache: a second identical run reuses analyze/impact/compile results
   and does not schedule new worker Jobs for those steps.
5. Node loss: delete an execution node during a run; the attempt is fenced,
   retried with a higher generation, and no stale result is committed.
6. Plugin (optional): register an external engine in `execution.plugins` and
   verify it is selected by the policy engine mapping, not silently skipped.

Record the report Merkle root, signature key id, image digests, cluster version,
RuntimeClass, node pool and any deviation from this checklist.
