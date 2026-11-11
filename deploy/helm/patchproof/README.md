# PatchProof Helm chart

This chart installs the regional control plane: API, controller, an optional
cluster agent, the pre-install/pre-upgrade migration Job, a shared ConfigMap
plus a controller-specific ConfigMap for signing settings, services, PDBs,
optional HPAs, optional ServiceMonitor and default-deny NetworkPolicies.
It does not install PostgreSQL, NATS, an S3-compatible store or an execution
cluster.

## Prerequisites

- Kubernetes 1.28 or newer with a CNI that enforces NetworkPolicy.
- An external PostgreSQL 16-compatible database, NATS JetStream with the stream
  `PATCHPROOF`, and an S3-compatible bucket.
- Secrets created out of band (the chart only references them):

| Secret | Keys | Used by |
|---|---|---|
| PostgreSQL DSN | `dsn` (default) | API, controller, cluster agent, migration Job |
| S3 credentials | `access-key`, `secret-key` | API, controller, cluster agent |
| Attempt token key | `key`, at least 32 bytes | API, controller |
| OIDC settings | chart values, not a Secret | API, controller |
| Development token | `token`, only when `auth.dev.enabled` | API, controller |
| Report signing key | `signing.pem`, `signing.pub` | controller |
| Cluster agent token | `token`, only when `clusterAgent.enabled` | controller, cluster agent |

Example creation commands use shell variables so no credential is committed:

```sh
kubectl -n patchproof-system create secret generic patchproof-postgres \
  --from-literal=dsn="$PATCHPROOF_POSTGRES_DSN"
kubectl -n patchproof-system create secret generic patchproof-s3 \
  --from-literal=access-key="$AWS_ACCESS_KEY_ID" \
  --from-literal=secret-key="$AWS_SECRET_ACCESS_KEY"
kubectl -n patchproof-system create secret generic patchproof-attempt-token \
  --from-literal=key="$(openssl rand -hex 32)"
kubectl -n patchproof-system create secret generic patchproof-signing \
  --from-file=signing.pem=./signing.pem --from-file=signing.pub=./signing.pub
```

The signing key must be an Ed25519 key in PKCS#8 PEM form. Both files are
required because the controller does not write into a read-only secret mount.

## Install

Image digests are mandatory. The chart rejects tags:

```sh
helm upgrade --install patchproof deploy/helm/patchproof \
  --namespace patchproof-system --create-namespace \
  --set auth.mode=oidc \
  --set auth.oidc.issuer=https://issuer.example.org \
  --set auth.oidc.audience=patchproof \
  --set auth.oidc.jwksUrl=https://issuer.example.org/.well-known/jwks.json \
  --set images.api.digest=sha256:... \
  --set images.controller.digest=sha256:... \
  --set execution.workerImage.digest=sha256:... \
  --set migrations.image.digest=sha256:...
```

`--set` is used only for the example. In a release pipeline, put the values in a
reviewed values file and pin the chart version.

The migration Job reads the DSN from the PostgreSQL Secret and applies the SQL
files in `files/migrations/`. These files are copies of
`migrations/postgres/*.sql`; `make sync-helm-migrations` refreshes them and CI
checks that they have not drifted.

## Worker image and execution cluster

The chart does not deploy workers. The controller (or cluster agent) creates one
Kubernetes Job per attempt in `execution.namespace` with:

- `RuntimeClass` = `execution.runtimeClass` (default `gvisor`),
- non-root UID 65532, read-only root filesystem, all capabilities dropped,
  `RuntimeDefault` seccomp, no service account token by default,
- one emptyDir for `/workspace` and one for `/scratch`,
- a step spec ConfigMap and a per-attempt token Secret.

Before enabling untrusted verification:

1. Install and validate the runtime class (for example gVisor `runsc`). The Job
   fails to start if the RuntimeClass does not exist; the controller does not
   silently fall back to `runc`.
2. Create a dedicated node pool with a taint, and give the worker Jobs a
   matching toleration. This chart does not create node pools or taints.
3. Create `execution.namespace` and the worker ServiceAccount. The worker image
   digest must be the same value used in `execution.workerImage.digest`.
4. Enforce a default-deny NetworkPolicy in the execution namespace that allows
   only the API callback endpoint and the artifact store (or a stage-in/stage-out
   broker for offline mode).
5. Node pool: set `execution.nodeSelector` and `execution.tolerations` so Jobs
   land on the dedicated pool. Example for a tainted gVisor pool:

   ```yaml
   execution:
     nodeSelector:
       pp.io/pool: untrusted-gvisor
     tolerations:
       - key: workload
         operator: Equal
         value: untrusted
         effect: NoSchedule
   ```

   The chart does not create node pools or taints; it renders the selector and
   tolerations into the controller and cluster-agent configuration that the
   Kubernetes executor uses.

