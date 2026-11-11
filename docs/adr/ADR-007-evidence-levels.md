# ADR-007: Evidence levels and verdict precedence

- Status: accepted
- Date: 2026-11-11

## Context

A binary "pass/fail" hides whether a claim was proven, tested, skipped or
invalidated. Users must be able to see the scope of a result and must not be
able to turn missing evidence into a pass.

## Decision

Claims carry a result from `internal/domain/enums.go`. The `Satisfies` ranking
is: `INVALID_EVIDENCE` and `VIOLATED` at 0, `NOT_RUN` and `UNKNOWN` at 1,
`TESTED_WITHIN_SCOPE` at 2, and `PROVEN_WITHIN_MODEL` at 3.

The report records every claim's minimum required result and its actual result.
`domain.ComputeVerdict` applies this precedence:

1. a policy block → `POLICY_BLOCKED`,
2. cancellation → `CANCELLED`,
3. no required claims or invalid evidence → `INCONCLUSIVE`,
4. a violated required claim → `COUNTEREXAMPLE_FOUND`,
5. all required claims at or above their minimum → `VERIFIED_WITHIN_SCOPE`,
6. otherwise, infrastructure failure with no positive evidence →
   `INFRA_FAILED`,
7. remaining cases → `INCONCLUSIVE`.

`PROVEN_WITHIN_MODEL` is used only when an engine actually produced a checkable
proof under the recorded assumptions. Bundled Go engines produce compile,
test, differential and fuzz evidence, not formal proofs. `TESTED_WITHIN_SCOPE`
records the input domain, seed/case counts, budget, engine version and image
digest; it does not mean the whole input space was covered.

Signatures and Merkle roots protect integrity and origin. They do not upgrade a
claim's evidence level and do not prove semantic correctness.

## Consequences

- A report can be validly signed and still be `INCONCLUSIVE`.
- Invalid evidence is excluded and forces a non-passing verdict; it is never
  silently dropped.
- New engines must map their output to an existing evidence level or introduce a
  new schema version with a verifier that understands it.
- Consumers must read the claim table and limitations, not only the top-level
  verdict.
