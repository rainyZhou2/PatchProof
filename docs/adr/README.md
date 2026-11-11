# Architecture decision records

| ADR | Decision |
|---|---|
| [ADR-001](ADR-001-postgresql-authority.md) | PostgreSQL is the authority for run state. |
| [ADR-002](ADR-002-jetstream-outbox.md) | NATS JetStream plus a transactional outbox, at-least-once. |
| [ADR-003](ADR-003-kubernetes-jobs.md) | One Kubernetes Job per attempt; PatchProof owns retries. |
| [ADR-004](ADR-004-no-workflow-engine.md) | No Temporal or Argo in v1; the internal orchestrator owns state. |
| [ADR-005](ADR-005-isolation-tiers.md) | Four isolation tiers with a policy floor. |
| [ADR-006](ADR-006-tenant-home-region.md) | One home region and one active writer epoch per tenant. |
| [ADR-007](ADR-007-evidence-levels.md) | Claim evidence levels and verdict precedence. |

ADRs describe decisions that affect state ownership, security or evidence
formats. Changes to those areas require a new ADR or an amendment, not only a
code review.
