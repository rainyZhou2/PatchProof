# Verdicts and evidence levels

PatchProof reports at two levels: each claim has a result, and the run aggregates
those results into a verdict. A run can be successful only when every
policy-required claim reaches its declared minimum evidence level.

## Claim results

| Result | Meaning |
|---|---|
| `VIOLATED` | A valid counterexample or deterministic failure contradicts the claim. |
| `PROVEN_WITHIN_MODEL` | A machine-checkable proof exists under the stated model and assumptions. |
| `TESTED_WITHIN_SCOPE` | Bounded tests or fuzzing passed within the recorded input domain and budget. |
| `UNKNOWN` | The engine could not decide, lacked an oracle, or exhausted resources. |
| `NOT_RUN` | Policy or budget skipped the claim. |
| `INVALID_EVIDENCE` | The evidence failed validation and cannot support a passing verdict. |

## Run verdicts

| Verdict | Meaning |
|---|---|
| `COUNTEREXAMPLE_FOUND` | At least one required claim was contradicted. |
| `VERIFIED_WITHIN_SCOPE` | Every required claim met its minimum evidence level. |
| `INCONCLUSIVE` | No confirmed counterexample, but required evidence is missing or unknown. |
| `POLICY_BLOCKED` | The patch or environment violated a trusted policy invariant. |
| `INFRA_FAILED` | Verification could not complete because infrastructure failed. |
| `CANCELLED` | Cancellation was requested before completion. |

## What `VERIFIED_WITHIN_SCOPE` means

It is a statement about the frozen base/head revisions, policy digest, input
domain, assumptions and budgets recorded in the report. It is not a proof of
general program correctness. Read the report's `assumptions` and `limitations`,
and check which claims were `TESTED_WITHIN_SCOPE` versus `PROVEN_WITHIN_MODEL`.

A valid signature proves who produced the report and that the referenced bytes
were not modified. It does not prove that the verified program is correct.

See [evidence and offline verification](../api/README.md#offline-verification)
and the [threat model](../security/threat-model.md) for the full guarantees.
