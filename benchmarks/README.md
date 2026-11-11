# Benchmarks

This directory defines the evaluation protocol for PatchProof. It does not
contain measured results for this tree; publish numbers only from a run whose
source commit, host configuration and corpus digests are recorded.

## Corpus classes

| Class | Expected outcome |
|---|---|
| `correct-patch` | `VERIFIED_WITHIN_SCOPE`; no counterexample |
| `partial-fix` | `COUNTEREXAMPLE_FOUND` in the un-fixed boundary |
| `overfix` | `COUNTEREXAMPLE_FOUND` for a formerly valid input |
| `test-weakening` | `POLICY_BLOCKED` when protected paths are touched, otherwise `INCONCLUSIVE` |
| `compatibility-regression` | `COUNTEREXAMPLE_FOUND` or `TESTED_WITHIN_SCOPE` with a finding |
| `security-regression` | required security claim `VIOLATED` |
| `nondeterministic` | `INCONCLUSIVE`, never `VERIFIED_WITHIN_SCOPE` |
| `adversarial-patch` | no false `VERIFIED_WITHIN_SCOPE` |
| `plugin-crash` | `INCONCLUSIVE` or `INFRA_FAILED`, never `VERIFIED_WITHIN_SCOPE` |
| `environment-drift` | `INCONCLUSIVE` with the drift recorded |

Each case must record `base_commit`, `head_commit`, policy digest, contract
digests, expected findings, known limitations and the wall-clock budget. Keep
the evaluation set separate from any corpus used to build or tune a patch
generator.

## Metrics

```text
precision                  = TP / (TP + FP)
recall                     = TP / (TP + FN)
valid_counterexample_rate  = reproduced_counterexamples / reported_counterexamples
unknown_rate               = unresolved_claims / all_claims
false_block_rate           = blocked_correct_patches / correct_patches
cost_per_valid_counterexample = total_compute_cost / valid_counterexamples
```

Report metrics per corpus class and per language. Also report queue wait
P50/P95/P99, step duration, retries by error class and CAS bytes per run.

## Reproduction requirements

A published result must include:

- the exact commit of this repository and all engine image digests,
- the host or node shape (CPU, memory, architecture), node count and region,
- the policy and contract files,
- the random seeds and case budgets,
- whether the network was enabled for subject code,
- the raw report bundles and their Merkle roots.

The report bundle is sufficient to recompute the verdict:

```sh
patchproof proof verify --report report.json \
  --cas-root /path/to/cas \
  --key release=/path/to/release.pub \
  --policy policy.yaml
```

`VALID_INTEGRITY` confirms the bundle and signature; reproduce the run with the
same images and inputs to re-confirm the verdict.
