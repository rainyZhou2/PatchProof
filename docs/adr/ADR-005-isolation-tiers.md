# ADR-005: Four isolation tiers with a policy floor

- Status: accepted
- Date: 2026-11-11

## Context

Patch verification executes code from pull requests. The risk differs between a
developer's local trusted repository and a multi-tenant SaaS receiving untrusted
PRs. A single runtime setting would either make local work unusable or silently
weaken production isolation.

## Decision

Define four isolation tiers in `internal/domain/enums.go`:

| Tier | Intended use |
|---|---|
| `trusted-local` | Local CLI and development on trusted code. In-process execution. |
| `isolated-container` | Trusted internal code or explicitly accepted risk, with container hardening. |
| `untrusted-gvisor` | Untrusted PRs with a gVisor `RuntimeClass` and a dedicated node pool. |
| `untrusted-vm` | High-risk or regulated tenants with Kata/microVM or equivalent isolation. |

The effective tier is the maximum of the policy tier, the platform minimum, the
tenant security floor and the requested tier. The policy engine raises the floor
and reports a violation when an untrusted tier requests a subject network other
than `none`. `internal/bootstrap` sets the platform minimum from
`execution.defaultIsolation` and requires `subjectNetwork=none` in its hard
policy.

## Consequences

- A local run cannot silently claim a stronger isolation than it provides; the
  CLI rejects a local policy above `trusted-local`.
- Operators must install the RuntimeClass and node pool. A missing class fails
  the Job rather than falling back to `runc`.
- Stronger tiers cost more startup time and may not support every syscall needed
  by compilers or fuzzers; compatibility must be tested.
- Isolation is a risk reduction, not a proof. Host, runtime and supply-chain
  compromise remain outside the boundary.
