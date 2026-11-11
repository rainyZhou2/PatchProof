# Architecture overview

**English** | [简体中文](../zh-CN/architecture/overview.md)

PatchProof separates a regional control plane from execution clusters. The
control plane owns identity, policy, run state, scheduling and reporting. An
execution cluster owns only the attempt it is currently running.

## Control plane

| Package | Responsibility |
|---|---|
| `internal/api` | HTTP routes, authentication, tenant rate limiting, problem documents, attempt callbacks. |
| `internal/app/runservice` | Run submission, idempotency fingerprint, list/get/cancel/report reads. |
| `internal/app/orchestrator` | Validation, snapshot creation, policy evaluation, planning, aggregation and terminal transitions. |
| `internal/planner` | Converts a policy and changed files into the step DAG and claims. |
| `internal/policy` | Built-in hard policy: isolation floor, protected paths, source limits, trusted policy, network rule. |
| `internal/app/scheduler` | Shard ownership, ready-step leasing, placement, executor dispatch, lease expiry. |
| `internal/app/attempts` | Fenced start/heartbeat/complete/fail handling for worker callbacks. |
| `internal/app/proof` | Verdict computation, evidence validation, report construction, Merkle root and optional signature. |
| `internal/app/outbox` | Drains transactional outbox rows to NATS. |
| `internal/app/admin` | Tenant, project and policy administration; cluster listing. |
| `internal/domain` | IDs, enums, state machines, evidence/report types, fencing validation. |
| `internal/ports` | Metadata store, artifact store, event bus, executor and planner interfaces. |

A control-plane process does not execute repository code except in the
`trusted-local` executor, which exists for the CLI and local development.

## Execution plane

| Package | Responsibility |
|---|---|
| `internal/adapters/executors/kubernetes` | Creates one hardened Job per attempt; observes Jobs and normalizes failures. |
| `internal/adapters/executors/local` | In-process executor for trusted-local runs and local development. |
| `internal/adapters/executors/remote` | HTTP client for a cluster agent. |
| `internal/clusteragent` | Serves `/v1/executions`, keeps cluster registration fresh, owns the Kubernetes executor. |
| `internal/worker` | Fetches inputs by digest, stages a workspace, invokes an engine, uploads outputs. |
| `engines/goanalyzer` | Go AST/SSA analysis and impact evidence. |
| `engines/gobuild` | `go build` and `go test` regression execution. |
| `engines/godifferential` | Differential execution of a base and head harness. |
| `engines/gofuzz` | Native Go fuzz target execution. |

A worker receives exactly one step spec and a non-secret environment (role,
region, in-memory metadata, disabled event bus, CAS endpoint/bucket and work
directory). It does not talk to PostgreSQL, does not list other attempts, does
not receive static S3 keys and does not receive cluster credentials. In direct
transport the worker receives a callback URL and a short-lived attempt token.

With `artifact_transport=brokered-offline-io`, the Kubernetes executor creates a
per-attempt volume and three Jobs: a trusted stage-in broker materializes the
snapshot into an attempt-local CAS, the execute worker runs with no network, no
attempt token and no CAS credentials, and a trusted stage-out broker uploads the
outputs and commits the result. The execute pod carries
`patchproof.io/phase=execute`, which the chart's NetworkPolicy selects for a
full ingress/egress deny. See [brokered-io.md](brokered-io.md).

## State ownership

| Store | Contents | Authority |
|---|---|---|
| PostgreSQL | tenants, projects, policies, runs, steps, attempts, evidence, counterexamples, reports, outbox, clusters, shard leases, audit log | Only authority for business state. |
| NATS JetStream | domain event notifications | Derived. Events can be rebuilt from the outbox; consumers must tolerate duplicates. |
| CAS | immutable blobs and snapshots | Content authority is the digest, not the object key. |
| Worker filesystem | one attempt's workspace | Disposable. Nothing outside the attempt may depend on it. |
| Memory store | development and unit tests | Never authoritative in production. |

`internal/ports.MetadataStore` is the persistence contract. The PostgreSQL
adapter and the memory adapter implement the same state transitions, including
fencing checks, so handlers cannot bypass them.

