# Test suites

| Suite | Command | Requires |
|---|---|---|
| Unit / package tests | `go test ./...` | Go toolchain |
| Race tests | `go test -race ./...` | Go toolchain |
| End-to-end local pipeline | `go test ./tests/e2e` | `git`, Go toolchain |
| Integration (PostgreSQL stack) | `PATCHPROOF_TEST_INTEGRATION=1 PATCHPROOF_TEST_POSTGRES_DSN=... go test ./tests/integration` | PostgreSQL |
| Security | `go test ./tests/security` | none |
| Chaos invariants | `go test ./tests/chaos` | none |
| Compatibility | `go test ./tests/compatibility` | none |
| Benchmarks | `go test ./benchmarks -bench . -benchmem` | Go toolchain |
| Adapter live tests | see each adapter package | S3/NATS/PostgreSQL env |

Environment-gated tests skip cleanly so the default `go test ./...` remains
hermetic. Never point live adapter variables at production infrastructure.

## What each suite protects

- **e2e** — planner → scheduler → worker → evidence → signed report → offline
  verification, using real Git and the Go toolchain.
- **integration** — the same pipeline with PostgreSQL as the authoritative
  state store, including migrations and outbox.
- **security** — protected-path policy, repository URI restrictions, CAS
  namespace/digest isolation, signed-report tamper detection, archive traversal
  and executable permissions.
- **chaos** — exactly-one lease winner, duplicate notification idempotency and
  fencing of expired attempts.
- **compatibility** — report forward compatibility, policy unknown-field
  rejection, formatting-independent policy digests, versioned event/schema
  constants and proto contract fences.
