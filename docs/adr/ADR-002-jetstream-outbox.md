# ADR-002: NATS JetStream with a transactional outbox

- Status: accepted
- Date: 2026-11-11

## Context

The control plane needs to wake the orchestrator and publish run, step, report
and attempt events without making broker availability part of the write path.
Exactly-once delivery across a database and a broker is not achievable without
distributed transactions.

## Decision

Use NATS JetStream as the event bus and a transactional outbox in PostgreSQL.

- A state change writes an outbox row in the same transaction.
- `internal/app/outbox` polls pending rows, publishes them, and marks them
  delivered.
- Delivery is at least once; consumers must tolerate duplicates and reordering.
- The stream `PATCHPROOF` is created or updated idempotently on connect. The
  current bootstrap creates it with one replica; HA of the stream is an
  operational choice that requires an explicit configuration change.
- Event envelopes carry id, type, tenant, run, generation and timestamp. Large
  payloads are never put on the bus; they are CAS references.

## Consequences

- A broker outage delays notifications; the outbox retries. It does not block
  the original database commit.
- Consumers cannot assume a single delivery. Orchestrator wake-ups are hints,
  not the only progress mechanism.
- A lost stream does not lose state. Recreating it may cause redelivery of
  events that were already processed.
- Kafka or another broker can replace JetStream behind `ports.EventBus`; the
  outbox contract stays the same.
