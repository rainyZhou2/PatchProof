# Threat model

This document covers the code in this repository and the deployment assets in
`deploy/`. It describes what the current implementation enforces, not what a
future release might enforce.

## Assets

- source snapshots and patches, including private repository content,
- trusted verification policies and their digests,
- evidence documents, counterexamples, reports and Merkle manifests,
- report signing keys and attempt token keys,
- tenant identities, roles, quotas and audit records,
- execution capacity and the integrity of the control plane.

## Trust boundaries

| Boundary | Trusted | Untrusted | Primary controls |
|---|---|---|---|
| T0 | API, controller, scheduler, PostgreSQL, policy store, signing key, cluster agent process | — | Private network, RBAC, secret references, audit log. |
| T0→T1 | Bundled engines and digest-pinned worker/engine images | Snapshots, patches, contract files, tool output | CAS digest verification, resource limits, schema validation. |
| T1→T2 | Worker process and sandbox runtime | Code under test, tests, generated files, dependencies | Kubernetes Job security context, RuntimeClass, network policy, no service account token by default. |
| T2→T3 | Git provider adapter, CI integration | Remote Git, webhooks, third-party reports | Project-admin-controlled repository URI, size limits, authenticated API. |

The T1/T2 distinction is important: the analyzer binary is trusted, but the Go
source it parses is not. Parsing must not follow arbitrary symlinks, allocate
without bound, or execute code from the snapshot.

## Threats and mitigations

### Malicious patch or test executes commands

A pull request can contain `go generate`, test helpers or build scripts that run
arbitrary commands.