## Run lifecycle

1. `POST /api/v1/runs` reaches `internal/api.handleCreateRun`. It requires
   `Idempotency-Key`, resolves the authenticated tenant, and calls
   `runservice.Submit`.
2. `Submit` loads the active tenant and project, resolves the policy by digest
   or reference, validates the budget, computes a fingerprint of the frozen
   inputs, and calls `CreateRun`. The run row and a `patchproof.run.created.v1` outbox event are
   written in one PostgreSQL transaction. Reusing an idempotency key with a
   different fingerprint returns `409 IDEMPOTENCY_CONFLICT`.
3. The controller's orchestrator is woken by the event bus and also polls. It
   fetches the base and head snapshots through `internal/adapters/repos/git`, evaluates
   the trusted policy, and plans the DAG with `internal/planner`. `PlanRun`
   writes steps, dependencies and claims in one transaction.
4. Each step gets a shard from `step.shard = hash(run, key) % shardCount`. A
   controller replica acquires shard leases in PostgreSQL and lists ready steps
   for the shards it owns. Leasing a step increments `generation`, creates an
   `attempts` row, records a lease deadline, and issues a short-lived attempt
   token bound to the attempt id and generation.
5. The scheduler builds a `StepSpec` with CAS references for the snapshots,
   policy and contract inputs, then calls the selected executor. The Kubernetes
   executor creates a ConfigMap with the spec, a Secret with the attempt token,
   and a Job.
6. The worker downloads inputs, verifies each digest, executes the engine,
   uploads artifacts to CAS, and calls the attempt callback. `internal/app/attempts`
   loads the current step and attempt and calls `domain.ValidateCommitFence`
   before accepting a result.
7. A successful or failed step transaction updates the step, records evidence
   and counterexamples, promotes newly-ready dependents, and may move the run to
   `AGGREGATING`. The proof service validates evidence, computes the verdict,
   stores the report, signs it when a signer is configured, and emits a report
   event.

## Outbox and events

The event bus is not the source of truth. Every state change that must be
observed elsewhere writes an outbox row in the same PostgreSQL transaction as
the state change. `internal/app/outbox` polls pending rows, publishes them to
JetStream, and marks them delivered. Delivery is at least once:

- the same event may be published more than once after a crash between publish
  and mark,
- consumers must be idempotent,
- `PATCHPROOF` stream creation is idempotent via `CreateOrUpdateStream`,
- losing NATS does not lose state; it delays notifications. The outbox keeps
  retrying until the broker recovers.

Consumers wake the orchestrator; correctness does not depend on receiving a
particular event because the orchestrator also reconciles from PostgreSQL.

## Fencing and attempts

Fencing is per step and monotonic:

- `step.generation` increases when a new attempt is leased.
- An attempt token embeds `attempt_id` and `generation`.
- Start, heartbeat, complete and fail callbacks are rejected when the generation
  does not match the token, or when the attempt is not the current attempt.
- `CommitAttempt` verifies step, attempt, generation, current-attempt id, status
  `LEASED`/`RUNNING`, non-terminal attempt and a plausible timestamp before it
  mutates anything.
- A late result from an expired generation is discarded as a diagnostic; it
  cannot become evidence for the run.

Retries are decided in one place (`domain.NextStepStatus`) with bounded
exponential backoff (`domain.RetryDelay`, capped at five minutes). Kubernetes
Jobs use `backoffLimit: 0`; retries are PatchProof attempts, not Job restarts.

## CAS

`internal/ports.ArtifactStore` is addressed by `SecurityScope` plus a `CASRef`
(`sha256`, digest, size). Implementations:

- `internal/adapters/cas/s3` streams to a temporary file while hashing, uploads
  the object, and re-verifies size and digest on read. Objects are immutable.
- `internal/adapters/cas/fs` stores under a digest-derived path for local use.
- `internal/adapters/cas/memory` is for tests.

Keys are scoped by tenant and trust domain (`pkg/cas.BlobKey`), so a private
source blob from one tenant is not addressable by another tenant's scope. The
S3 adapter can issue scoped pre-signed GET/PUT URLs; callers should assume the
URL is a bearer capability for its TTL.

