# ADR-006: One home region and one active writer epoch per tenant

- Status: accepted
- Date: 2026-11-11

## Context

A globally consistent database across regions adds latency and failure modes
that are not required for patch verification. A run naturally belongs to one
region, one PostgreSQL primary and one CAS locality. Data residency also forbids
moving some repositories across regions.

## Decision

Each tenant has a `home_region` and an `active_writer_epoch`
(`internal/domain/tenant.go`). Only the region holding the current epoch accepts
writes for that tenant. `UpdateTenantEpoch` performs an expected-epoch
compare-and-swap when promoting a new writer.

The repository does not implement a global tenant directory, cross-region
replication or automated migration. Those are manual operations governed by the
region migration runbook and invariants. Execution clusters are region-scoped
and advertise `cas_region`; placement prefers local CAS.

## Consequences

- There is no multi-region active-active write path. A region failure may require
  unavailability or a controlled promotion, not automatic dual-write.
- A tenant migration is a freeze, copy, verify and single-writer cutover. In-flight
  processes are not migrated; work is re-run or cancelled.
- Reads for a tenant are served by its home region; a global read cache is not
  part of v1.
- Compliance policy can refuse a move even when capacity is available; the
  system prefers residency over availability.
