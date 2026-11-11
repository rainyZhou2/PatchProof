# Runbook: NATS outage

NATS JetStream carries domain event notifications and wakes the orchestrator. It
is not the authority for business state; PostgreSQL and its outbox are.

## Symptoms

- `GET /readyz` returns `503` because readiness probes the event bus.
- Runs are accepted but remain in an early state longer than expected.
- Controller logs repeat `outbox drain failed` or `ensure stream`.
- `patchproof_outbox_failures_total` rises while
  `patchproof_outbox_published_total` is flat.
- The NATS monitoring endpoint or consumer lag shows disconnected clients or a
  growing backlog.

## Signals and dashboards

- `patchproof_outbox_failures_total{event_type}` and
  `patchproof_outbox_published_total{event_type}`.
- JetStream consumer pending count, stream storage and redelivery count.
- Controller log volume for `outbox drain failed`.
- API readiness status and `patchproof_http_requests_total{status="5xx"}`.
- `patchproof_scheduler_backlog` may rise indirectly if planning is delayed.

## Diagnosis

1. Check NATS server health and the monitoring endpoint (`:8222/healthz` in the
   Compose stack).
2. Verify URLs, credentials and TLS configuration. `PATCHPROOF_NATS_URL` and
   `PATCHPROOF_NATS_URLS` are comma-separated.
3. Verify the `PATCHPROOF` stream exists. The adapter creates or updates it on
   connect; a permissions error prevents that.
4. Check controller logs for `nats events: connect` or `ensure stream`.
5. Confirm PostgreSQL still accepts outbox rows. If it does not, this is a
   database incident first.

## Remediation

1. Restore the NATS cluster or the route to it.
2. The outbox relay retries automatically. Do not restart every controller at
   once; one healthy relay is enough to drain.
3. If a stream was lost, let the adapter recreate it. Expect duplicate
   deliveries because events may have been published before the loss.
4. If a broker migration is in progress, follow the migration plan; do not run
   two publishers with different schemas.
5. Confirm events flow and a test run advances, then verify the outbox pending
   count returns to zero.

## Invariants not to break

- Do not use NATS as the source of truth or reconstruct state from it.
- Do not delete pending outbox rows to clear an alert.
- Do not disable readiness just to make the API look healthy; a failed event bus
  delays work.
- Consumers must remain idempotent; duplicate delivery is expected.
- Do not purge the stream while consumers still need to acknowledge messages.
