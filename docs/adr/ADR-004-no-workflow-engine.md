# ADR-004: No Temporal or Argo in v1

- Status: accepted
- Date: 2026-11-11

## Context

Durable workflow engines provide retries, timers and visibility. PatchProof
already persists its own run, step and attempt state and must fence attempts
against stale generations. Adding a second workflow authority would create two
places that decide what runs next.

## Decision

v1 uses an in-process orchestrator and scheduler backed by PostgreSQL:

- `internal/app/orchestrator` polls and reacts to events, validates, plans and
  aggregates.
- `internal/app/scheduler` owns shard leases and dispatches attempts.
- `internal/app/attempts` enforces generation and lease checks on results.
- `internal/app/outbox` publishes notifications.

No Temporal, Argo Workflows or external workflow engine is a dependency. The
interfaces in `internal/ports` would allow a future `WorkflowEngine` or executor
adapter, but PostgreSQL remains authoritative and the fencing rules do not move
into the external engine.

## Consequences

- There is no workflow UI or generic workflow DSL to learn; operators use the
  API, PostgreSQL and metrics.
- Scheduling and retry logic is code owned by this repository, which is a
  maintenance cost.
- An external engine must not own placement for the same attempt at the same
  time. If one is introduced, it must be an adapter under the existing state
  machine, not a parallel authority.
- This decision can be revisited without changing the evidence or report
  formats.