The action cache stores successful results for declared hermetic
analyze/impact/compile steps in PostgreSQL, keyed by tenant, cache epoch,
snapshots, policy, engine, options, architecture and isolation. Entries are
tenant-scoped, expire, and are never populated with counterexamples or
non-positive evidence. The scheduler and the worker callback path both write
entries; a hit is committed as a new fenced attempt with fresh evidence ids.
See [action-cache.md](action-cache.md).

## Action cache

Hermetic analysis and build steps can reuse a previous successful result. The
key covers tenant, cache epoch, snapshots, policy, engine, options, architecture
and isolation; only positive evidence is stored and each hit is committed as a
new fenced attempt. See [action-cache.md](action-cache.md).

## Brokered offline IO

High-security attempts can run with no network and no credentials: a trusted
broker stages inputs onto an attempt volume and uploads outputs after the
sandbox exits. See [brokered-io.md](brokered-io.md).

## Extensibility

Two extension seams exist today:

- In-process `engine.Engine` implementations register in `internal/engine` and
  are selected by `(step kind, language, engine name)`. This is how the bundled
  Go analyzer, build, differential and fuzz engines are wired.
- `pkg/plugin` implements the stdio plugin protocol (length-prefixed JSON
  frames) with the same messages as `api/proto/patchproof/plugin/v1`;
  `internal/engine/pluginengine` adapts a configured executable to
  `engine.Engine`. Plugins are registered through `execution.plugins` and
  selected by policy `verification.engines`.

There is no automatic OCI plugin loader. A plugin is only reachable if it exists
in the worker image and is registered in configuration. See
[plugins.md](plugins.md). The `Executor`, `ArtifactStore`, `EventBus` and `MetadataStore`
interfaces are the infrastructure seams; replacing PostgreSQL, NATS, S3 or
Kubernetes means implementing the interface, not changing the orchestrator.

## Region model

A region runs its own PostgreSQL, JetStream and CAS. Tenant state is not
global:

- `tenant.home_region` selects the control plane that accepts the tenant's
  writes.
- `tenant.active_writer_epoch` is the single-writer fence. A new epoch is
  promoted only when the old writer is frozen.
- `UpdateTenantEpoch` updates the epoch and home region with an expected-epoch
  compare-and-swap.
- Execution clusters are region-scoped and advertise `cas_region`; placement
  prefers a cluster whose CAS region matches the run.

The repository does not implement a global tenant directory, cross-region
replication, automated failover or active-active writes. Region migration is a
manual, rehearsed procedure; see
[docs/migration/region-migration.md](../migration/region-migration.md) and the
region migration runbook. A tenant is never written by two regions at the same
epoch.

## Trust boundaries

| Boundary | Trusted side | Untrusted side | Enforcement |
|---|---|---|---|
| T0 | API, controller, scheduler, PostgreSQL, policy, signing key | — | Network isolation, RBAC, secret references. |
| T0→T1 | Signed engine and worker images, bundled analyzers | Engine input data | Digest-pinned images, CAS digest verification, resource limits. |
| T1→T2 | Worker process and sandbox runtime | Patch code, tests, generated files, tool output | Job security context, RuntimeClass, no service account token by default, read-only root, network policy. |
| T2→T3 | Platform adapters | Git providers, CI webhooks, third-party reports | Provider allowlists, size limits, schema validation, no LLM authority. |

Source snapshots are untrusted data even though the analyzer code is trusted.
Parsing, path handling, symlink handling and resource exhaustion are therefore
T2 concerns in the analyzer and worker as well as in the sandbox.

## Failure semantics

- A run is accepted durably when its PostgreSQL transaction commits.
- Step dispatch is at least once; duplicate Job creation or duplicate delivery
  must be tolerated by attempt fencing.
- A worker result is committed only against the current generation and lease.
- A missing NATS event delays progress, never loses accepted runs.
- A CAS outage fails attempts with retryable infrastructure classes; it does not
  produce a passing verdict.
- A signing failure leaves the report `PENDING_ATTESTATION`; it is never
  presented as signed.
