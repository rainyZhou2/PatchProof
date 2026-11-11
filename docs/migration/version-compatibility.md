# Version compatibility

## Component versions

| Surface | Source of truth | Compatibility rule |
|---|---|---|
| Go module and toolchain | `go.mod` (`go 1.26.0`) | Build with the pinned toolchain. A newer minor needs a deliberate upgrade. |
| Public HTTP API | `api/openapi/v1/openapi.yaml` | Additive within `/v1`; unknown enum values must be tolerated. |
| Internal attempt API | `internal/api` handlers | Same deployment only; attempt tokens are short-lived and bound to generation. |
| Plugin protocol | `api/proto/patchproof/plugin/v1` | Major mismatch is incompatible; minor versions add fields and capabilities. |
| Worker protocol | `api/proto/patchproof/worker/v1` | Contract mirrors the HTTP attempt API; generated bindings are not checked in. |
| Evidence | `patchproof.evidence/v1` (`internal/domain/evidence.go`) | A new schema version gets a new verifier path; old evidence is not reinterpreted. |
| Report | `patchproof.report/v1` | Same rule as evidence. The offline verifier checks the identifier. |
| Signature statement | `patchproof.statement/v2` | v2 binds the whole report body (findings, limitations, checks, metrics, provenance). Signatures produced by v1 statements are not accepted by the v2 verifier. |
| Policy | `patchproof.io/v1` (`internal/domain/policy_spec.go`) | Unknown fields are rejected (`KnownFields(true)`); additive fields require a parser change. |
| PostgreSQL schema | `migrations/postgres` | Filename-ordered, versioned in `schema_migrations`, expand/contract. |

`api/proto` is a contract only at this stage: the server and worker use HTTP.
Advertising support for the proto does not mean generated bindings exist in this
tree; `make generate` produces them with Buf or protoc.

## Supported window

Only the latest `0.1.x` line is supported. During a rolling upgrade, the
controller and worker may differ by one minor release, provided that:

- the step spec and result fields are additive,
- the generation/fencing rules are unchanged,
- the policy and evidence parsers accept the documents produced by the other
  side,
- the database schema is in an expand phase that both versions can read.

A release that removes a field, changes a verdict's meaning or changes a digest
algorithm is a new protocol or schema version, not a minor upgrade.

## Rolling upgrade order

1. Apply the expand migration. It must be backward compatible with the running
   binaries.
2. Roll the controllers. Replicas with different shard counts must not run
   together; keep `scheduler.shardCount` identical during the rollout.
3. Roll workers and execution clusters. Old workers in flight finish under their
   existing attempt generation; new attempts use the new image digest.
4. Observe one compatibility window before removing old fields or images.
5. Apply the contract migration only after no supported binary writes the old
   shape.

## Protocol handshake

The plugin contract carries `protocol_major` and `protocol_minor`. A plugin with
a different major is rejected during capability negotiation. A higher minor is
acceptable only if all required capabilities are present; unknown capabilities
are ignored. The same rule applies if the worker proto is implemented over gRPC.

## Rollback

Rolling back the binary is allowed while the schema is in its expand phase.
Rolling back a contract migration that removed a column or changed a digest is
not a normal operation; restore from backup instead. A report generated under a
new evidence schema cannot be verified by an older verifier that does not know
the schema identifier.
