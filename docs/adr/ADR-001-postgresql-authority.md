# ADR-001: PostgreSQL is the authority for run state

- Status: accepted
- Date: 2026-11-11

## Context

Runs, steps, attempts, leases, evidence, reports and idempotency keys must
survive process crashes and broker outages. Event consumers and execution
clusters must not become a second source of truth.

## Decision

PostgreSQL is the only authority for business state. `internal/ports.MetadataStore`
is implemented by `internal/adapters/storage/postgres` for production and by
`internal/adapters/storage/memory` for tests and local development. State
transitions, generation checks and outbox rows are committed in the same
database transaction.

NATS carries notifications, CAS carries immutable bytes, and worker disks carry
one attempt's scratch data. None of them is authoritative for run state. The
orchestrator reconciles from PostgreSQL even when an expected event is missing.

## Consequences

- The event bus can be rebuilt from the outbox; losing NATS delays work but does
  not lose accepted runs.
- Scaling the controller is bounded by database connections and lock
  contention, not by an in-memory scheduler.
- Cross-region writes cannot use a single database; each region has its own
  PostgreSQL and tenant epoch.
- Database failover is a correctness event and has a dedicated runbook.