- The Kubernetes executor runs one Job per attempt with `runAsNonRoot`,
  UID/GID 65532, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`,
  all capabilities dropped, `seccompProfile: RuntimeDefault`,
  `automountServiceAccountToken: false` by default, and `enableServiceLinks: false`
  (`internal/adapters/executors/kubernetes/job.go`).
- The step spec declares isolation, network mode and artifact transport.
  `internal/policy` rejects untrusted isolation tiers with a subject network
  other than `none` (`NETWORK_POLICY_WEAKER_THAN_ISOLATION`).
- The `trusted-local` executor runs in the control-plane process. It is for
  trusted local code only; `cmd/patchproof` rejects a local policy whose
  `isolationTier` is stronger than `trusted-local`, but it cannot make local
  execution safe for a hostile repository.
- CPU, memory, ephemeral disk and deadline are bounded by the step requirements
  and the Job's `activeDeadlineSeconds`; Go build output is capped by engine
  limits.

Residual risk: a runtime or kernel escape, a malicious dependency fetched by an
allowed proxy, or a toolchain vulnerability is not contained by these settings.

### Container escape and lateral movement

- gVisor (`untrusted-gvisor`) or a VM runtime (`untrusted-vm`) is selected by
  `execution.runtimeClass` and the step's isolation requirement. A missing
  RuntimeClass fails the Job; it is not downgraded silently.
- Execution namespaces are separate from the control plane and should run on a
  dedicated, tainted node pool with no control-plane workloads.
- NetworkPolicy defaults to deny ingress and egress, with explicit DNS, database,
  broker, object-store and OTLP rules.
- Workers receive no database credentials. The service account token is not
  automounted by default; `execution.automountWorkerToken=true` opts in for
  workload identity (IRSA or equivalent), and the worker service account must be
  scoped accordingly.

Residual risk: NetworkPolicy is enforced only by a capable CNI; a cluster
administrator, the container runtime, the host kernel and the cloud account are
outside the boundary. gVisor and VM isolation are mitigations, not proofs.

### Dependency poisoning and SSRF

- `engines/gobuild` runs with `GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`
  and `GOWORK=off` unless the step explicitly sets a proxy. A proxy is an
  explicit, policy-visible exception.
- Repository size, changed-file count and per-file limits are enforced by the
  policy engine and the snapshot adapter where configured.
- Submodules are rejected when the policy requires it
  (`SUBMODULES_NOT_ALLOWED`); the built-in controller policy enables this.
- The worker stages only `cas_ref` inputs; it does not accept arbitrary URLs.
- Project repository URIs are administrator-controlled. A project with a
  `file:` URI is restricted to `trusted-local` classification
  (`internal/domain/tenant.go`).

Residual risk: a Git provider or module proxy can serve malicious content to a
trusted fetch path. Snapshot content remains untrusted; allowlists and egress
policy must be configured by the operator.

### Policy bypass

- The policy engine loads a stored policy digest; the run freezes the digest and
  the report records it. A pull request cannot replace the trusted policy in the
  request path.
- `RequireTrustedPolicy` rejects a policy not marked trusted.
- Protected paths (for example `.patchproof/**` and `.github/workflows/**`) and
  project policy protected paths produce `PROTECTED_PATH_MODIFIED`.
- Isolation floors can be raised by tenant security tier
  (`EnforceTenantFloor`: regulated → VM, elevated → gVisor, standard →
  container) and by the platform minimum.
- The hard policy can require `subjectNetwork=none` regardless of a weaker
  project policy.

Residual risk: the administrator who can write policies or change the policy
store is inside T0. Policy changes must be audited and reviewed.

### Evidence tampering

- Evidence is validated on write and again during aggregation. Invalid evidence
  is excluded and forces an `INCONCLUSIVE` verdict rather than a pass.
- The report references a CAS manifest and a Merkle root over its leaves; the
  offline verifier checks structure, digests, sizes, signature and trust root.
- Reports are immutable versions keyed by `(run_id, report_id)`; retrying
  aggregation inserts a new version instead of replacing the prior signed
  report. `GetReport` returns the latest version.
- Signature status controls whether the report is presented as `SIGNED`.

Residual risk: a compromised control plane or signing key can produce a
fraudulent but internally consistent report. Signing proves origin and
integrity, not semantic truth.

### Secret and credential leakage

- No credential is rendered into a ConfigMap in the Helm chart; the chart
  references Secrets with `secretKeyRef` and mounts the signing key read-only.
- Attempt tokens are short-lived and bound to one attempt id and generation.
- The worker has no database password, no cloud long-lived key and no service
  account token.
- Presigned CAS URLs are object-scoped and time-limited; they are bearer
  capabilities and must not be logged or shared.

Residual risk: application logs and error messages are not guaranteed to be
fully redacted; avoid putting secrets in repository paths, commit messages or
step options. Node-level access exposes mounted Secrets and `/tmp` spool files.

### Resource exhaustion

- Per-tenant quotas cover concurrent runs, concurrent steps, CPU, fuzz time,
  storage and API RPS.
- The scheduler enforces per-tenant and per-cluster step caps.
- Jobs have `activeDeadlineSeconds`, a TTL after finish, and resource
  requests/limits. The worker caps extracted bytes and artifact counts.
- `http.MaxBytesReader` bounds request bodies; list limits are clamped.

Residual risk: a very large but allowed repository can still consume snapshot
bandwidth and disk. Budgets are policy-controlled, not attacker-controlled.

### Cache poisoning

The action cache is implemented in `internal/actioncache` and backed by the
metadata store. It keys on tenant, cache epoch, step kind, engine and version,
snapshots, policy, input manifest, architecture, isolation, network, runtime
class, worker image and step options; only positive analyze/impact/compile
results are stored. Cross-tenant reads are prevented by the store key and the
tenant-scoped CAS namespace.

A hit is cloned only after every referenced result artifact and evidence input
has been `Stat`ed in the tenant's run CAS scope. A missing blob discards the hit.
The key includes the engine image, configured plugin environment digest and the
resolved snapshot input digest in addition to the action fields.

### Symlink and path handling

- The worker rejects tar entries that are absolute, contain `..`, or are
  symlinks/hard links (`internal/worker/runner.go`).
- Input bindings reject absolute paths and `..` (`internal/domain/resources.go`).
- The analysis snapshot walkers skip symlinks; the Go build workspace resolves
  the workspace and cache roots with `EvalSymlinks` and verifies containment.
- Workspace and cache paths are created with restrictive permissions.

Residual risk: path handling differs across engines and host filesystems. New
archives, engines or adapters must add negative tests for traversal, links and
case-insensitive collisions.

### S3 temporary spooling

`internal/adapters/cas/s3` streams a blob to a temporary file while hashing it,
then uploads it. The temporary file is removed on completion, but:

- it is written to the process temporary directory, which must be writable and
  sized for the largest artifact,
- it contains plaintext repository/evidence content on the node,
- a crash or full disk can leave debris.

Mitigations: run the S3-backed controller/worker on dedicated nodes, bound the
temporary directory, enable node disk encryption and monitor free space. The
worker image sets `TMPDIR=/scratch`, which the Job template mounts as an
emptyDir; a custom image without that setting needs a writable temporary path.

### Prompt injection and LLM output

No LLM is part of the verification pipeline in this tree. Git commit messages,
source comments, generated reports and webhook payloads must still be treated as
untrusted data; they cannot change policy, grant a role or produce a verdict.
If an LLM integration is added, it must not hold signing authority or control
plane credentials.

## Residual risks summary

- NetworkPolicy depends on the CNI and the policy is only as narrow as the
  configured CIDRs.
- gVisor/VM isolation reduces, but does not eliminate, kernel and runtime attack
  surface.
- The local executor is trusted-only and is not a sandbox.
- A control-plane or signing-key compromise defeats integrity guarantees.
- Repository and dependency fetches can be poisoned upstream.
- Temporary spool files and node access expose plaintext data.
- The cluster agent authenticates callers with a bearer token over HTTP; TLS
  termination and mTLS are deployment responsibilities.
- Workers receive non-secret CAS endpoint configuration, never static S3 keys.
  When `execution.automountWorkerToken=true` is used for IRSA/workload identity,
  a compromised worker can exercise the permissions of that identity; scope the
  service account to the specific bucket and prefix.

## Plugin, broker and cache-specific threats

| Threat | Control |
|---|---|
| Plugin returns a path that escapes its output directory | Host rejects absolute paths, `..`, symlinked components, directories and non-regular files; every output is re-validated before upload. |
| Plugin tries to smuggle evidence without artifacts | Raw evidence JSON is rejected; only typed drafts whose artifact names resolve to uploaded blobs are accepted. |
| Plugin exceeds resource or size bounds | 256 files, 4 GiB, 512 drafts, protocol frame cap and per-execution timeout. |
| Broker stage-out uploads a corrupted blob | Every local blob is opened through the verifying reader and its remote digest/size is compared before success. |
| Execute sandbox reaches the network | Brokered execute Job carries no token or CAS credentials and matches the chart's `phase=execute` deny-all NetworkPolicy; enforcement must be verified per cluster. |
| Stage-out submits a stale result | The attempt token is generation-scoped; the controller fences the commit. |
| Cache poisoning across tenants or runs | Cache entries are keyed by `(tenant_id, action_key)`; the key includes tenant, policy, snapshots, engine, options and cache epoch; only successful positive evidence is stored. |
| Cache hides a regression | Test, differential, fuzz and security steps are never cacheable; counterexamples and non-positive evidence are never stored. |
