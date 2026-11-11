# CLI reference

Build the CLI with `make build`; the binary is `bin/patchproof`. It uses the same
client for local and remote verification.

## Commands

| Command | Purpose |
|---|---|
| `patchproof init` | Write a policy template (`patchproof.yaml`) to the current directory. |
| `patchproof verify` | Run the local pipeline, or submit to a control plane with `--api`. |
| `patchproof run submit` | Submit a run to a control plane. |
| `patchproof run status` | Show run and step state. |
| `patchproof run list` | List runs with cursor pagination. |
| `patchproof report` | Render a report as JSON, Markdown or SARIF. |
| `patchproof case reproduce` | Create and optionally wait for a counterexample replay. |
| `patchproof proof verify` | Verify a report offline. |
| `patchproof cluster doctor` | Check registered execution clusters. |

## Local verification

```sh
bin/patchproof init
bin/patchproof verify --repo . --base HEAD~1 --head HEAD \
  --policy patchproof.yaml --format markdown
```

Local mode is `trusted-local` only and must not process untrusted pull requests.

## Remote verification

```sh
export PATCHPROOF_API=http://localhost:8080
export PATCHPROOF_TOKEN=...

bin/patchproof run submit --project "$PROJECT_ID" \
  --repo . --base HEAD~1 --head HEAD --policy patchproof.yaml
bin/patchproof run status "$RUN_ID" --steps
bin/patchproof report "$RUN_ID" --format sarif
bin/patchproof case reproduce --run "$RUN_ID" --case "$CASE_ID" --wait
```

`run submit` sends an `Idempotency-Key`; reusing it with different frozen inputs
returns `409`.

## Offline verification

Export the report produced by a local run first:

```sh
bin/patchproof verify --repo . --base HEAD~1 --head HEAD \
  --policy patchproof.yaml --format json > report.json
```

Then verify it without the control plane:

```sh
bin/patchproof proof verify --report report.json \
  --cas-root .patchproof/cas \
  --key patchproof-local=.patchproof/keys/signing.pub \
  --policy patchproof.yaml
```

Exit codes:

| Code | Status | Meaning |
|---|---|---|
| `0` | `VALID_INTEGRITY` | Schema, signatures, manifest, evidence and policy all valid. |
| `1` | `INVALID` | Integrity or policy check failed. |
| `2` | `UNVERIFIABLE` | Missing CAS object, key or other required input. |
| `3` | usage error | Invalid flags or arguments. |

See [getting started](../getting-started.md) and the
[API guide](../api/README.md) for request/response details.
