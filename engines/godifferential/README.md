# go-differential

Counterexample-driven differential verification for Go. The engine instruments a
base (P0) and a head (P1) checkout with a generated reflection harness, runs the
same generated inputs against both binaries, minimizes the first reproducible
differences and records CAS-backed evidence plus a counterexample bundle.

## Harness protocol

`harness.GenerateSource` writes `<snapshot>/.patchproof/harness/main.go` (removed
after the run). The harness imports the contract's package directly, so a
missing symbol fails at build time and is reported as `ENGINE_UNSUPPORTED` with
the compiler log as an artifact.

Modes:

| argv      | stdin                                   | stdout                                  |
|-----------|-----------------------------------------|-----------------------------------------|
| `-describe` | empty                                 | one signature object                    |
| `-single` | one JSON array of arguments             | one observation                         |
| `-batch`  | newline-delimited JSON arrays           | one observation per input line          |

Observation shape:

```json
{"status":"return|panic|invalid","value":[...],"panic":"<type: message>","elapsed_ms":12}
```

* Values are JSON-marshalled return values. `error` values become
  `{"__error__":"<type>: <message>"}`; values that cannot be marshalled become
  `{"__unserializable__":"<type>"}`. Non-finite floats use
  `{"__unserializable__":"<type>:NaN|+Inf|-Inf"}`.
* Arguments can pass non-finite floats (which JSON numbers cannot represent) as
  `{"__float__":"NaN|+Inf|-Inf"}` for direct float parameters.
* `elapsed_ms` is timing metadata and is excluded from canonical observations
  and equality checks.
* Target stdout is redirected to stderr, so printing from the target cannot
  corrupt the observation stream.
* Each case is capped at 1 MiB of argument JSON and 1 MiB of observation JSON.
  Batch harness processes have a per-process timeout and a bounded stdout
  capture; timeouts and overflows are recorded in evidence limits, never
  silently dropped.

## Options

| option | default | meaning |
|---|---|---|
| `contract.ref` | — | contract path relative to the head snapshot root |
| `claim.id` | contract `metadata.name` | claim identifier |
| `fuzz.seed` | `spec.domain.seed` | deterministic generator seed |
| `fuzz.max_cases` | 20000 | case budget (min with `spec.budget.maxCases`) |
| `fuzz.seconds` | 30 | wall-clock budget, capped at 600 |
| `timeout.seconds` | — | step-level wall-clock cap |
| `differential.min_cases` | 100 | cases needed for `TESTED_WITHIN_SCOPE` |
| `differential.batch_size` | 512 | cases per harness process |
| `differential.batch_timeout_ms` | 5000 or `spec.timeoutMillis` | per-process timeout |
| `differential.max_counterexamples` | 3 | stop after N distinct counterexamples |
| `oracle.float_epsilon` | 1e-9 | default tolerance for `float-eps` |
| `go.proxy` | `off` | module proxy for harness builds |

`spec.oracle.normalize` supports `float-eps`, `trim-space`, `error-string`,
`canonical-json`; `spec.oracle.ignoreFields` drops keys before comparison.

## Limitations

* Only package-level functions are supported; methods and variadic functions are
  rejected as unsupported.
* Seed generation is type-directed over JSON-encodable values: invalid UTF-8
  strings and negative zero cannot be represented. NaN/Inf are passed through the
  `__float__` extension for direct float parameters.
* No fix-invariant expression is evaluated. Findings are observable differences
  in return values/panics; for `safety` contracts a head-side panic is also a
  finding.
* Each harness batch is one process per side and runs sequentially (concurrency
  1); a hung batch is killed on timeout and recorded as an infrastructure limit.
* Harness builds run `go` with `GOTOOLCHAIN=local`, so the toolchain in `PATH`
  must satisfy the snapshot's `go.mod`.
