# Chaos and invariant tests

These tests assert the three invariants that the distributed design depends on:

1. a confirmed run is never lost,
2. no cross-tenant data is returned,
3. a stale attempt can never overwrite a newer generation.

The suite uses the in-memory metadata store so it runs in CI without
infrastructure. It covers:

- 32 concurrent schedulers attempting to lease the same READY step: exactly one
  receives a lease, the rest receive a fence/lease error;
- a duplicate notification cannot create a second lease for the same generation;
- an expired worker that later reports a successful result is rejected by the
  fencing check, while the retry generation commits normally.

For infrastructure-level chaos, run the same scenarios against PostgreSQL (see
`tests/integration`) and add fault injection for:

- PostgreSQL primary failover and connection exhaustion,
- JetStream acknowledgement loss and redelivery,
- CAS 503 responses, partial uploads and digest mismatch,
- Kubernetes node loss, eviction and image pull failure,
- cluster-agent disconnect and reconnect,
- signing service unavailability.

Every chaos scenario must record which invariant was checked and how the
platform recovered (retry, new generation, or terminal classification).
