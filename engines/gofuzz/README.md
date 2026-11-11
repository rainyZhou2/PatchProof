# go-native-fuzz

Head-side native fuzzing for Go. The engine writes
`<head>/.patchproof/fuzz_target_test.go`, fuzzes it with the Go toolchain and
reports reproducible panics/crashes. It deliberately does **not** prove
differential equivalence; run `go-differential` for base/head comparisons.

## Generated target

`FuzzTarget` decodes its `[]byte` input into the target function's parameter
types through `reflect` and `encoding/json`. The primary codec is
length-prefixed JSON arguments (4-byte big-endian length per argument); a raw
JSON array is accepted as a fallback so byte-level fuzzing can mutate textual
arguments directly. Contract seeds are injected with `f.Add`.

Any panic from the target is reported as a crash. For `safety` contracts this is
the contract's no-panic invariant; for other kinds it is reported as a head-side
crash finding and flagged in evidence limits. No fix-invariant expression is
invented because the contract language cannot express one.

## Execution

```
go test -run=^$ -fuzz=FuzzTarget -fuzztime=<N>s -parallel=1 ./.patchproof
```

* `-parallel=1` keeps a single fuzz worker (v1 concurrency policy).
* Current Go releases do not implement the public `-fuzzcache` flag, so the
  engine detects it and otherwise passes the equivalent
  `-test.fuzzcachedir=<workspace>/fuzzcache` test-binary flag; the build/fuzz
  cache also lives in the workspace-local `GOCACHE`.
* On failure, inputs written to `.patchproof/testdata/fuzz/FuzzTarget/` are
  uploaded as CAS artifacts together with the captured output, then the saved
  corpus is replayed (`go test -run=FuzzTarget`) to confirm reproducibility.
* Timeouts, capture overflows and a missing toolchain produce `UNKNOWN`
  evidence; compile failures and invalid symbols produce `FAILED` with
  `ENGINE_UNSUPPORTED` and the compiler log as an artifact.

## Options

| option | default | meaning |
|---|---|---|
| `contract.ref` | — | contract path relative to the head snapshot root |
| `claim.id` | contract `metadata.name` | claim identifier |
| `fuzz.seconds` | 10 | fuzz duration, capped at 600 |
| `timeout.seconds` | — | step-level cap on the duration |
| `go.proxy` | `off` | module proxy for `go test` |

## Limitations

* Detects only head-side crashes/panics; no differential oracle.
* One fuzz worker; mutation quality depends on the Go fuzzing engine and the
  JSON argument codec.
* Fuzzing requires the toolchain in `PATH` to satisfy the snapshot's `go.mod`
  (`GOTOOLCHAIN=local`).