The executor injects non-secret worker environment for role, service name,
region, in-memory metadata, disabled events, CAS driver/root or S3
endpoint/region/bucket, `PATCHPROOF_WORKER_WORKDIR=/workspace`, and the attempt
token/callback/spec/engine image. Static S3 access keys are deliberately not
injected. Workers should obtain object-store access through workload identity
(IRSA or equivalent); set `execution.automountWorkerToken=true` to expose the
pod service account token to the worker, and scope that service account tightly.
The tradeoff is that a compromised worker pod can use the token within its
identity's permissions. The shipped worker image sets `TMPDIR=/scratch`, which
the Job mounts as writable emptyDir. The chart documents
this instead of rendering a worker Deployment that would not match the
executor's actual contract.

## Brokered offline IO

For `artifactTransport: brokered-offline-io` the agent creates a per-attempt
ReadWriteOnce PVC from `execution.brokeredStorageClass` and three Jobs:
`stage-in`, `execute`, `stage-out`. Configure:

```yaml
execution:
  brokerImage:
    repository: ghcr.io/patchproof/patchproof-broker
    digest: sha256:...
  brokerServiceAccount: patchproof-broker
  brokeredStorageClass: standard-rwo
  workspaceSizeGiB: 20
worker:
  brokerServiceAccount:
    create: true
    name: patchproof-broker
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::...:role/patchproof-broker
```

The broker service account needs object-store access and the callback endpoint;
the worker service account does not. The chart creates a NetworkPolicy selecting
`patchproof.io/phase=execute` with empty ingress and egress. That policy is what
makes the execute sandbox offline; without it, brokered mode provides credential
separation but not network isolation.

## External plugin engines

`execution.plugins` is rendered into the control-plane configuration and, for
Kubernetes workers, into `PATCHPROOF_PLUGINS_JSON`. The plugin binaries must be
baked into the worker image and pinned by digest. See
[docs/architecture/plugins.md](../../../docs/architecture/plugins.md).

## Action cache

`scheduler.actionCacheEnabled`, `scheduler.actionCacheTtl` and
`scheduler.cacheEpoch` control reuse of hermetic analyze/impact/compile results.
Bump `cacheEpoch` when an engine or worker image changes. See
[docs/architecture/action-cache.md](../../../docs/architecture/action-cache.md).

## Cluster agent

`clusterAgent.enabled=true` deploys one agent per execution cluster. It needs a
bearer token, a cluster id, and permission to manage Jobs, Pods, ConfigMaps,
Secrets and PVCs in the execution namespace (`clusterAgent.rbac.create=true` creates a
namespaced Role and RoleBinding).

The agent registers `ExecutorKind=remote` and an endpoint from
`execution.agentEndpoint` (or `externalUrl` when unset). Set
`execution.agentEndpoint` to a URL reachable from the controller; the
controller's remote executor dials that endpoint with the agent token. The
agent advertises the tiers in `execution.supportedIsolationTiers`; untrusted
tiers require `execution.runtimeClass`.

## Development auth

`auth.mode=dev` is refused unless `auth.dev.enabled=true`, and it requires a
static token Secret and a tenant UUID:

```sh
helm upgrade --install patchproof deploy/helm/patchproof \
  --namespace patchproof-system --create-namespace \
  --set auth.mode=dev --set auth.dev.enabled=true \
  --set auth.dev.tenantId=11111111-1111-4111-8111-111111111111 \
  --set images.api.digest=sha256:... \
  --set images.controller.digest=sha256:... \
  --set execution.workerImage.digest=sha256:... \
  --set migrations.image.digest=sha256:...
```

Development auth does not provision the tenant row. Create it with
`platform-admin` credentials or seed it explicitly. Never expose a dev-auth
installation to untrusted clients.

## Network policy

`networkPolicy.enabled=true` creates a default-deny policy for all chart pods
and explicit rules for DNS, the API ingress, metric scraping, PostgreSQL, NATS,
S3/HTTPS, OTLP and the cluster agent. CIDRs default to `0.0.0.0/0` limited to
the listed ports; narrow them to the actual database, broker, object-store and
collector addresses. The ingress namespace selectors default to all namespaces
for the API; restrict `networkPolicy.ingress.api.namespaceSelector` to the
ingress controller namespace in production.

## Validate locally

```sh
helm lint deploy/helm/patchproof -f ci-values.yaml
helm template patchproof deploy/helm/patchproof -n patchproof-system -f ci-values.yaml | kubectl apply --dry-run=client -f -
```

`helm template` without values fails by design on the first missing image
digest, OIDC setting or signing secret name.
